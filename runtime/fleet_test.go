package runtime

import (
	"context"
	"testing"
)

// THE CEILING BOUNDS WORKERS, NOT RUNS, so the fleet reports the two counts
// separately. Rendering non-terminal runs against the concurrency ceiling
// produced "3 / 2" on a fleet that was enforcing the ceiling exactly.
func TestTheFleetCountsExecutingSeparatelyFromNonterminal(t *testing.T) {
	fleet := Fleet{Capacity: 2}
	for _, summary := range []RunSummary{
		{Disposition: Active, Operation: "execution.invoke", Executing: true},
		{Disposition: Waiting, Operation: "execution.invoke", Executing: true},
		{Disposition: Waiting, Reason: "goal_state_reached"},
		{Disposition: Completed},
	} {
		if !terminalDisposition(summary.Disposition) {
			fleet.Active++
		}
		if summary.Executing {
			fleet.Executing++
		}
	}
	if fleet.Executing != 2 {
		t.Fatalf("executing = %d, want the two runs with a live operation", fleet.Executing)
	}
	if fleet.Active != 3 {
		t.Fatalf("nonterminal = %d, want the three unsettled runs", fleet.Active)
	}
	if fleet.Executing > fleet.Capacity {
		t.Fatalf("executing %d exceeds the ceiling %d", fleet.Executing, fleet.Capacity)
	}
}

// EXECUTING IS ASKED OF THE OPERATION ROWS, NOT OF THE REDUCED JOURNAL.
//
// This shipped: the first version read the snapshot's ActiveSince, which the
// reduction leaves set on operations that have ended, and the header reported
// twenty runs executing on a fleet running two - several of them completed
// hours earlier. The rows said two the whole time.
func TestExecutingIsTakenFromTheOperationRows(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	if _, err := fixture.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	// The run has real history now, so the reduced journal holds operations
	// that started and ended. None of them is executing.
	if executingNow(fixture.store, runID) {
		t.Fatal("a run whose operations have all ended was reported as executing")
	}
	operations, opErr := fixture.store.Operations(runID)
	if opErr != nil || len(operations) == 0 {
		t.Fatalf("HARNESS PRECONDITION: no operations to work with: %v", opErr)
	}

	// One attempt begins: state running, with an active attempt.
	active, version, found, err := fixture.store.Operation(operations[0].ID)
	if err != nil || !found {
		t.Fatalf("HARNESS PRECONDITION: %v found=%v", err, found)
	}
	began := fixture.runtime.deps.Clock.Now()
	active.State, active.ActiveSince = Running, &began
	version, ok, err := fixture.store.PutOperation(active, version)
	if err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("HARNESS PRECONDITION: the operation row was not updated")
	}
	if !executingNow(fixture.store, runID) {
		t.Fatal("a run with a running attempt was not reported as executing")
	}

	// And a running operation whose attempt has ended is not executing: the
	// scheduler clears ActiveSince, and that is the fact being read.
	active.ActiveSince = nil
	if _, _, err := fixture.store.PutOperation(active, version); err != nil {
		t.Fatal(err)
	}
	if executingNow(fixture.store, runID) {
		t.Fatal("a run whose attempt has ended was reported as executing")
	}
}
