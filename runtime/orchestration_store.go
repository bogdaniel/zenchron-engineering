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
	var document string
	err := s.db.QueryRow(`SELECT document FROM orchestration_batches WHERE id = ?`, id).Scan(&document)
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
	if err := handoff.Validate(); err != nil {
		return false, err
	}
	document, err := CanonicalJSON(handoff)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO orchestration_handoffs (id, batch_id, run_id, admitted_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		handoff.ID, handoff.BatchID, handoff.RunID, handoff.AdmittedAt.UnixNano(), string(document))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// RunHandoffs reads every handoff admitted for one run, oldest first.
func (s *SQLiteOperationStore) RunHandoffs(runID string) ([]orchestration.EngineeringHandoff, error) {
	rows, err := s.db.Query(`SELECT document FROM orchestration_handoffs WHERE run_id = ? ORDER BY admitted_unix_nano ASC, id ASC`, runID)
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
	rows, err := s.db.Query(`SELECT id, reason FROM orchestration_handoff_refusals WHERE run_id = ?`, runID)
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
