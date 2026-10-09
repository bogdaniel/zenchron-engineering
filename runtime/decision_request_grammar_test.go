package runtime

// #508 P4c: the #473 decision_request grammar extension. A worker may
// prescribe the only outcome kind its own question accepts and, for
// selected_option, its own closed, exhaustive option set; #508's
// ResolveDecision enforces that shape authoritatively, and the bounded
// same-RunID continuation (#508 P4b) delivers the authorized answer exactly
// as it does any other resolution. These tests prove the COMPLETE lifecycle
// end to end, through the real worker-report and supervisor control paths -
// never a hand-built EngineeringMessage or DecisionRequestRef.

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestASelectedOptionDecisionRequestEndToEndLifecycle drives the complete
// decision-request -> authorized resolution -> bounded same-RunID
// continuation lifecycle for a worker-prescribed selected_option question:
// an out-of-set answer is refused authoritatively before anything is
// resolved; the in-set answer succeeds; and the decision-resumed
// continuation (#508 P4b) delivers exactly that resolution.
func TestASelectedOptionDecisionRequestEndToEndLifecycle(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	runID := view.Items[0].RunID

	document := messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindDecisionRequest, Purpose: "which approach?", Body: "two viable options",
		ExpectedOutcomeKind: orchestration.DecisionSelectedOption, PermittedOptions: []string{"approach-a", "approach-b"},
	})
	worker.say(runID, document, false)

	tick := func() {
		t.Helper()
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	var waiting EngineeringRun
	for range 40 {
		tick()
		waiting = decisionWaitRunRow(t, fixture, runID)
		if waiting.Reason == ReasonDecisionPending {
			break
		}
	}
	if waiting.Reason != ReasonDecisionPending {
		t.Fatal("test precondition violated: the run never reached the decision wait")
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected exactly one open decision request: open=%v err=%v", open, err)
	}
	if open[0].ExpectedOutcomeKind != orchestration.DecisionSelectedOption {
		t.Fatalf("expected_outcome_kind = %q", open[0].ExpectedOutcomeKind)
	}
	if got := open[0].PermittedOptions; len(got) != 2 || got[0] != "approach-a" || got[1] != "approach-b" {
		t.Fatalf("permitted_options = %+v", got)
	}

	// The requesting worker's own prescribed shape is enforced
	// authoritatively: an answer outside its permitted_options is refused,
	// and nothing is resolved.
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: "approach-c",
		Operator: "operator@example",
	}); err == nil {
		t.Fatal("expected an out-of-set answer to be refused")
	}
	if _, found, err := fixture.store.DecisionResolutionByRequestID(open[0].ID); err != nil || found {
		t.Fatalf("expected the refused attempt to resolve nothing: found=%t err=%v", found, err)
	}

	resolution, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: "approach-b",
		Operator: "operator@example",
	})
	if err != nil {
		t.Fatalf("expected the in-set answer to succeed, got %v", err)
	}
	if resolution.Resolution.Outcome.Value != "approach-b" {
		t.Fatalf("resolution outcome = %+v", resolution.Resolution.Outcome)
	}
	worker.say(runID, "", false)

	var settled EngineeringRun
	for range 20 {
		tick()
		settled = decisionWaitRunRow(t, fixture, runID)
		if settled.Reason != ReasonDecisionPending && settled.Disposition != Active {
			break
		}
	}
	resumed := decisionResumedOperations(t, fixture, runID)
	if len(resumed) != 1 || resumed[0].State != Succeeded {
		t.Fatalf("expected exactly one SUCCEEDED decision-resumed operation delivering the authorized answer, got %+v", resumed)
	}
	deliveries := 0
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range payload.Decisions {
			if decision.RequestID == open[0].ID {
				deliveries++
			}
		}
	}
	if deliveries == 0 {
		t.Fatal("expected the authorized selected_option resolution to be journalled as delivered")
	}
}

// TestASelectedOptionDecisionRequestWithoutPermittedOptionsNeverOpens is the
// malformed-input refusal, exercised through the REAL worker-report
// admission path: a worker's whole message report is refused, and no
// decision request ever opens, let alone one a worker could then answer
// with anything because its shape was left unconstrained by a defect
// instead of by intent.
func TestASelectedOptionDecisionRequestWithoutPermittedOptionsNeverOpens(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	runID := view.Items[0].RunID

	malformed := messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindDecisionRequest, Purpose: "which approach?", Body: "two viable options",
		ExpectedOutcomeKind: orchestration.DecisionSelectedOption,
	})
	worker.say(runID, malformed, false)

	for range 10 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("expected the malformed report to be refused as a whole, admitting nothing, got %+v", open)
	}
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run unreadable: found=%t err=%v", found, err)
	}
	if run.Reason == ReasonDecisionPending {
		t.Fatal("expected the run to never reach a decision wait for a request that was never admitted")
	}
}
