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

// reviewVerificationClaimTTL is the MINIMUM bound on how long an abandoned
// claim - its owning controller died mid-review - may hold a slot before
// reclaimReviewVerificationClaims may release it. The same bounded-lease
// shape VerificationPermit's own ExpiresAt already uses.
//
// #474 R11: a FIXED 15 minutes shorter than the reviewer's own actual
// permitted wall-clock budget (ReviewBudget().WallLimit, operator-
// configurable past this) could expire while a genuinely still-running,
// still-alive review had not yet reached its own deadline - "expired"
// would then mean only "15 minutes passed," never "the reviewer could not
// still legitimately be running." claimReviewVerificationSlot now takes
// the caller's actual review TTL and uses whichever is LARGER, so a claim
// can only ever expire once the review's own enforced budget guarantees it
// is no longer legitimately in flight.
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
// ordering AcquireVerification already uses - then releases any EXPIRED
// claim THIS SAME (owner, runID) pair already holds (#474 R9) before
// attempting a fresh one.
//
// #474 R11: expiry, not merely the (owner, runID) match, is what makes this
// safe. Owner is a single process instance's lifetime identity
// (host/pid/start-token, NewRuntimeOwner): two DIFFERENT processes can
// never collide on it, and ReconcileReviewRemediationForRun's only caller
// (driveOne) never drives the same run twice concurrently within one
// process (Supervisor.admit's inflight set) - so an UNEXPIRED claim for
// this EXACT (owner, runID) pair can never be a live concurrent sibling
// under the wiring this scheduler is actually used with today. But
// claimReviewVerificationSlot is a reusable primitive, not entitled to
// assume a caller it cannot see will always honor that: requiring expiry
// first means that even if some future or misbehaving caller DID invoke it
// twice for the same pair while the first genuinely still held the slot,
// this call could still never destroy that still-active hold - only a
// claim already past its own bounded TTL, exactly like any other expiring
// lease in this system, is ever released here. A release that failed
// leaves its claim recoverable within at most one TTL (bounded, never
// permanent); a release that is still correctly held stays held.
//
// ttl is the caller's own actual review wall-clock budget (#474 R11): the
// claim's real expiry is whichever is LARGER of ttl and
// reviewVerificationClaimTTL, so a short or zero-valued ttl can never
// produce a claim that expires before the system's own minimum bound,
// and a longer operator-configured review budget can never be undercut
// by a shorter fixed constant.
//
// ok is false exactly when either ceiling is genuinely held by someone
// else: never queued, never retried within this call, exactly as
// idempotent as every other call into ReconcileReviewRemediation.
func (s Scheduler) claimReviewVerificationSlot(owner, runID string, ttl time.Duration) (ReviewVerificationClaim, bool, error) {
	s = s.defaults()
	store, ok := s.Store.(ReviewVerificationClaimStore)
	if !ok {
		return ReviewVerificationClaim{}, false, errors.New("operation store cannot enforce review verification capacity")
	}
	if ttl < reviewVerificationClaimTTL {
		ttl = reviewVerificationClaimTTL
	}
	if err := s.reclaimReviewVerificationClaims(); err != nil {
		return ReviewVerificationClaim{}, false, err
	}
	now := s.Clock.Now()
	existing, err := store.ReviewVerificationClaims()
	if err != nil {
		return ReviewVerificationClaim{}, false, err
	}
	for _, leaked := range existing {
		if leaked.ControllerOwner != owner || leaked.RunID != runID || now.Before(leaked.ExpiresAt) {
			continue
		}
		if err := store.ReleaseReviewVerificationClaim(leaked.ID); err != nil {
			return ReviewVerificationClaim{}, false, err
		}
	}
	claim := ReviewVerificationClaim{
		ID: "review-verification-" + rand.Text(), ControllerOwner: owner, RunID: runID,
		RequestedAt: now, ExpiresAt: now.Add(ttl),
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
