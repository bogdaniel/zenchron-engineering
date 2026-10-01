package runtime

// transient_connectivity_test.go pins the #380 dogfood regression: a
// recognized transient connectivity failure during source.observe must enter
// a durable wait instead of being left unclassified and burning its whole
// attempt budget in one Reconcile pass.
//
// In the failed run, a DNS/network-unreachable failure under
// source.observe#epoch-37 consumed attempts 1, 2 and 3 in the same second and
// the run terminalized as source.observe_attempts_exhausted, because the
// handler's failed(err) recorded no FailureClass at all - the same unrouted
// shape retry_routing_test.go's #29 regression exercises for execution.invoke,
// reached here through a different operation.

import (
	"testing"
)

// TestSourceObserveTransientConnectivityWaitsWithoutSpendingAttempts is the
// restored incident: the forge fails the exact source.observe call with a
// recognized connectivity failure twice, then answers normally. Each of the
// two failing passes must settle the run WAITING - never failed, and never
// more than one attempt per pass - and the run must still be able to proceed
// once the forge answers again.
func TestSourceObserveTransientConnectivityWaitsWithoutSpendingAttempts(t *testing.T) {
	fixture := newPhase8Fixture(t)
	failing := 2
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" && failing > 0 {
			failing--
			return &GitHubTransientError{Detail: "github endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()

	first := fixture.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "source_forge_unavailable" {
		t.Fatalf("first outcome = %#v, want waiting/source_forge_unavailable", first)
	}

	second := fixture.reconcile(runID)
	if second.Disposition != Waiting || second.Reason != "source_forge_unavailable" {
		t.Fatalf("second outcome = %#v, want still waiting on the forge", second)
	}

	if failing != 0 {
		t.Fatalf("forge still has %d scripted connectivity failures left, want them both consumed one pass at a time", failing)
	}

	// The defect this pins: before classification, these two failures would
	// have been unrouted and consumed inside a single Reconcile call, so the
	// run would already have failed with source.observe_attempts_exhausted
	// well before the forge ever got a chance to answer normally.
	for _, op := range fixture.state(runID).snapshot.Operations {
		if op.Kind == OpSourceObserve && op.State == OperationFailed && op.Attempt > 1 {
			t.Fatalf("source.observe operation %q recorded %d attempts before failing, want at most 1 per pass", op.ID, op.Attempt)
		}
	}

	third := fixture.reconcile(runID)
	if third.Disposition == Failed {
		t.Fatalf("third outcome = %#v, want the run to recover once the forge answers", third)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got < 3 {
		t.Fatalf("the forge was asked %d times for the issue, want at least 3 (2 failures + 1 recovery)", got)
	}
}

// TestSourceObserveUnrecognizedFailureStaysUnrouted proves the fix is narrow:
// an error the forge boundary has not been told to recognize as transient
// connectivity keeps today's behaviour - an unclassified failure under
// budget-only retry, exhausting its attempts inside one pass - rather than
// being guessed into the connectivity wait. This is the fail-closed half of
// #380: only a RECOGNIZED condition gets the durable wait.
func TestSourceObserveUnrecognizedFailureStaysUnrouted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" {
			return &GitHubAPIError{Status: 422, Detail: "the forge rejected the request"}
		}
		return nil
	})
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	if outcome.Disposition != Failed || outcome.Reason != OpSourceObserve+"_attempts_exhausted" {
		t.Fatalf("outcome = %#v, want the pre-existing budget-only-retry behaviour (attempts exhausted in one pass)", outcome)
	}

	state := fixture.state(runID)
	var observed RunOperation
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpSourceObserve {
			observed = op
		}
	}
	if class := durableFailureClass(t, observed); class != "" {
		t.Fatalf("durable failure class = %q, want unclassified for an unrecognized forge fault", class)
	}
}
