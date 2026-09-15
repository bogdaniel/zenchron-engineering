package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// stopAtAcquisition forces the ONE interleaving that matters, deterministically
// rather than by timing: the operator's stop commits in full - the run document
// is written and its operations are scanned - and only then does the driver's
// durable acquisition statement run.
//
// That ordering is not contrived. CancelRun is reached from the control
// endpoint's stop-all on its own goroutine while Supervisor.Tick is driving
// runs on others, and Supervisor.mu serializes neither. A driver that read its
// run before the stop is inside a pass whose every decision is already stale.
type stopAtAcquisition struct {
	OperationStore
	t       *testing.T
	fixture *phase8Fixture
	runID   string
	fired   bool
}

func (s *stopAtAcquisition) AcquireOperation(op RunOperation, expected int64, maxRuns int) (int64, bool, error) {
	if !s.fired && op.Kind == OpExecutionInvoke {
		s.fired = true
		if _, err := CancelRun(s.fixture.store, s.fixture.runtime.scheduler, s.fixture.clock.Now(), s.runID, "operator/stop"); err != nil {
			s.t.Fatal(err)
		}
		run, found, err := s.fixture.store.Run(s.runID)
		if err != nil || !found {
			s.t.Fatal(err, found)
		}
		if run.Disposition != Cancelled {
			s.t.Fatalf("the stop did not commit before the acquisition: %q", run.Disposition)
		}
	}
	return s.OperationStore.AcquireOperation(op, expected, maxRuns)
}

// TestAStopAtAcquisitionNeverExecutesTheWorkItStopped is the cancel-versus-
// acquire proof. A run is stopped after its driver has read it and before that
// driver leases the execution operation, and the provider must not be invoked -
// not on that pass, and not on any pass after it.
//
// The name says ACQUISITION deliberately. A stop landing one durable write
// later - after Start - is a different window and is NOT covered: nothing on
// the executing path re-reads the run or the cancellation flag, so that attempt
// finishes. It reproduces identically on main, it is the mid-flight drain case,
// and closing it needs cooperative cancellation rather than a durable condition.
//
// It fails three different ways without the three parts of the repair, which is
// why all three are asserted here rather than in separate tests:
//
//   - with the acquisition statement unguarded, the provider is invoked on THIS
//     pass: nothing between Next and handle re-reads the run;
//   - with only the acquisition guarded, the pass settles on its stale view,
//     appends run.waiting after run.cancelled, writes the run document back to
//     waiting, and the provider is invoked on the NEXT pass;
//   - with only the acquisition guarded and the settle reporting what it was
//     asked for, the operator is told the run is waiting when it is stopped.
func TestAStopAtAcquisitionNeverExecutesTheWorkItStopped(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	stop := &stopAtAcquisition{OperationStore: f.runtime.scheduler.Store, t: t, fixture: f, runID: runID}
	f.runtime.scheduler.Store = stop

	// Passes before the execution operation are ordinary work; the stop arms
	// itself on the operation that actually reaches the provider.
	var outcome Outcome
	for pass := 0; pass < 8 && !stop.fired; pass++ {
		var err error
		outcome, err = f.runtime.Reconcile(context.Background(), runID)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if !stop.fired {
		t.Fatal("the execution operation was never acquired, so nothing was contested")
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("the execution provider was invoked %d time(s) on a run the operator had already stopped", len(f.provider.requests))
	}
	if outcome.Disposition != Cancelled {
		t.Fatalf("the pass reported %q %q over a stopped run", outcome.Disposition, outcome.Reason)
	}
	stopped, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if stopped.Disposition != Cancelled {
		t.Fatalf("the pass settled a stopped run back to %q, returning it to the supervisor's active set", stopped.Disposition)
	}
	// The supervisor would drive this run again on the next tick if anything
	// above had un-stopped it, so the next pass is part of the assertion.
	if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("a later pass invoked the execution provider %d time(s) on a stopped run", len(f.provider.requests))
	}
	// Nothing the repair does may cost the run its slot back: #171's whole
	// point is that a stopped run holds nothing.
	operations, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if op.Lease != nil {
			t.Fatalf("operation %q still holds a lease on a stopped run", op.ID)
		}
	}
}

// TestSQLiteAStoppedRunsOperationIsNeverAcquired states the durable rule on its
// own, across two database handles, because the statement is where the rule has
// to live: the run document and the acquisition are written by different
// processes, and only SQLite serializes them.
//
// The operation is merely PLANNED when the stop lands, which is exactly the case
// CancelRun's own loop cannot answer - it finishes what is leased or running,
// and a pending operation is neither. A driver reaching Next afterwards must be
// refused by the acquisition itself.
func TestSQLiteAStoppedRunsOperationIsNeverAcquired(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	if err := storeA.PutRun(newJournalRun("run-a")); err != nil {
		t.Fatal(err)
	}
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 2}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 2}
	planned := planFor(t, one, "run-a", 2)
	if _, err := CancelRun(storeA, one, c.Now(), "run-a", "operator/stop"); err != nil {
		t.Fatal(err)
	}
	got, err := two.Next("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("a stopped run's operation %q was leased by %q", got.ID, "two")
	}
	stored, _, found, err := storeB.Operation(planned.ID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if stored.Lease != nil || stored.State == Leased || stored.State == Running {
		t.Fatalf("the refused acquisition still wrote the lease: %+v", stored)
	}
}

// TestAStaleWaitNeverUnStopsARun covers the one window the guard in
// recordDisposition cannot close by itself: a stop that commits between that
// guard's read of the run and its own write. Durably that is indistinguishable
// from any other stale settle - the journal carries run.waiting after
// run.cancelled and the run document says waiting - so the state is built here
// exactly as such a race would leave it, and it is also the state a database
// that raced before this repair is already sitting in.
//
// Replay is what remembers. The next pass must refuse on the run being terminal
// before it plans anything, settle the document back, and reach no provider.
func TestAStaleWaitNeverUnStopsARun(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
		t.Fatal(err)
	}
	invocations := len(f.provider.requests)

	// The losing driver's settle, landing after the stop.
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{"operation_unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: runID + "-stale-wait", RunID: runID,
		Type: EventRunWaiting, OccurredAt: f.clock.Now(), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	stale, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	stale.Disposition, stale.Reason = Waiting, "operation_unavailable"
	if err := f.store.PutRun(stale); err != nil {
		t.Fatal(err)
	}

	outcome, err := f.runtime.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Cancelled {
		t.Fatalf("a stale wait un-stopped the run: the next pass reported %q %q", outcome.Disposition, outcome.Reason)
	}
	if len(f.provider.requests) != invocations {
		t.Fatalf("the execution provider was invoked on a run the operator had stopped")
	}
	repaired, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if repaired.Disposition != Cancelled {
		t.Fatalf("the run document was left %q, so the supervisor keeps driving a stopped run", repaired.Disposition)
	}
}
