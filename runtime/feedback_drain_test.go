package runtime

// The consumption record is what drains the pending set. Both defects below
// were introduced by the fix for an earlier finding about that set never
// draining, which is why they are pinned together.

import (
	"encoding/json"
	"testing"
)

// TestAConsumptionRecordMayCarryOnlyLostItems. When every pending item's text
// artifact has been reclaimed, nothing was delivered and the record exists
// solely to drain the set. Requiring Keys made that event unwritable: AppendEvent
// failed AFTER the provider had already been invoked, so every later pass
// re-derived the same pending set, re-ran the provider, and died at the same
// append until the attempt budget was gone.
func TestAConsumptionRecordMayCarryOnlyLostItems(t *testing.T) {
	validate, ok := eventPayloads[EventFeedbackConsumed]
	if !ok {
		t.Fatal("feedback.consumed has no payload validator")
	}
	onlyLost, err := json.Marshal(FeedbackConsumedPayload{
		Unavailable: []string{"pull_request_review:1"},
		OperationID: "run:execution.invoke:initial|1|base", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(onlyLost); err != nil {
		t.Fatalf("a drain-only consumption record was refused, so the pending set can never empty: %v", err)
	}

	// A record that accounts for nothing at all is still refused.
	empty, err := json.Marshal(FeedbackConsumedPayload{
		OperationID: "run:execution.invoke:initial|1|base", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(empty); err == nil {
		t.Fatal("a consumption record naming no items at all was accepted")
	}
}

// TestFeedbackIsNotConsumedWhenNoWorkerRan. CLIAgentProvider refuses before
// starting a process on a failed capability probe, a missing home or a missing
// executable. Recording delivery on those paths meant an agent CLI absent for
// one tick marked a reviewer's comment consumed, dropped its binding, and the
// review reached nobody - ever.
func TestFeedbackIsNotConsumedWhenNoWorkerRan(t *testing.T) {
	// The distinction the runtime uses: invocation provenance is written only
	// after the process returns, so it separates "never started" from "ran and
	// failed". An attempt that ran and failed must still count, because
	// re-delivering a human's review after a failure would duplicate it.
	neverStarted := ExecutionResult{}
	if neverStarted.Invocation != nil {
		t.Fatal("a refused invocation carries provenance, so the two cases cannot be told apart")
	}
	ranAndFailed := ExecutionResult{
		Outcome:    OperationFailed,
		Invocation: &InvocationProvenance{AgentID: "codex", Executable: "codex"},
		Failure:    &ProviderFailure{Classification: FailureUnknown},
	}
	if ranAndFailed.Invocation == nil {
		t.Fatal("an invocation that ran records no provenance, so delivery could never be recorded")
	}
}
