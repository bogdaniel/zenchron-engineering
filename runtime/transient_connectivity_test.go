package runtime

// Regression proofs for #380: a recognized transient connectivity failure must
// enter a bounded external wait instead of exhausting an operation's attempt
// budget immediately.
//
// The live dogfood this pins: a continuation lost Internet connectivity
// mid-run. The execution provider's own connectivity diagnostic was not
// recognized (closed by agent_specs.go's bare "enotfound" token - see
// TestRecognizedConnectivityDiagnosticsRouteToABoundedWait), and the run's
// NEXT observation - source.observe, re-deriving the pinned issue and base
// against the same unreachable GitHub - recorded no failure class at all. An
// unclassified failure is a budget-only retry, and the reconciler's own pass
// loop retries that within the SAME call with no delay between tries, so all
// three of the operation's attempts were spent against a network that was not
// coming back that fast: the run terminalized as
// source.observe_attempts_exhausted, in under a second, with connectivity
// never given a chance to recover.
//
// Everything here is the real EngineeringRuntime against the phase 8 fixture:
// no network, no real GitHub, an injected failure on the fake forge's Issue
// call. Assertions are against the persisted journal and the forge's own
// recorded call sequence, not against timing.

import "testing"

// TestGitHubTransientFailureDuringSourceObserveWaitsWithoutExhaustingAttempts
// is the restored incident. A GitHubTransientError on the pinned issue lookup
// - the adapter's own typed statement that the forge was unreached or refused
// for a reason expected to clear on its own - must route source.observe to a
// bounded external wait, never to an unclassified, budget-only retry.
func TestGitHubTransientFailureDuringSourceObserveWaitsWithoutExhaustingAttempts(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" {
			return &GitHubTransientError{Detail: "the GitHub API endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	// THE DEFECT ITSELF: the forge was asked exactly once in this pass, not
	// three times in a tight loop against an outage no attempt of theirs could
	// clear.
	issueCalls := 0
	for _, method := range fixture.forge.Methods() {
		if method == "Issue" {
			issueCalls++
		}
	}
	if issueCalls != 1 {
		t.Fatalf("the forge was asked %d times in one reconciliation pass, want exactly one", issueCalls)
	}

	op := onlyOperation(t, fixture, runID, OpSourceObserve)
	if op.Attempt != 1 {
		t.Fatalf("source.observe durably recorded %d attempts, want 1 - RestoreAttempt must give the attempt back on a wait", op.Attempt)
	}

	class := durableFailureClass(t, op)
	if class != FailureGitHubTransient {
		t.Fatalf("source.observe recorded failure class %q, want %q", class, FailureGitHubTransient)
	}
	if route := RouteFailure(class); route != RouteWait {
		t.Fatalf("%q routes to %q, want a bounded wait", class, route)
	}

	// The run WAITS. It must not fail, and it must not have spent its attempt
	// budget doing so.
	if outcome.Disposition != Waiting {
		t.Fatalf("outcome = %#v, want the run waiting on the external connectivity condition", outcome)
	}
	if outcome.Reason != waitReason(FailureGitHubTransient) {
		t.Fatalf("outcome reason = %q, want %q", outcome.Reason, waitReason(FailureGitHubTransient))
	}
}

// TestGitHubTransientFailureRecoversWithinExistingAuthority proves the other
// half: once connectivity clears, the SAME run resumes and makes real
// progress - no budget was minted, none was reset, and the attempt the wait
// restored is simply never needed again because the run re-derives a fresh
// observation at the next epoch, exactly as it would have if the first
// observation had simply never been attempted.
func TestGitHubTransientFailureRecoversWithinExistingAuthority(t *testing.T) {
	fixture := newPhase8Fixture(t)
	failing := true
	fixture.inject(func(call GitHubCall) error {
		if failing && call.Method == "Issue" {
			return &GitHubTransientError{Detail: "the GitHub API endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()
	waiting := fixture.reconcile(runID)
	if waiting.Disposition != Waiting {
		t.Fatalf("first pass outcome = %#v, want waiting on connectivity", waiting)
	}

	// The run has done nothing else yet - it is waiting on the very first
	// operation - so exactly one source.observe operation exists, and it is
	// the one that recorded the transient failure.
	waitingOp := onlyOperation(t, fixture, runID, OpSourceObserve)
	if waitingOp.Attempt != 1 {
		t.Fatalf("the failed attempt is recorded as attempt %d, want 1", waitingOp.Attempt)
	}
	if class := durableFailureClass(t, waitingOp); class != FailureGitHubTransient {
		t.Fatalf("source.observe recorded failure class %q, want %q", class, FailureGitHubTransient)
	}

	// Connectivity recovers. Nothing about the run was torn down while it
	// waited, so it is free to run all the way to completion from here.
	failing = false
	outcome := fixture.reconcile(runID)
	if outcome.Disposition == Failed {
		t.Fatalf("the run failed after connectivity recovered: %#v", outcome)
	}

	state := fixture.state(runID)
	if state.source == nil {
		t.Fatal("the recovered run never pinned a source snapshot")
	}
	if countType(state.events, EventCandidateCommitted) == 0 {
		t.Fatal("the recovered run never produced a committed candidate; connectivity recovering did not let it resume")
	}

	// The wait left no trace of having spent anything it should not have: the
	// ORIGINAL failed attempt is still exactly what it was - one attempt, the
	// transient class, never silently rewritten into a success or erased now
	// that the run moved on.
	still, ok := state.snapshot.Operations[waitingOp.ID]
	if !ok {
		t.Fatalf("operation %s is gone from the journal after the run resumed", waitingOp.ID)
	}
	if still.State != OperationFailed || still.Attempt != 1 || durableFailureClass(t, still) != FailureGitHubTransient {
		t.Fatalf("the original failed attempt was rewritten: state=%s attempt=%d class=%q",
			still.State, still.Attempt, durableFailureClass(t, still))
	}
}

// onlyOperation asserts the journal holds exactly one operation of the given
// kind for this run, and returns it. A run barely started - waiting on its
// very first operation - has exactly one of any kind, so this is where
// durableInvoke's multi-attempt search (which reads the one operation id a
// RETRIED operation keeps across attempts) would be the wrong tool: here the
// assertion is that nothing else has run yet at all.
func onlyOperation(t *testing.T, fixture *phase8Fixture, runID, kind string) RunOperation {
	t.Helper()
	state := fixture.state(runID)
	var found RunOperation
	seen := 0
	for _, op := range state.snapshot.Operations {
		if op.Kind == kind {
			found, seen = op, seen+1
		}
	}
	if seen != 1 {
		t.Fatalf("journal holds %d %s operations, want exactly one: %v", seen, kind, journalTypes(state.events))
	}
	return found
}
