package runtime

// A controller shutdown is not the work failing and not the operator cancelling
// a run. Those are three different acts, and only one of them is terminal.

import (
	"strings"
	"testing"
)

// TestControllerShutdownLeavesTheRunResumable is the lifecycle law docs/supervisor.md
// promises: shutdown propagates cancellation into in-flight work, and every run
// stays exactly as resumable as its journal says.
//
// Recording a cancelled invocation as FailureUnknown broke that promise
// silently: RouteFailure(FailureUnknown) is RouteStop, the re-attempt rule
// requires RouteRetry or a wait, and the run settled terminal. A supervisor
// shutting down therefore killed whatever was mid-flight.
func TestControllerShutdownLeavesTheRunResumable(t *testing.T) {
	if route := RouteFailure(FailureControllerShutdown); route != RouteWait {
		t.Fatalf("a controller shutdown routes to %q; it must wait, not stop", route)
	}
	if route := RouteFailure(FailureUnknown); route != RouteStop {
		t.Fatal("FailureUnknown stopped routing to RouteStop, so this test no longer proves the distinction")
	}
	// It settles into a named wait rather than an anonymous one.
	reason, ok := waitReasons[FailureControllerShutdown]
	if !ok || reason == "" {
		t.Fatal("a controller shutdown has no durable wait reason")
	}
	// And that wait does not spend the execution budget: the run is not
	// working, it is waiting for a supervisor to exist again.
	if !externalWaitReasons[reason] {
		t.Fatalf("wait reason %q spends the execution budget while the controller is down", reason)
	}
}

// TestOperatorCancellationStaysTerminal is the other half. Making shutdown
// resumable must not weaken `stop RUN` or `stop-all`: those are explicit
// operator acts that journal run.cancelled, and the Cancelled disposition takes
// precedence over every wait below it.
func TestOperatorCancellationStaysTerminal(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	outcome, err := CancelRun(fixture.store, fixture.runtime.scheduler, fixture.clock.Now(), runID, "operator/stop")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Cancelled {
		t.Fatalf("stop produced %q, want cancelled", outcome.Disposition)
	}
	state, err := fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	disposition, reason := state.conditions()
	if disposition != Cancelled {
		t.Fatalf("a cancelled run settled as %q/%q; operator cancellation must outrank every wait", disposition, reason)
	}
	if !terminalDisposition(disposition) {
		t.Fatal("operator cancellation stopped being terminal")
	}
	// The cancellation is durable, and named as the operator's act.
	found := false
	for _, event := range state.events {
		if event.Type == EventRunCancelled {
			found = true
			if !strings.Contains(string(event.Payload), "operator/stop") {
				t.Fatalf("the cancellation does not record why: %s", event.Payload)
			}
		}
	}
	if !found {
		t.Fatal("stop journalled no run.cancelled event")
	}
}
