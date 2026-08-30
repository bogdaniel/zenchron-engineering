package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// The journal lives in the same runtime.db as run_operations and advances the
// same PRAGMA user_version migration list. There is one runtime database, one
// migration mechanism, and one reducer.
const (
	sqliteRunColumns   = `id, repository, base_id, base_revision, contract_id, contract_revision, candidate_branch, candidate_revision, candidate_tree, controller_sha256, created_unix_nano, document`
	sqliteEventColumns = `id, run_id, sequence, type, operation_id, previous_event_id, previous_event_hash, state_before, state_after, event_hash, document`
)

// PutRun persists run identity together with its exact subject and contract
// binding. The durable document is canonical (RFC 8785) so equal run state is
// byte-equal. The journal cursor is deliberately not a column: replay derives
// it from the events, so it can never disagree with them.
func (s *SQLiteOperationStore) PutRun(run EngineeringRun) error {
	if run.ID == "" {
		return fmt.Errorf("run id is required")
	}
	document, err := CanonicalJSON(run)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO runs (`+sqliteRunColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository = excluded.repository,
			base_id = excluded.base_id, base_revision = excluded.base_revision,
			contract_id = excluded.contract_id, contract_revision = excluded.contract_revision,
			candidate_branch = excluded.candidate_branch, candidate_revision = excluded.candidate_revision,
			candidate_tree = excluded.candidate_tree,
			controller_sha256 = excluded.controller_sha256,
			document = excluded.document`,
		run.ID, run.Repository, run.Base.ID, run.Base.Revision, run.Contract.ID, run.Contract.Revision,
		run.Candidate.Branch, run.Candidate.Revision, run.Candidate.Tree, run.ControllerSHA256,
		run.CreatedAt.UnixNano(), string(document))
	return err
}

// Run returns the persisted run identity.
func (s *SQLiteOperationStore) Run(id string) (EngineeringRun, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM runs WHERE id = ?`, id).Scan(&document)
	if err == sql.ErrNoRows {
		return EngineeringRun{}, false, nil
	}
	if err != nil {
		return EngineeringRun{}, false, err
	}
	run, err := decodeRun(document)
	return run, err == nil, err
}

// AppendEvent allocates the run's next sequence, links the hash chain, records
// the state-before/state-after digests, and inserts the row in one transaction.
// The caller never chooses a sequence or a chain link: allocation happens under
// the transaction's write lock, so two processes cannot race one another into a
// gap, and UNIQUE(run_id, sequence) refuses a duplicate even if one tried.
//
// Validation is the typed payload registry plus Reduce: a reserved or unknown
// event type, a payload that does not match its type's schema, a canonical
// payload above the byte ceiling, or an unpublishable artifact fails the append
// before any row is written.
//
// ponytail: each append reduces the whole run twice (before/after state), which
// is O(n) per event. A cached snapshot per run is the upgrade if a journal ever
// grows past a few thousand events.
func (s *SQLiteOperationStore) AppendEvent(e EngineeringEvent) (EngineeringEvent, error) {
	if e.ID == "" || e.RunID == "" {
		return EngineeringEvent{}, fmt.Errorf("event id and run id are required")
	}
	if e.Sequence != 0 || e.PreviousEventID != "" || e.PreviousEventHash != "" || e.StateBefore != "" || e.StateAfter != "" || e.EventHash != "" {
		return EngineeringEvent{}, fmt.Errorf("sequence, chain, and state hashes are allocated by the journal, not the caller")
	}
	if err := validateEventPayload(e); err != nil {
		return EngineeringEvent{}, err
	}
	if err := rejectEmbeddedTranscript(e); err != nil {
		return EngineeringEvent{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EngineeringEvent{}, err
	}
	defer tx.Rollback()
	var document string
	switch err := tx.QueryRow(`SELECT document FROM runs WHERE id = ?`, e.RunID).Scan(&document); err {
	case nil:
	case sql.ErrNoRows:
		return EngineeringEvent{}, fmt.Errorf("unknown run %q", e.RunID)
	default:
		return EngineeringEvent{}, err
	}
	run, err := decodeRun(document)
	if err != nil {
		return EngineeringEvent{}, err
	}
	existing, err := queryEvents(tx, e.RunID)
	if err != nil {
		return EngineeringEvent{}, err
	}
	before, err := Reduce(run, existing)
	if err != nil {
		return EngineeringEvent{}, err
	}
	e.Sequence = int64(len(existing)) + 1
	if n := len(existing); n > 0 {
		e.PreviousEventID = existing[n-1].ID
		e.PreviousEventHash = existing[n-1].EventHash
	}
	e.StateBefore = before.StateSHA256
	// StateDigest excludes the journal cursor and state_sha256, so an event's
	// state_after never feeds back into the digest it records: the last event's
	// state_after equals the replayed snapshot's state_sha256.
	after, err := Reduce(run, append(existing, e))
	if err != nil {
		return EngineeringEvent{}, err
	}
	e.StateAfter = after.StateSHA256
	if e.EventHash, err = EventDigest(e); err != nil {
		return EngineeringEvent{}, err
	}
	canonical, err := CanonicalJSON(e)
	if err != nil {
		return EngineeringEvent{}, err
	}
	if _, err := tx.Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.RunID, e.Sequence, e.Type, e.OperationID, e.PreviousEventID, e.PreviousEventHash,
		e.StateBefore, e.StateAfter, e.EventHash, string(canonical)); err != nil {
		return EngineeringEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return EngineeringEvent{}, err
	}
	return e, nil
}

// Events returns the run's persisted events in sequence order. Every indexed
// column must still agree with its canonical document, so tampering with either
// side is refused rather than returned; the hash chain itself is verified by
// Reduce, which Replay applies.
func (s *SQLiteOperationStore) Events(runID string) ([]EngineeringEvent, error) {
	return queryEvents(s.db, runID)
}

// Replay rebuilds run state by feeding the persisted events back through the
// one reducer. There is no second reducer.
func (s *SQLiteOperationStore) Replay(runID string) (RunSnapshot, error) {
	run, ok, err := s.Run(runID)
	if err != nil {
		return RunSnapshot{}, err
	}
	if !ok {
		return RunSnapshot{}, fmt.Errorf("unknown run %q", runID)
	}
	events, err := s.Events(runID)
	if err != nil {
		return RunSnapshot{}, err
	}
	return Reduce(run, events)
}

func queryEvents(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, runID string) ([]EngineeringEvent, error) {
	rows, err := q.Query(`SELECT `+sqliteEventColumns+` FROM events WHERE run_id = ? ORDER BY sequence ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EngineeringEvent{}
	for rows.Next() {
		var row EngineeringEvent
		var document string
		if err := rows.Scan(&row.ID, &row.RunID, &row.Sequence, &row.Type, &row.OperationID,
			&row.PreviousEventID, &row.PreviousEventHash, &row.StateBefore, &row.StateAfter,
			&row.EventHash, &document); err != nil {
			return nil, err
		}
		var e EngineeringEvent
		if err := json.Unmarshal([]byte(document), &e); err != nil {
			return nil, fmt.Errorf("decode durable event: %w", err)
		}
		if e.ID != row.ID || e.RunID != row.RunID || e.Sequence != row.Sequence || e.Type != row.Type ||
			e.OperationID != row.OperationID || e.PreviousEventID != row.PreviousEventID ||
			e.PreviousEventHash != row.PreviousEventHash || e.StateBefore != row.StateBefore ||
			e.StateAfter != row.StateAfter || e.EventHash != row.EventHash {
			return nil, fmt.Errorf("durable event %q columns disagree with its document", row.ID)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// transcriptFields name raw provider and verifier output. ArtifactStore keeps
// those bodies in local-only files and the journal records only the Artifact
// reference, which has no body field at all, so an event that inlines one in
// its payload is refused before it can reach a canonical row. Raw artifact
// references are separately constrained by ValidateArtifact, which Reduce
// already applies during the append.
//
// This is defense in depth behind validateEventPayload, which is the actual
// proof: closed per-type schemas reject an unexpected field whatever it is
// named, and maxCanonicalPayloadBytes bounds the fields that are expected. The
// deny-list survives because it costs nothing and catches a raw body smuggled
// through a payload type that is later widened.
var transcriptFields = map[string]bool{
	"transcript": true, "transcript_body": true, "raw_transcript": true,
	"stdout": true, "stderr": true, "raw_output": true,
}

func rejectEmbeddedTranscript(e EngineeringEvent) error {
	if len(e.Payload) == 0 {
		return nil
	}
	var payload any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return fmt.Errorf("invalid event payload: %w", err)
	}
	return walkPayload(payload)
}

func walkPayload(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for name, value := range t {
			if transcriptFields[strings.ToLower(name)] {
				return fmt.Errorf("event payload embeds a raw transcript body in %q; record an artifact reference instead", name)
			}
			if err := walkPayload(value); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range t {
			if err := walkPayload(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeRun(document string) (EngineeringRun, error) {
	var run EngineeringRun
	if err := json.Unmarshal([]byte(document), &run); err != nil {
		return EngineeringRun{}, fmt.Errorf("decode durable run: %w", err)
	}
	return run, nil
}
