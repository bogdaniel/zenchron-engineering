package runtime

// #474 R5: an independent-review trigger's durable claim and an ordinary
// assurance/verification operation share ONE authoritative capacity
// ceiling - never two uncoordinated counters. These tests exercise that
// directly against the real scheduler/store, the same verificationScheduler/
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

	claim, claimed, err := s.claimReviewVerificationSlot("controller-a")
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
	claim, claimed, err := s.claimReviewVerificationSlot("controller-a")
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
	if _, claimed, err := first.claimReviewVerificationSlot("controller-a"); err != nil || !claimed {
		t.Fatalf("first controller claiming the only slot: claimed=%v err=%v", claimed, err)
	}

	second := verificationScheduler(store, "controller-b", 10, 1)
	if _, claimedAgain, err := second.claimReviewVerificationSlot("controller-b"); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a second controller's claim succeeded although the first controller's claim already held the only slot")
	}
}

// TestAnAbandonedReviewVerificationClaimIsReclaimed proves the claim is not
// permanent: once it is BOTH past its own TTL and its owning controller is
// no longer alive - the same "death and expiry are both required" rule
// reclaimVerificationPermit already applies - a fresh claim attempt
// reclaims it and succeeds.
func TestAnAbandonedReviewVerificationClaimIsReclaimed(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &fakeClock{now: time.Unix(100, 0)}
	s := Scheduler{
		Store: store, Clock: clock, Owner: "controller-a", LeaseDuration: time.Minute,
		Liveness: neverAlive(), MaxConcurrentVerifications: 1,
	}
	first, claimed, err := s.claimReviewVerificationSlot("controller-a")
	if err != nil || !claimed {
		t.Fatalf("claiming the only slot: claimed=%v err=%v", claimed, err)
	}

	// Not yet expired: the dead owner's claim still holds, even though
	// Liveness already reports it dead - death ALONE is never enough.
	if _, claimedAgain, err := s.claimReviewVerificationSlot("controller-a"); err != nil {
		t.Fatal(err)
	} else if claimedAgain {
		t.Fatal("a second claim succeeded before the first one's TTL expired")
	}

	clock.now = clock.now.Add(reviewVerificationClaimTTL + time.Minute)
	second, claimedAfterExpiry, err := s.claimReviewVerificationSlot("controller-a")
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
