package runtime

import "testing"

// TestGovernedEpochAuthorizationNeverResumesTheOriginalRun is the #581 Batch 3
// acceptance regression for the binding scope's own requirement #2: "Human
// resolution of an epoch authorization must not accidentally invoke #508's
// existing same-RunID continuation." Broken before the decision_store.go fix
// (confirmed by running this exact test against the pre-fix code: the
// ORIGINAL, already goal-settled producer run received a SECOND
// execution.invoke operation - key "decision-resumed|...", actually
// dispatched (operation.before/after), that went on to plan a further
// candidate.commit on a run every other #581 invariant treats as
// historically immutable - because the epoch_authorization decision_request
// (remediation_eligibility.go) shares that run's RunID for #473 scope
// routing, and resolvedOwnDecisionRequestsTx (decision_store.go) read ANY
// resolved decision_request with that run_id as "this run's own", with no
// System-sourced exclusion.
func TestGovernedEpochAuthorizationNeverResumesTheOriginalRun(t *testing.T) {
	g := driveToGovernedRecovery(t)
	run, ok := storedRun(t, g.fixture.phase8Fixture, g.blamedOriginalRunID)
	if !ok {
		t.Fatal("original run not found")
	}
	events, err := g.fixture.store.Events(g.blamedOriginalRunID)
	if err != nil {
		t.Fatal(err)
	}
	var invokeOps []RunOperation
	for _, event := range events {
		if event.Type != EventOperationPlanned {
			continue
		}
		var op RunOperation
		if err := decodeJSON(event.Payload, &op); err != nil {
			t.Fatal(err)
		}
		if op.Kind == OpExecutionInvoke {
			invokeOps = append(invokeOps, op)
		}
	}
	if len(invokeOps) != 1 {
		t.Fatalf("original run %s (disposition=%s reason=%s) received %d execution.invoke operations, want exactly 1 "+
			"(the epoch-1 invocation): %+v", run.ID, run.Disposition, run.Reason, len(invokeOps), invokeOps)
	}
}
