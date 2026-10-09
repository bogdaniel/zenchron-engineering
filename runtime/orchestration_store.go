package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// CreateOrchestrationBatch writes a batch ONCE. A batch whose identity is
// already stored is not rewritten: the stored document is returned instead,
// with created=false, which is what makes resubmitting a request whose reply
// was lost find the fleet it already created.
func (s *SQLiteOperationStore) CreateOrchestrationBatch(batch orchestration.Batch) (orchestration.Batch, bool, error) {
	if err := batch.Validate(); err != nil {
		return orchestration.Batch{}, false, err
	}
	document, err := CanonicalJSON(batch)
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO orchestration_batches (id, repository, created_unix_nano, document)
		VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		batch.ID, batch.Repository, batch.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	if inserted == 1 {
		return batch, true, nil
	}
	stored, found, err := s.OrchestrationBatch(batch.ID)
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	if !found {
		return orchestration.Batch{}, false, fmt.Errorf("orchestration batch %s was neither inserted nor found", batch.ID)
	}
	return stored, false, nil
}

// OrchestrationBatch reads one batch. A stored document that does not decode
// strictly or does not match its own identity is an error, never repaired.
func (s *SQLiteOperationStore) OrchestrationBatch(id string) (orchestration.Batch, bool, error) {
	return queryOrchestrationBatch(s.db, id)
}

// queryOrchestrationBatch is the sqlExecutor-generic read
// ResolveDecisionRequest (decision_store.go) pins inside its own linearized
// transaction, exactly as the standalone OrchestrationBatch does outside one.
func queryOrchestrationBatch(q sqlExecutor, id string) (orchestration.Batch, bool, error) {
	var document string
	err := q.QueryRow(`SELECT document FROM orchestration_batches WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.Batch{}, false, nil
	}
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	batch, err := decodeOrchestrationBatch(document)
	return batch, err == nil, err
}

// OrchestrationBatches reads every batch in creation order.
//
// ponytail: reads every batch each supervisor pass; fine for an operator's
// tens of batches, add a "has unmaterialized or unadmitted items" index if
// batches ever number in the thousands.
func (s *SQLiteOperationStore) OrchestrationBatches() ([]orchestration.Batch, error) {
	rows, err := s.db.Query(`SELECT document FROM orchestration_batches ORDER BY created_unix_nano ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batches []orchestration.Batch
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		batch, err := decodeOrchestrationBatch(document)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, rows.Err()
}

func decodeOrchestrationBatch(document string) (orchestration.Batch, error) {
	var batch orchestration.Batch
	if err := strictJSON([]byte(document), &batch); err != nil {
		return orchestration.Batch{}, fmt.Errorf("stored orchestration batch is unreadable: %w", err)
	}
	if err := batch.Validate(); err != nil {
		return orchestration.Batch{}, fmt.Errorf("stored orchestration batch is corrupt: %w", err)
	}
	return batch, nil
}

// AdmitHandoff persists one admitted handoff. It is insert-only: a handoff
// already admitted under this identity is left exactly as it was, so a second
// admission pass cannot rewrite history.
func (s *SQLiteOperationStore) AdmitHandoff(handoff orchestration.EngineeringHandoff) (bool, error) {
	return s.admitHandoff(handoff, 0)
}

// AdmitCheckpointHandoff inserts only if no candidate movement followed the
// journal snapshot that proved the checkpoint binding. validatedThrough is that
// snapshot's last run-local sequence. SQLite serializes the conditional INSERT
// with AppendEvent's writes, including writers using another store/process.
// false means either the proof lost currentness or this identity already exists;
// the caller must replay before making another admission decision.
func (s *SQLiteOperationStore) AdmitCheckpointHandoff(handoff orchestration.EngineeringHandoff, validatedThrough int64) (bool, error) {
	if validatedThrough <= 0 {
		return false, errors.New("checkpoint handoff admission requires a validated journal sequence")
	}
	return s.admitHandoff(handoff, validatedThrough)
}

func (s *SQLiteOperationStore) admitHandoff(handoff orchestration.EngineeringHandoff, validatedThrough int64) (bool, error) {
	if err := handoff.Validate(); err != nil {
		return false, err
	}
	document, err := CanonicalJSON(handoff)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO orchestration_handoffs (id, batch_id, run_id, admitted_unix_nano, document)
		SELECT ?, ?, ?, ?, ?
		WHERE ? = 0 OR (
			EXISTS (SELECT 1 FROM events WHERE stream_kind = ? AND run_id = ? AND sequence = ?)
			AND NOT EXISTS (SELECT 1 FROM events WHERE stream_kind = ? AND run_id = ?
				AND sequence > ? AND type IN (?, ?, ?))
		) ON CONFLICT(id) DO NOTHING`,
		handoff.ID, handoff.BatchID, handoff.RunID, handoff.AdmittedAt.UnixNano(), string(document),
		validatedThrough, streamRun, handoff.RunID, validatedThrough,
		streamRun, handoff.RunID, validatedThrough,
		EventCandidateCommitted, EventCandidateCheckpointed, EventCandidateBaseIntegrated)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// AdmitIntegratedHandoff inserts a WorkGraph integration unit's handoff only
// if every consumed input is STILL the latest admitted handoff for its own
// run (#475 review B3): the freshness check and the insert are the SAME
// SQLite statement, not a read followed by a separate write a concurrent
// supersession could land between. "Latest for its run" is exact: a handoff
// row with a later admitted_unix_nano for that same run_id proves a newer
// one superseded it, by this store's own insert-only, append-time ordering.
func (s *SQLiteOperationStore) AdmitIntegratedHandoff(handoff orchestration.EngineeringHandoff, inputs orchestration.WorkUnitInputs) (bool, error) {
	if err := handoff.Validate(); err != nil {
		return false, err
	}
	if err := inputs.Validate(); err != nil {
		return false, err
	}
	document, err := CanonicalJSON(handoff)
	if err != nil {
		return false, err
	}
	args := []any{handoff.ID, handoff.BatchID, handoff.RunID, handoff.AdmittedAt.UnixNano(), string(document)}
	clauses := make([]string, 0, len(inputs))
	for _, input := range inputs {
		// #475 review N1: the referenced handoff must EXIST with this exact
		// run and subject (a missing id makes the scalar subquery below NULL,
		// which would otherwise let a nonexistent or wrong-subject reference
		// pass silently), and it must be the LATEST row for its run ordered
		// by (admitted_unix_nano, id) - the same tie-break
		// queryRunHandoffs's own ORDER BY already uses - not merely "nothing
		// strictly newer", which a same-nanosecond replacement would defeat.
		// #475 review N1 (third pass, #5473634194): the checks above prove H
		// is the latest ADMITTED row for its run, but admission is not the
		// only way a producer's current output can move - a new attempt, or
		// a refusal, can be JOURNALLED before any new handoff is ever
		// admitted, and from that moment inspectHandoff/WorkGraphStatus no
		// longer presents H as the run's satisfied output. The run's latest
		// event among {handoff.reported, handoff.refused} must therefore
		// still BE a report naming H's own producer identity - read from
		// '$.payload.operation_id'/'$.payload.attempt', the engineering
		// invocation HandoffReportedPayload actually carries, never the
		// outer EngineeringEvent's own top-level operation_id (that names
		// whichever operation appended the event, a repair when one
		// rewrote the report, not H's producer). A later refusal, a report
		// for a different attempt, or no report/refusal at all, all refuse.
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM orchestration_handoffs
			WHERE id = ? AND run_id = ?
			AND json_extract(document, '$.subject.candidate_revision') = ?
			AND json_extract(document, '$.subject.candidate_tree') = ?
		) AND NOT EXISTS (
			SELECT 1 FROM orchestration_handoffs AS newer
			WHERE newer.run_id = ? AND newer.id <> ?
			AND (
				newer.admitted_unix_nano > (SELECT admitted_unix_nano FROM orchestration_handoffs WHERE id = ?)
				OR (
					newer.admitted_unix_nano = (SELECT admitted_unix_nano FROM orchestration_handoffs WHERE id = ?)
					AND newer.id > ?
				)
			)
		) AND EXISTS (
			SELECT 1 FROM events AS latest
			WHERE latest.stream_kind = 'run' AND latest.run_id = ?
			AND latest.type IN (?, ?)
			AND latest.sequence = (
				SELECT MAX(sequence) FROM events
				WHERE stream_kind = 'run' AND run_id = ? AND type IN (?, ?)
			)
			AND latest.type = ?
			AND json_extract(latest.document, '$.payload.operation_id') = (SELECT json_extract(document, '$.producer.operation_id') FROM orchestration_handoffs WHERE id = ?)
			AND json_extract(latest.document, '$.payload.attempt') = (SELECT json_extract(document, '$.producer.attempt') FROM orchestration_handoffs WHERE id = ?)
		)`)
		args = append(args,
			input.HandoffID, input.RunID, input.CandidateRevision, input.CandidateTree,
			input.RunID, input.HandoffID, input.HandoffID, input.HandoffID, input.HandoffID,
			input.RunID, EventHandoffReported, EventHandoffRefused,
			input.RunID, EventHandoffReported, EventHandoffRefused,
			EventHandoffReported, input.HandoffID, input.HandoffID,
		)
	}
	where := "1=1"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}
	result, err := s.db.Exec(`INSERT INTO orchestration_handoffs (id, batch_id, run_id, admitted_unix_nano, document)
		SELECT ?, ?, ?, ?, ?
		WHERE `+where+`
		ON CONFLICT(id) DO NOTHING`, args...)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// RunHandoffs reads every handoff admitted for one run, oldest first.
func (s *SQLiteOperationStore) RunHandoffs(runID string) ([]orchestration.EngineeringHandoff, error) {
	return queryRunHandoffs(s.db, runID)
}

func queryRunHandoffs(q eventQuerier, runID string) ([]orchestration.EngineeringHandoff, error) {
	rows, err := q.Query(`SELECT document FROM orchestration_handoffs WHERE run_id = ? ORDER BY admitted_unix_nano ASC, id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var handoffs []orchestration.EngineeringHandoff
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var handoff orchestration.EngineeringHandoff
		if err := strictJSON([]byte(document), &handoff); err != nil {
			return nil, fmt.Errorf("stored handoff is unreadable: %w", err)
		}
		if err := handoff.Validate(); err != nil {
			return nil, fmt.Errorf("stored handoff is corrupt: %w", err)
		}
		handoffs = append(handoffs, handoff)
	}
	return handoffs, rows.Err()
}

// RefuseHandoff durably records that the handoff with this identity will never
// be admitted, and why. It is insert-only: the first decision stands.
func (s *SQLiteOperationStore) RefuseHandoff(id, batchID, runID, reason string, at time.Time) error {
	if id == "" || batchID == "" || runID == "" || strings.TrimSpace(reason) == "" {
		return errors.New("a handoff refusal needs its identity, batch, run and reason")
	}
	_, err := s.db.Exec(`INSERT INTO orchestration_handoff_refusals (id, batch_id, run_id, refused_unix_nano, reason)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, batchID, runID, at.UnixNano(), boundedDetail(reason))
	return err
}

// RunHandoffRefusals maps every refused handoff identity of one run to its
// recorded reason.
func (s *SQLiteOperationStore) RunHandoffRefusals(runID string) (map[string]string, error) {
	return queryRunHandoffRefusals(s.db, runID)
}

func queryRunHandoffRefusals(q eventQuerier, runID string) (map[string]string, error) {
	rows, err := q.Query(`SELECT id, reason FROM orchestration_handoff_refusals WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refused := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		refused[id] = reason
	}
	return refused, rows.Err()
}
