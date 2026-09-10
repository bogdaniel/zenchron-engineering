package main

import (
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// Lost-reply recovery answers a HISTORICAL question.
//
// It asked "is this still the latest approval", which the snapshot's approval
// slot answers - and that slot is reset to pending by the next proposal, which
// the supervisor's own reconcile appends as soon as an approval unblocks a
// decomposition. So an approval that HAD been applied was reported as "the
// decision was NOT applied": the exact inverse of the defect this path exists
// to prevent.
func TestLostReplyRecoveryReadsTheDecisionHistory(t *testing.T) {
	payload := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	events := []runtime.EngineeringEvent{
		{Type: runtime.EventPlanApproved, Payload: payload(runtime.PlanDecisionPayload{
			Revision: 2, Digest: "digest-2", Operator: "operator-1", Note: "the two-half split",
		})},
		// Durable state moves on: the approval unblocked a decomposition, and
		// the supervisor stored its proposal before this terminal reopened the
		// store.
		{Type: runtime.EventPlanProposed, Payload: payload(runtime.PlanDecisionPayload{
			Revision: 3, Digest: "digest-3",
		})},
	}

	decided, applied := decisionEventFor(events, "approve", 2, "digest-2")
	if !applied {
		t.Fatal("an approval that landed was reported as not applied because a later proposal arrived")
	}
	if decided.Revision != 2 || decided.Operator != "operator-1" || decided.Status != domain.ApprovalApproved {
		t.Fatalf("the recovered record is not the decision that was made: %#v", decided)
	}

	// A decision that did NOT land is still reported as not landed: the same
	// digest at a different revision, and the same revision at a different
	// digest, are different decisions.
	if _, applied := decisionEventFor(events, "approve", 3, "digest-3"); applied {
		t.Fatal("a proposal was read as an approval")
	}
	if _, applied := decisionEventFor(events, "approve", 2, "digest-other"); applied {
		t.Fatal("a different digest was accepted as this decision")
	}
	if _, applied := decisionEventFor(events, "reject", 2, "digest-2"); applied {
		t.Fatal("an approval answered a question about a rejection")
	}
	// An unreadable record is not this decision, and not a reason to claim one.
	broken := []runtime.EngineeringEvent{{Type: runtime.EventPlanApproved, Payload: json.RawMessage("[]")}}
	if _, applied := decisionEventFor(broken, "approve", 2, "digest-2"); applied {
		t.Fatal("an unreadable decision record was reported as this decision")
	}
}
