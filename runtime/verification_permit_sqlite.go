package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var errVerificationNotAcquired = errors.New("verification transition was not acquired")

// verificationCountSQL is the same resource count for assurance acquisition
// and nested tool acquisition. Neither drops a held grant on clock expiry.
func verificationCountSQL() (string, []any) {
	args := make([]any, 0, len(verificationKinds))
	for _, kind := range verificationKinds {
		args = append(args, kind)
	}
	kinds := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	return `(SELECT COUNT(DISTINCT run_id) FROM run_operations
		WHERE json_extract(document, '$.state') IN ('leased','running')
		AND json_extract(document, '$.lease') IS NOT NULL
		AND json_extract(document, '$.kind') IN (` + kinds + `)) +
		(SELECT COUNT(*) FROM verification_permits WHERE json_extract(document, '$.state') = 'granted')`, args
}

func (s *SQLiteOperationStore) VerificationPermit(id string) (VerificationPermit, int64, bool, error) {
	var document string
	var revision int64
	err := s.db.QueryRow(`SELECT document, revision FROM verification_permits WHERE id = ?`, id).Scan(&document, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationPermit{}, 0, false, nil
	}
	if err != nil {
		return VerificationPermit{}, 0, false, err
	}
	p, err := decodeVerificationPermit(document)
	if err == nil && p.ID != id {
		err = errors.New("verification permit row identity mismatch")
	}
	return p, revision, err == nil, err
}

func (s *SQLiteOperationStore) VerificationPermits() ([]VerificationPermit, error) {
	rows, err := s.db.Query(`SELECT id, document FROM verification_permits
		WHERE COALESCE(json_extract(document, '$.state'), '') <> 'released' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanVerificationPermits(rows)
}

func scanVerificationPermits(rows *sql.Rows) ([]VerificationPermit, error) {
	defer rows.Close()
	var permits []VerificationPermit
	for rows.Next() {
		var id, document string
		if err := rows.Scan(&id, &document); err != nil {
			return nil, err
		}
		p, err := decodeVerificationPermit(document)
		if err != nil {
			return nil, err
		}
		if p.ID != id {
			return nil, errors.New("verification permit row identity mismatch")
		}
		permits = append(permits, p)
	}
	return permits, rows.Err()
}

func decodeVerificationPermit(document string) (VerificationPermit, error) {
	var p VerificationPermit
	if err := decodeJSON([]byte(document), &p); err != nil {
		return p, err
	}
	return p, p.validate()
}

// Parent authority is checked in the same transaction as the grant, against
// the physical attempt and lease owner, never against a client snapshot.
const verificationParentSQL = `EXISTS (SELECT 1 FROM run_operations AS parent
	WHERE parent.id = ? AND parent.run_id = ?
	AND json_extract(parent.document, '$.state') = 'running'
	AND json_extract(parent.document, '$.lease.owner') = ?
	AND json_extract(parent.document, '$.attempt_identity') = ?
	AND COALESCE(json_extract(parent.document, '$.cancel_requested'), 0) = 0)
	AND NOT EXISTS (SELECT 1 FROM runs WHERE id = ?
	AND json_extract(document, '$.disposition') IN ('completed','failed','cancelled'))`

func (s *SQLiteOperationStore) TransitionVerificationPermit(p VerificationPermit, expected int64, ceiling int) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	if expected < 0 || (expected == 0) != (p.State == VerificationWaiting) || ceiling < 1 {
		return false, errors.New("invalid verification transition")
	}
	document, err := CanonicalJSON(p)
	if err != nil {
		return false, err
	}
	args := []any{string(document), p.ID, expected}
	query := `UPDATE verification_permits SET document = ?, revision = revision + 1 WHERE id = ? AND revision = ?`
	if expected == 0 {
		query = `INSERT INTO verification_permits (document, id, revision) SELECT ?, ?, 1 WHERE ? = 0
		AND NOT EXISTS (SELECT 1 FROM verification_permits WHERE id = ?)`
		args = append(args, p.ID)
	} else {
		query += ` AND json_extract(document, '$.tool_owner') = ?
		AND json_extract(document, '$.controller_owner') = ?
		AND json_extract(document, '$.parent.OperationID') = ?
		AND json_extract(document, '$.parent.RunID') = ?
		AND json_extract(document, '$.parent.Attempt') = ?`
		args = append(args, p.ToolOwner, p.ControllerOwner, p.Parent.OperationID, p.Parent.RunID, p.Parent.Attempt)
		query += ` AND COALESCE(json_extract(document, '$.tool_lock_dir'), '') = ?
		AND json_extract(document, '$.sandbox') IS json_extract(?, '$.sandbox')`
		args = append(args, p.ToolLockDir, string(document))
	}
	if p.State != VerificationReleased {
		query += ` AND ` + verificationParentSQL
		args = append(args, p.Parent.OperationID, p.Parent.RunID, p.ControllerOwner, p.Parent.Attempt, p.Parent.RunID)
		// A nested grant completes an already-leased operation. Pause blocks
		// new operation leases, and deliberately lets that operation settle.
	}
	if p.State == VerificationGranted {
		count, capacityArgs := verificationCountSQL()
		query += ` AND json_extract(document, '$.state') = 'waiting'
		AND (` + count + `) < ?`
		args = append(args, capacityArgs...)
		args = append(args, ceiling)
	}
	at := p.RequestedAt
	if p.GrantedAt != nil {
		at = *p.GrantedAt
	}
	if p.ReleasedAt != nil {
		at = *p.ReleasedAt
	}
	event := EngineeringEvent{SchemaVersion: SchemaVersion, ID: p.ID + "/" + string(p.State), RunID: p.Parent.RunID,
		OperationID: p.Parent.OperationID, Type: EventVerificationPermitChanged, OccurredAt: at, Payload: document}
	_, err = s.appendEventWithMutation(event, func(tx *sql.Tx) error {
		result, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errVerificationNotAcquired
		}
		return nil
	})
	if errors.Is(err, errVerificationNotAcquired) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("persist verification permit: %w", err)
	}
	return true, nil
}
