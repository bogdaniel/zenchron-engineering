package runtime

import (
	"context"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type reconcilerFunc func(context.Context, RunOperation) (OperationState, error)

func (f reconcilerFunc) Reconcile(c context.Context, o RunOperation) (OperationState, error) {
	return f(c, o)
}
func TestSchedulerStableIdempotencyLeaseAndRecovery(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	s := Scheduler{Store: store, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: OwnerLivenessFunc(func(string) bool { return true })}
	op, created, err := s.Plan(RunOperation{RunID: "r", Kind: "fake.side_effect", IdempotencyKey: StableOperationKey("r", "fake.side_effect", "a"), MaxAttempts: 1})
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if _, created, err = s.Plan(op); err != nil || created {
		t.Fatal(err, created)
	}
	leased, err := s.Next("r")
	if err != nil || leased == nil {
		t.Fatal(err)
	}
	if _, err = s.Start(leased.ID); err != nil {
		t.Fatal(err)
	}
	// A second healthy owner cannot take an expired lease merely by waiting.
	c.now = c.now.Add(2 * time.Minute)
	other := Scheduler{Store: store, Clock: c, Owner: "two", Liveness: OwnerLivenessFunc(func(string) bool { return true })}
	if got, err := other.Next("r"); err != nil || got != nil {
		t.Fatal(err, got)
	}
	if _, err = s.Reconcile(context.Background(), *leased, reconcilerFunc(func(context.Context, RunOperation) (OperationState, error) { return Succeeded, nil })); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Next("r"); err != nil || got != nil {
		t.Fatal(err, got)
	}
}
func TestSchedulerCancellationBecomesTerminal(t *testing.T) {
	c := &fakeClock{now: time.Unix(1, 0)}
	s := Scheduler{Store: NewMemoryOperationStore(), Clock: c, Owner: "x"}
	op, _, _ := s.Plan(RunOperation{RunID: "r", Kind: "long", IdempotencyKey: "k"})
	if _, err := s.RequestCancel(op.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Next("r"); err != nil || got != nil {
		t.Fatal(err, got)
	}
}

// A crashed owner's blown wall budget must end terminal with no lease left
// behind, otherwise the operation stays unacquirable forever.
func TestSchedulerWallBudgetTerminalClearsLease(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	s := Scheduler{Store: store, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: OwnerLivenessFunc(func(string) bool { return true })}
	op, _, err := s.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a", MaxAttempts: 2, WallBudget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next("r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(op.ID); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(5 * time.Minute)
	successor := Scheduler{Store: store, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: OwnerLivenessFunc(func(string) bool { return false })}
	if got, err := successor.Next("r"); err != nil || got != nil {
		t.Fatalf("budget-exceeded operation must not be leased: %v %v", got, err)
	}
	durable, _, ok, err := store.Operation(op.ID)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if durable.State != OperationFailed || durable.Lease != nil {
		t.Fatalf("expected terminal failed state with cleared lease, got %+v", durable)
	}
}

// An absent OwnerLiveness must not read as "the owner is dead". Otherwise wall
// clock expiry alone would authorize a takeover, and two schedulers could drive
// the same operation.
func TestSchedulerWithoutConfiguredLivenessDoesNotStealExpiredLease(t *testing.T) {
	store := NewMemoryOperationStore()
	clock := &fakeClock{now: time.Unix(2000, 0).UTC()}
	owner := NewRuntimeOwner()
	planner := Scheduler{Store: store, Clock: clock, Owner: owner}
	op, _, err := planner.Plan(RunOperation{RunID: "run", Kind: "assurance.go", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	// A lease held by this live process, already past its expiry.
	op.State = Leased
	op.Lease = &Lease{Owner: owner, HeartbeatAt: clock.now, ExpiresAt: clock.now.Add(-time.Hour)}
	_, revision, _, err := store.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PutOperation(op, revision); err != nil {
		t.Fatal(err)
	}

	taker := Scheduler{Store: store, Clock: clock, Owner: "other-instance"}
	next, err := taker.Next("run")
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("expired lease held by a live owner was stolen by %q", next.Lease.Owner)
	}
}

// TestOperatorCeilingCannotBeRaisedByConfiguration pins the ceiling rule: the
// default is one, a requested value may only lower it, and raising it requires
// the separate operator-authorized value. Repository configuration reaches the
// requested side only, so it can never widen the ceiling.
func TestOperatorCeilingCannotBeRaisedByConfiguration(t *testing.T) {
	for _, c := range []struct{ requested, operator, want int }{
		{0, 0, 1},  // M0 default
		{8, 0, 1},  // configuration asking for more is clamped to the default
		{8, 1, 1},  // ... and to an explicit ceiling of one
		{1, 4, 1},  // lowering is always allowed
		{8, 4, 4},  // a request above the authorization is clamped to it
		{4, 4, 4},  // an operator authorization is honoured
		{-3, 0, 1}, // nonsense never widens anything
	} {
		if got := resolveMaxConcurrentRuns(c.requested, c.operator); got != c.want {
			t.Fatalf("resolveMaxConcurrentRuns(%d, %d) = %d, want %d", c.requested, c.operator, got, c.want)
		}
	}
}
