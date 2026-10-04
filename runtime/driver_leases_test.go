package runtime

import (
	"reflect"
	"testing"
	"time"
)

// strandedFixture plans one operation, has a driver of the live owner "one"
// lease and start it, and then has that driver stop holding it WITHOUT
// finishing it or writing anything - the shape a store error mid-pass leaves.
func strandedFixture(t *testing.T, drivers *DriverLeases) (Scheduler, *MemoryOperationStore, RunOperation) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	store := NewMemoryOperationStore()
	alive := OwnerLivenessFunc(func(string) bool { return true })
	s := Scheduler{Store: store, Clock: clock, Owner: "one", LeaseDuration: time.Minute, Liveness: alive, Drivers: drivers,
		MaxConcurrentRuns: 1, MaxConcurrentObservations: 1}
	planned, _, err := s.Plan(RunOperation{RunID: "r", Kind: "fake.work", IdempotencyKey: "k", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if leased, err := s.Next("r"); err != nil || leased == nil || leased.ID != planned.ID {
		t.Fatalf("lease: %v %v", leased, err)
	}
	started, err := s.Start(planned.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The pass returns here. The in-process hold ends; the durable lease does
	// not, because the write that would have ended it is the one that failed.
	drivers.release(planned.ID)
	clock.now = clock.now.Add(10 * time.Minute)
	return s, store, started
}

// TestALeaseThisProcessStrandedIsRecoveredByItsNextPass is #485 at the
// scheduler: the owner is alive, so the dead-owner rule never applies, yet no
// driver here holds the lease. The next scan in the SAME process removes the
// lease - and only the lease - and the operation is leasable again.
func TestALeaseThisProcessStrandedIsRecoveredByItsNextPass(t *testing.T) {
	s, store, stranded := strandedFixture(t, NewDriverLeases())
	again, err := s.Next("r")
	if err != nil || again == nil || again.ID != stranded.ID {
		t.Fatalf("the same live process could not recover its stranded lease: %v %v", again, err)
	}
	// What the recovery wrote before re-leasing: the row it reclaimed must
	// differ from the stranded one ONLY by the lease. Re-read through the
	// store after relinquishing this new lease, which also drops only it.
	if err := s.Relinquish(again.ID, *again.Lease); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := store.Operation(stranded.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := stranded
	want.Lease = nil
	// Next sets Leased when it takes the operation; nothing else moved.
	after.State = want.State
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("recovery rewrote more than the lease:\n got %+v\nwant %+v", after, want)
	}
	if after.Attempt != 1 || after.AttemptIdentity != 1 || after.ActiveSince == nil {
		t.Fatalf("attempt accounting changed: attempt %d identity %d active-since %v", after.Attempt, after.AttemptIdentity, after.ActiveSince)
	}
}

// TestALeaseAConcurrentDriverHoldsIsNeverReclaimed: same owner, but a driver
// in this process holds it. That is live work, not a stranded lease.
func TestALeaseAConcurrentDriverHoldsIsNeverReclaimed(t *testing.T) {
	drivers := NewDriverLeases()
	s, store, stranded := strandedFixture(t, drivers)
	drivers.hold(stranded.ID)
	if got, err := s.Next("r"); err != nil || got != nil {
		t.Fatalf("a held lease was taken: %v %v", got, err)
	}
	if row, _, _, _ := store.Operation(stranded.ID); row.Lease == nil {
		t.Fatal("a held lease was dropped")
	}
}

// TestAnotherLiveOwnersExpiredLeaseIsNeverReclaimed: the record speaks only
// for this process. Another owner that is alive keeps its lease, expired or not.
func TestAnotherLiveOwnersExpiredLeaseIsNeverReclaimed(t *testing.T) {
	_, store, stranded := strandedFixture(t, NewDriverLeases())
	other := Scheduler{Store: store, Clock: &fakeClock{now: time.Unix(1_000, 0).Add(time.Hour)}, Owner: "two",
		LeaseDuration: time.Minute, Liveness: OwnerLivenessFunc(func(string) bool { return true }),
		Drivers: NewDriverLeases(), MaxConcurrentRuns: 1, MaxConcurrentObservations: 1}
	if got, err := other.Next("r"); err != nil || got != nil {
		t.Fatalf("another process took a live owner's lease: %v %v", got, err)
	}
	if row, _, _, _ := store.Operation(stranded.ID); row.Lease == nil || row.Lease.Owner != "one" {
		t.Fatalf("another process dropped a live owner's lease: %+v", row.Lease)
	}
	if err := other.Relinquish(stranded.ID, Lease{Owner: "two"}); err != nil {
		t.Fatal(err)
	}
	if row, _, _, _ := store.Operation(stranded.ID); row.Lease == nil {
		t.Fatal("Relinquish dropped a lease its scheduler was never granted")
	}
}

// TestWithoutARecordAStrandedLeaseIsNotGuessedAt: a scheduler that cannot tell
// its own drivers' leases apart recovers nothing new. Unknown is not stranded.
func TestWithoutARecordAStrandedLeaseIsNotGuessedAt(t *testing.T) {
	s, _, _ := strandedFixture(t, NewDriverLeases())
	s.Drivers = nil
	if got, err := s.Next("r"); err != nil || got != nil {
		t.Fatalf("a scheduler with no record took an own-owner lease: %v %v", got, err)
	}
}

// acquireHook runs a sibling's work the instant a lease has been written,
// before the acquiring Next has returned it to its driver.
type acquireHook struct {
	*MemoryOperationStore
	after func()
}

func (s *acquireHook) AcquireOperation(op RunOperation, expected int64, maxRuns, maxObservations int) (int64, bool, error) {
	revision, acquired, err := s.MemoryOperationStore.AcquireOperation(op, expected, maxRuns, maxObservations)
	if acquired && s.after != nil {
		after := s.after
		s.after = nil
		after()
	}
	return revision, acquired, err
}

// TestALeaseIsHeldBeforeItIsWritten: a sibling driver in the same process that
// scans in the window between the durable acquisition and the acquiring driver
// receiving the operation must read it as held, never as stranded.
func TestALeaseIsHeldBeforeItIsWritten(t *testing.T) {
	store := &acquireHook{MemoryOperationStore: NewMemoryOperationStore()}
	drivers := NewDriverLeases()
	s := Scheduler{Store: store, Clock: &fakeClock{now: time.Unix(1_000, 0)}, Owner: "one", LeaseDuration: time.Minute,
		Liveness: OwnerLivenessFunc(func(string) bool { return true }), Drivers: drivers,
		MaxConcurrentRuns: 2, MaxConcurrentObservations: 2}
	first, _, err := s.Plan(RunOperation{RunID: "r1", Kind: "fake.work", IdempotencyKey: "k1", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Plan(RunOperation{RunID: "r2", Kind: "fake.work", IdempotencyKey: "k2", MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	store.after = func() {
		if _, err := s.Next("r2"); err != nil {
			t.Error(err)
		}
	}
	leased, err := s.Next("r1")
	if err != nil || leased == nil || leased.ID != first.ID {
		t.Fatalf("lease: %v %v", leased, err)
	}
	if row, _, _, _ := store.Operation(first.ID); row.Lease == nil {
		t.Fatal("a sibling scan reclaimed a lease its driver had just been granted")
	}
}

// TestADriverReturningLateDoesNotDropItsSuccessorsLease: two drivers of one run
// in this process overlap on one operation - the first finished it retryably
// and is still returning while the second has leased it again. The first
// one's relinquish and release must leave the second one's lease and hold
// exactly as they are.
func TestADriverReturningLateDoesNotDropItsSuccessorsLease(t *testing.T) {
	drivers := NewDriverLeases()
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	s := Scheduler{Store: NewMemoryOperationStore(), Clock: clock, Owner: "one", LeaseDuration: time.Minute,
		Liveness: OwnerLivenessFunc(func(string) bool { return true }), Drivers: drivers,
		MaxConcurrentRuns: 1, MaxConcurrentObservations: 1}
	planned, _, err := s.Plan(RunOperation{RunID: "r", Kind: "fake.work", IdempotencyKey: "k", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Next("r")
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if _, err := s.Start(planned.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(planned.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	second, err := s.Next("r")
	if err != nil || second == nil {
		t.Fatalf("the successor could not lease the retryable operation: %v %v", second, err)
	}
	// The first driver returns now.
	if err := s.Relinquish(first.ID, *first.Lease); err != nil {
		t.Fatal(err)
	}
	row, _, _, _ := s.Store.Operation(planned.ID)
	if row.Lease == nil || !sameLease(*row.Lease, *second.Lease) {
		t.Fatalf("the late driver dropped its successor's lease: %+v", row.Lease)
	}
	if s.stranded(row) {
		t.Fatal("the late driver's release erased its successor's hold")
	}
}
