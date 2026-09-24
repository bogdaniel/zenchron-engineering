package runtime

import "testing"

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
