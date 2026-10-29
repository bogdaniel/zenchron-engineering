package runtime

// #474 R5/R8/R9: an independent-review trigger's durable claim, an
// ordinary assurance/verification operation, and an ordinary WORK
// operation all share the SAME two authoritative capacity ceilings -
// never uncoordinated counters. These tests exercise that directly
// against the real scheduler/store, the same verificationScheduler/
// planKind/mustNext fixtures #490's own capacity tests use.

import (
	"testing"
	"time"
)

// TestAssuranceOperationBlocksAReviewVerificationClaim is R5's first
// required direction: one durable verification-class operation occupying
// the only slot must prevent a supervisor reviewer from starting.
func TestAssuranceOperationBlocksAReviewVerificationClaim(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	s := verificationScheduler(store, "controller-a", 10, 1)
	planKind(t, s, "run-1", OpAssuranceGo)
	if leased := mustNext(t, s, "run-1"); leased == nil {
		t.Fatal("expected the assurance operation to acquire the only verification slot")
	}

	claim, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatalf("expected the review claim to be refused while assurance holds the only verification slot: %+v", claim)
	}
}

// TestReviewVerificationClaimBlocksAnAssuranceOperation is R5's other
// required direction: a reviewer's durable claim occupying the only slot
// must prevent a NEW assurance/verification operation from being acquired,
// and releasing it must free the slot for an ordinary operation again.
func TestReviewVerificationClaimBlocksAnAssuranceOperation(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	s := verificationScheduler(store, "controller-a", 10, 1)
	claim, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-2", 0)
	if err != nil || !claimed {
		t.Fatalf("claiming the only verification slot: claimed=%v err=%v", claimed, err)
	}

	planKind(t, s, "run-1", OpAssuranceGo)
	if leased := mustNext(t, s, "run-1"); leased != nil {
		t.Fatal("expected the assurance operation to be refused while the review claim holds the only verification slot")
	}

	if err := s.releaseReviewVerificationSlot(claim.ID); err != nil {
		t.Fatal(err)
	}
	if leased := mustNext(t, s, "run-1"); leased == nil {
		t.Fatal("expected the assurance operation to acquire the slot once the review claim released it")
	}
}

// TestAnOrdinaryWorkOperationBlocksAReviewVerificationClaim is R8's first
// required direction: an independent review is read-only but still
// provider WORK (#85), bounded by max_concurrent_runs too - never only by
// max_concurrent_verifications. A ceiling of 1 work slot, already held by
// an ordinary execution.invoke, must refuse a review claim for a
// DIFFERENT run even though verification capacity alone is wide open.
func TestAnOrdinaryWorkOperationBlocksAReviewVerificationClaim(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	s := verificationScheduler(store, "controller-a", 1, 10)
	planKind(t, s, "run-1", OpExecutionInvoke)
	if leased := mustNext(t, s, "run-1"); leased == nil {
		t.Fatal("expected the ordinary work operation to acquire the only work slot")
	}

	if _, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-2", 0); err != nil {
		t.Fatal(err)
	} else if claimed {
		t.Fatal("expected the review claim to be refused while an ordinary work operation holds the only work slot")
	}
}

// TestAReviewVerificationClaimBlocksAnOrdinaryWorkOperation is R8's other
// required direction: a review claim holding the only work slot must
// refuse a NEW, UNRELATED run's ordinary work operation, and releasing it
// must free the slot again.
func TestAReviewVerificationClaimBlocksAnOrdinaryWorkOperation(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	s := verificationScheduler(store, "controller-a", 1, 10)
	claim, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0)
	if err != nil || !claimed {
		t.Fatalf("claiming the only work slot for a review: claimed=%v err=%v", claimed, err)
	}

	planKind(t, s, "run-2", OpExecutionInvoke)
	if leased := mustNext(t, s, "run-2"); leased != nil {
		t.Fatal("expected the ordinary work operation to be refused while the review claim holds the only work slot")
	}

	if err := s.releaseReviewVerificationSlot(claim.ID); err != nil {
		t.Fatal(err)
	}
	if leased := mustNext(t, s, "run-2"); leased == nil {
		t.Fatal("expected the work operation to acquire the slot once the review claim released it")
	}
}

// TestReviewVerificationClaimIsVisibleAcrossControllers is R5's explicit
// restart/controller-succession requirement: the claim is durable store
// state, not a Supervisor-local semaphore, so a SECOND scheduler instance
// (simulating a successor controller, or the same controller restarted)
// sharing the same store sees the SAME occupancy, never a fresh, empty
// count.
func TestReviewVerificationClaimIsVisibleAcrossControllers(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	first := verificationScheduler(store, "controller-a", 10, 1)
	if _, claimed, err := first.claimReviewVerificationSlot("controller-a", "run-1", 0); err != nil || !claimed {
		t.Fatalf("first controller claiming the only slot: claimed=%v err=%v", claimed, err)
	}

	second := verificationScheduler(store, "controller-b", 10, 1)
	if _, claimedAgain, err := second.claimReviewVerificationSlot("controller-b", "run-2", 0); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a second controller's claim succeeded although the first controller's claim already held the only slot")
	}
}

// TestAnAbandonedReviewVerificationClaimIsReclaimed proves the GENERAL
// reclaim path: once a DIFFERENT owner/run's claim is both past its own
// TTL and its owning controller is no longer alive - the same "death and
// expiry are both required" rule reclaimVerificationPermit already
// applies - a fresh claim attempt reclaims it and succeeds.
func TestAnAbandonedReviewVerificationClaimIsReclaimed(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	dead := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	first, claimed, err := dead.claimReviewVerificationSlot("controller-a", "run-1", 0)
	if err != nil || !claimed {
		t.Fatalf("claiming the only slot: claimed=%v err=%v", claimed, err)
	}

	// Not yet expired: a DIFFERENT owner and run's claim attempt is still
	// refused by the ceiling. This is never self-heal (#474 R9's own
	// leaked-claim recovery, proven separately below) - that only ever
	// touches an EXACT (owner, runID) match, never someone else's claim.
	other := Scheduler{
		Store: store, Clock: clock, Owner: "controller-b", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	if _, claimedAgain, err := other.claimReviewVerificationSlot("controller-b", "run-2", 0); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a second claim succeeded before the first one's TTL expired")
	}

	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	second, claimedAfterExpiry, err := other.claimReviewVerificationSlot("controller-b", "run-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !claimedAfterExpiry {
		t.Fatal("expected the expired, dead-owner claim to be reclaimed, freeing the slot")
	}
	if second.ID == first.ID {
		t.Fatal("the reclaimed slot was granted under the SAME claim id as the abandoned one")
	}
}

// TestALiveOwnersExpiredClaimIsNotReclaimed is R9's explicit third case:
// expiry ALONE, with the owner still reported alive, must never reclaim a
// claim - "death and expiry are both required" holds in both directions.
func TestALiveOwnersExpiredClaimIsNotReclaimed(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	s := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: alwaysAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	if _, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0); err != nil || !claimed {
		t.Fatalf("claiming the only slot: claimed=%v err=%v", claimed, err)
	}

	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	other := Scheduler{
		Store: store, Clock: clock, Owner: "controller-b", LeaseDuration: time.Minute,
		Liveness: alwaysAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	if _, claimed, err := other.claimReviewVerificationSlot("controller-b", "run-2", 0); err != nil {
		t.Fatal(err)
	} else if claimed {
		t.Fatal("expected a live owner's expired claim to remain held - expiry alone is never enough")
	}
}

// TestAnOrdinaryVerificationAcquisitionReclaimsADeadReviewClaim is R9's
// Scheduler.Next integration requirement: a dead, expired review claim
// must be freed by an ORDINARY assurance/verification acquisition with no
// review tick involved at all - claimReviewVerificationSlot's own reclaim
// is not enough, since nothing guarantees a review is ever triggered again
// for a repository that stops publishing PRs or never configured
// --reviewer-agent in the first place.
func TestAnOrdinaryVerificationAcquisitionReclaimsADeadReviewClaim(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	dead := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	if _, claimed, err := dead.claimReviewVerificationSlot("controller-a", "run-1", 0); err != nil || !claimed {
		t.Fatalf("claiming the only verification slot: claimed=%v err=%v", claimed, err)
	}

	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	assurance := Scheduler{
		Store: store, Clock: clock, Owner: "controller-b", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	planKind(t, assurance, "run-2", OpAssuranceGo)
	if leased := mustNext(t, assurance, "run-2"); leased == nil {
		t.Fatal("expected the ordinary verification acquisition to reclaim the dead, expired review claim and acquire the slot")
	}
}

// TestAFailedReleaseIsRecoveredOnceExpiredAndDeadByTheNextClaimForTheSameOwnerAndRun
// is R9's idempotent-retry requirement, as narrowed by #474 R11's second
// re-review (#5478592498): a leaked claim - simulated here by never
// releasing it - is recoverable within at most one TTL once its owner is
// ALSO provably dead, never on expiry alone. An EARLIER version of this
// recovered it the instant the same (owner, runID) pair attempted a fresh
// claim, merely once expired - removed because expiry is a wall-clock
// deadline, never proof a specific invocation actually finished (see
// TestASameOwnerAndRunClaimIsNeverSelfHealedWhileStillAlive for the
// negative case this guards: a live owner's claim is never freed merely
// because it is expired).
func TestAFailedReleaseIsRecoveredOnceExpiredAndDeadByTheNextClaimForTheSameOwnerAndRun(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	s := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	leaked, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0)
	if err != nil || !claimed {
		t.Fatalf("the first claim: claimed=%v err=%v", claimed, err)
	}

	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	healed, claimedAgain, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !claimedAgain {
		t.Fatal("expected the same (owner, run) pair's next claim attempt to recover a leaked, expired AND dead claim, never treat it as permanently held")
	}
	if healed.ID == leaked.ID {
		t.Fatal("the recovered claim reused the SAME id as the leaked one")
	}
}

// TestASameOwnerAndRunClaimIsNeverSelfHealedWhileStillAlive is #474 R11's
// required regression, across two re-reviews (#5478443739 then
// #5478592498): an earlier self-heal mechanism unconditionally deleted any
// existing claim matching (owner, runID) once merely EXPIRED, which could
// free a still-legitimately-running claim - expiry is a wall-clock
// deadline, never proof an invocation has actually finished (the
// reviewer's own provider budget does not cover every external operation
// in its full lifecycle). Self-heal was removed rather than patched: a
// fresh attempt for the SAME pair while the existing claim's owner is
// reported ALIVE must be refused exactly as anyone else's claim holding
// the only slot would be, however long it has been held, relying solely
// on the general death-AND-expiry reclaim every other resource in this
// system already uses.
func TestASameOwnerAndRunClaimIsNeverSelfHealedWhileStillAlive(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	s := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: alwaysAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	active, claimed, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0)
	if err != nil || !claimed {
		t.Fatalf("the first claim: claimed=%v err=%v", claimed, err)
	}

	// Long past the claim's own TTL, with the owner still reported alive:
	// a fresh attempt for the exact same pair is still refused.
	clock.now = clock.now.Add(10 * reviewVerificationClaimTTL)
	if _, claimedAgain, err := s.claimReviewVerificationSlot("controller-a", "run-1", 0); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a second attempt for the SAME (owner, runID) pair deleted and replaced a still-reported-alive claim merely because it had expired")
	}

	// The original claim must still be exactly the one holding the slot.
	remaining, err := store.ReviewVerificationClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != active.ID {
		t.Fatalf("claims = %+v, want exactly the original claim %q untouched", remaining, active.ID)
	}
}

// TestALiveReviewWithALongerBudgetIsNotReclaimedAtTheFixedMinimumTTL is
// #474 R11's required "live but expired [by the fixed minimum]" regression:
// a review bound to an operator-configured budget LONGER than
// reviewVerificationClaimTTL must not have its claim treated as abandoned
// merely because that shorter fixed minimum elapsed. The general
// death-AND-expiry reclaim (reclaimReviewVerificationClaims, the same path
// a plain Scheduler.Next tick exercises) must respect the LONGER ttl the
// caller actually claimed with, never the fixed constant alone - and,
// consistent with R11's second re-review, recovery still requires the
// owner be reported dead even once that longer budget has genuinely
// elapsed.
func TestALiveReviewWithALongerBudgetIsNotReclaimedAtTheFixedMinimumTTL(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	longBudget := 45 * time.Minute
	clock := &fakeClock{now: time.Unix(100, 0)}
	live := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: alwaysAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	original, claimed, err := live.claimReviewVerificationSlot("controller-a", "run-1", longBudget)
	if err != nil || !claimed {
		t.Fatalf("the first claim: claimed=%v err=%v", claimed, err)
	}

	// Past the fixed 15-minute minimum, but well within the 45-minute
	// budget this specific review actually claimed with: a review legitimately
	// still running this long is NOT abandoned.
	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	if reviewVerificationClaimTTL+time.Minute >= longBudget {
		t.Fatal("test setup error: the elapsed time must stay within the long budget")
	}

	// The general reclaim path (as a plain Scheduler.Next tick would
	// trigger) must not free it even though the owner is reported dead -
	// death alone is moot here because the longer ttl has not elapsed.
	dead := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentRuns: 10, MaxConcurrentVerifications: 1,
	}
	if err := dead.reclaimReviewVerificationClaims(); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ReviewVerificationClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != original.ID {
		t.Fatalf("the general reclaim path freed a claim still within its own longer budget: claims = %+v", remaining)
	}

	// A fresh attempt for the exact same (owner, runID) pair must likewise
	// refuse: the claim is not yet expired BY ITS OWN longer TTL, and the
	// shared verification ceiling (occupied by the still-present claim)
	// refuses a second one regardless.
	if _, claimedAgain, err := live.claimReviewVerificationSlot("controller-a", "run-1", longBudget); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a claim still within its own longer budget was released merely because the fixed minimum TTL had elapsed")
	}

	// Once the review's OWN actual budget has genuinely elapsed AND its
	// owner is reported dead, it is correctly reclaimable again (#474 R11,
	// second re-review: expiry alone is never enough, even past the
	// review's own real budget - death is required too, exactly as for
	// every other claim in this system).
	clock.now = clock.now.Add(longBudget)
	if _, claimedAfterRealExpiry, err := dead.claimReviewVerificationSlot("controller-a", "run-1", longBudget); err != nil {
		t.Fatal(err)
	} else if !claimedAfterRealExpiry {
		t.Fatal("expected the claim to be recoverable once its own actual, longer budget had elapsed and its owner was reported dead")
	}
}
