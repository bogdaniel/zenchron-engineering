package runtime

// #474 R5: an independent-review trigger invokes a full reviewer provider -
// the same verification-capacity weight as an ordinary assurance operation
// or a nested VerificationPermit - but is bound to no run's own operation
// row, so without this it had no way to participate in the SAME
// MaxConcurrentVerifications ceiling the scheduler already enforces
// durably (#490). ReviewVerificationClaim is the durable, restart-visible
// fact that one trigger invocation currently holds that slot, counted
// additively into the one shared formula (verificationCountSQL) every
// other verification-capacity consumer is counted and checked by. This is
// not a second scheduler and not a second ceiling: it is the existing one,
// with a third consumer.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"
)

// ReviewVerificationClaim is the durable fact behind one held slot.
// RunID is the producer run the review is FOR (#474 R8): a review claim
// carries no run_operations row of its own, so this is what the shared
// work-ceiling count (workCountSQL/reviewClaimRunCountSQL) attributes it
// to - an independent review is read-only but still provider work,
// bounded by max_concurrent_runs exactly as any other provider invocation
// already is.
type ReviewVerificationClaim struct {
	ID              string    `json:"id"`
	ControllerOwner string    `json:"controller_owner"`
	RunID           string    `json:"run_id"`
	RequestedAt     time.Time `json:"requested_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func (c ReviewVerificationClaim) validate() error {
	if c.ID == "" || c.ControllerOwner == "" || c.RunID == "" || c.RequestedAt.IsZero() || c.ExpiresAt.Before(c.RequestedAt) {
		return errors.New("incomplete review verification claim")
	}
	return nil
}

// reviewVerificationClaimTTL bounds how long an abandoned claim - its
// owning controller died mid-review - may hold a slot before
// reclaimReviewVerificationClaims may release it. The same bounded-lease
// shape VerificationPermit's own ExpiresAt already uses.
const reviewVerificationClaimTTL = 15 * time.Minute

// ReviewVerificationClaimStore is the scheduler's durable I/O boundary for
// review-trigger claims, mirroring VerificationPermitStore's own shape.
type ReviewVerificationClaimStore interface {
	ReviewVerificationClaims() ([]ReviewVerificationClaim, error)
	ClaimReviewVerificationSlot(claim ReviewVerificationClaim, maxRuns, maxVerifications int) (bool, error)
	ReleaseReviewVerificationClaim(id string) error
}

func (s *SQLiteOperationStore) ReviewVerificationClaims() ([]ReviewVerificationClaim, error) {
	rows, err := s.db.Query(`SELECT document FROM review_verification_claims`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ReviewVerificationClaim
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var claim ReviewVerificationClaim
		if err := json.Unmarshal([]byte(document), &claim); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

// ClaimReviewVerificationSlot inserts claim in the SAME statement that
// checks it keeps BOTH shared ceilings - verificationCountSQL AND
// workCountSQL (#474 R8: a review is read-only but still provider work,
// bounded by max_concurrent_runs too, never only max_concurrent_verifications)
// - at or under their respective bounds. One atomic write, never a
// separate read-then-write race between two controllers.
func (s *SQLiteOperationStore) ClaimReviewVerificationSlot(claim ReviewVerificationClaim, maxRuns, maxVerifications int) (bool, error) {
	if err := claim.validate(); err != nil {
		return false, err
	}
	if maxRuns < 1 || maxVerifications < 1 {
		return false, errors.New("a work or verification ceiling below one claims nothing")
	}
	document, err := CanonicalJSON(claim)
	if err != nil {
		return false, err
	}
	verificationSQL, verificationArgs := verificationCountSQL()
	workSQL, workArgs := workCountSQL(claim.RunID)
	execArgs := append([]any{claim.ID, string(document)}, verificationArgs...)
	execArgs = append(execArgs, maxVerifications)
	execArgs = append(execArgs, workArgs...)
	execArgs = append(execArgs, maxRuns)
	result, err := s.db.Exec(`INSERT INTO review_verification_claims (id, revision, document)
		SELECT ?, 1, ? WHERE (`+verificationSQL+`) < ? AND (`+workSQL+`) < ?`, execArgs...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *SQLiteOperationStore) ReleaseReviewVerificationClaim(id string) error {
	_, err := s.db.Exec(`DELETE FROM review_verification_claims WHERE id = ?`, id)
	return err
}

// claimReviewVerificationSlot reclaims abandoned claims first - the same
// ordering AcquireVerification already uses - then releases any claim THIS
// SAME (owner, runID) pair already holds (#474 R9) before attempting a
// fresh one. A review claim is never held across two separate calls here
// by design (claim, do the review, release, every single call) - an
// existing one found at entry is always a LEAK from an earlier call whose
// own release failed, owned by a controller that is, by definition, alive
// right now (it is making this call). reclaimReviewVerificationClaims's
// "dead AND expired" rule can never free that: only this controller
// retrying its own release can, which is what makes a failed release
// idempotently retryable rather than a permanent capacity loss.
//
// ok is false exactly when either ceiling is genuinely held by someone
// else: never queued, never retried within this call, exactly as
// idempotent as every other call into ReconcileReviewRemediation.
func (s Scheduler) claimReviewVerificationSlot(owner, runID string) (ReviewVerificationClaim, bool, error) {
	s = s.defaults()
	store, ok := s.Store.(ReviewVerificationClaimStore)
	if !ok {
		return ReviewVerificationClaim{}, false, errors.New("operation store cannot enforce review verification capacity")
	}
	if err := s.reclaimReviewVerificationClaims(); err != nil {
		return ReviewVerificationClaim{}, false, err
	}
	existing, err := store.ReviewVerificationClaims()
	if err != nil {
		return ReviewVerificationClaim{}, false, err
	}
	for _, leaked := range existing {
		if leaked.ControllerOwner != owner || leaked.RunID != runID {
			continue
		}
		if err := store.ReleaseReviewVerificationClaim(leaked.ID); err != nil {
			return ReviewVerificationClaim{}, false, err
		}
	}
	now := s.Clock.Now()
	claim := ReviewVerificationClaim{
		ID: "review-verification-" + rand.Text(), ControllerOwner: owner, RunID: runID,
		RequestedAt: now, ExpiresAt: now.Add(reviewVerificationClaimTTL),
	}
	claimed, err := store.ClaimReviewVerificationSlot(claim, s.MaxConcurrentRuns, s.MaxConcurrentVerifications)
	if err != nil || !claimed {
		return ReviewVerificationClaim{}, false, err
	}
	return claim, true, nil
}

// releaseReviewVerificationSlot releases claim. A caller that never
// successfully claimed (ok was false) never calls this.
func (s Scheduler) releaseReviewVerificationSlot(id string) error {
	s = s.defaults()
	store, ok := s.Store.(ReviewVerificationClaimStore)
	if !ok {
		return errors.New("operation store cannot enforce review verification capacity")
	}
	return store.ReleaseReviewVerificationClaim(id)
}

// reclaimReviewVerificationClaims releases every claim whose owning
// controller is both expired AND dead - the same "death and expiry are
// both required" rule reclaimVerificationPermit already applies, so a
// claim outliving its own controller's crash is reclaimed exactly when a
// lease or a nested permit in the same state would be, never sooner.
func (s Scheduler) reclaimReviewVerificationClaims() error {
	s = s.defaults()
	store, ok := s.Store.(ReviewVerificationClaimStore)
	if !ok {
		return nil
	}
	claims, err := store.ReviewVerificationClaims()
	if err != nil {
		return err
	}
	now := s.Clock.Now()
	for _, claim := range claims {
		if now.Before(claim.ExpiresAt) || s.Liveness.Alive(claim.ControllerOwner) {
			continue
		}
		if err := store.ReleaseReviewVerificationClaim(claim.ID); err != nil {
			return err
		}
	}
	return nil
}
