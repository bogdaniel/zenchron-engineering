package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var errVerificationNotAcquired = errors.New("verification transition was not acquired")

// verificationCountSQL is the same resource count for assurance
// acquisition, nested tool acquisition, and an independent-review trigger's
// claim (#474 R5) - the one shared formula every verification-capacity
// consumer is counted by and checked against. Neither a permit nor a
// review claim drops its held slot on clock expiry; both are reclaimed
// explicitly (reclaimVerificationPermits, reclaimReviewVerificationClaims),
// never by this count alone.
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
		(SELECT COUNT(*) FROM verification_permits WHERE json_extract(document, '$.state') = 'granted') +
		(SELECT COUNT(*) FROM review_verification_claims)`, args
}

// reviewClaimRunCountSQL is the distinct-run-id count of open review
// verification claims (#474 R8): an independent review is read-only but
// still provider WORK, which #85 classifies as bounded by BOTH
// max_concurrent_verifications (verificationCountSQL already covers this)
// AND max_concurrent_runs - a review claim carries no run_operations row
// of its own for the ordinary work-ceiling count to see, so this is added
// wherever that count is. excludeRunID is the SAME run a caller already
// excludes from its own ordinary-operations count, applied here too so an
// operation never double-counts the review claim it is itself about to
// hold (or just released).
func reviewClaimRunCountSQL(excludeRunID string) (string, []any) {
	return `(SELECT COUNT(DISTINCT json_extract(document, '$.run_id')) FROM review_verification_claims
		WHERE json_extract(document, '$.run_id') IS NOT NULL AND json_extract(document, '$.run_id') <> ?)`,
		[]any{excludeRunID}
}

// workCountSQL is the distinct-run-id WORK-capacity-class occupancy count
// (#85): every ordinary leased/running operation whose kind is NOT an
// observation kind, plus every open review verification claim's own run
// (#474 R8) - ClaimReviewVerificationSlot's own ceiling check, since a
// review claim is never observation-class work.
func workCountSQL(excludeRunID string) (string, []any) {
	kinds := observationKindList()
	args := make([]any, 0, len(kinds)+1)
	args = append(args, excludeRunID)
	for _, kind := range kinds {
		args = append(args, kind)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	claimSQL, claimArgs := reviewClaimRunCountSQL(excludeRunID)
	args = append(args, claimArgs...)
	return `(SELECT COUNT(DISTINCT run_id) FROM run_operations
		WHERE run_id <> ? AND json_extract(document, '$.state') IN ('leased','running')
		AND json_extract(document, '$.lease') IS NOT NULL
		AND COALESCE(json_extract(document, '$.kind') IN (` + placeholders + `), 0) = 0) +
		` + claimSQL, args
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
