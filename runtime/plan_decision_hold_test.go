package runtime

// A run that owes a human decision its answer has not settled on a head that
// dependent work may consume - whatever its stored reason says. Each clause of
// awaitsHumanDecision is proved against real store state, with the run row
// forced to the stale goal_state_reached a plan pass can observe between the
// question's admission and the run's own next reconcile.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestARunOwingAHumanDecisionIsNeverSettledAcrossTheDecisionLifecycle(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	runID := fixture.orchestrate(supervisor, "claude", []int{fleetFirstIssue}).Items[0].RunID
	worker.say(runID, decisionWaitDocument(t), false)
	reconciler := PlanReconciler{Store: fixture.store, StateDir: fixture.stateDir}
	tick := func() {
		t.Helper()
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	// What a plan pass can see between admission and the run's next reconcile.
	stale := func() EngineeringRun {
		t.Helper()
		run := decisionWaitRunRow(t, fixture, runID)
		run.Disposition, run.Reason = Waiting, ReasonGoalStateReached
		return run
	}
	awaiting := func() bool {
		t.Helper()
		got, err := reconciler.awaitsHumanDecision(stale())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	var open []orchestration.EngineeringMessage
	for range 40 {
		tick()
		if open, _ = fixture.store.OpenDecisionRequestsForRun(runID); len(open) == 1 {
			break
		}
	}
	if len(open) != 1 {
		t.Fatal("the question was never admitted")
	}
	if !awaiting() {
		t.Fatal("an admitted, open question did not hold the run")
	}
	if _, settled, _ := reconciler.settledRun(stale()); settled {
		t.Fatal("a run with an open question counted as settled for dependent work")
	}

	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionText, DecisionOutcomeValue: "approach B", Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}
	if !awaiting() {
		t.Fatal("a resolved but undelivered decision did not hold the run")
	}

	worker.say(runID, "", false)
	for range 20 {
		tick()
		if len(decisionResumedOperations(t, fixture, runID)) == 1 && !awaiting() {
			break
		}
	}
	if awaiting() {
		t.Fatal("the run is still held after its decision was delivered")
	}
}

// An observed report is not permission to settle until the admission owner has
// durably decided it - including when the journalled document has since become
// unavailable, which admission refuses rather than admits.
func TestAnUndecidedObservationWhoseReportIsGoneHoldsTheRunUntilAdmissionRefusesIt(t *testing.T) {
	fixture, _ := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	runID := fixture.orchestrate(supervisor, "claude", []int{fleetFirstIssue}).Items[0].RunID
	reconciler := PlanReconciler{Store: fixture.store, StateDir: fixture.stateDir}

	// The harness's own second invocation: the report is written to the
	// attempt's slot and its observation journalled, with no admission pass.
	const operationID = "op-undecided"
	simulateInvocation(t, fixture, runID, operationID, decisionWaitDocument(t))
	observed := MessagesObservedPayload{OperationID: operationID, Attempt: 1}
	path, err := MessageReportPath(fixture.stateDir, ExecutionAttemptRef{RunID: runID, OperationID: observed.OperationID, Attempt: observed.Attempt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stale := decisionWaitRunRow(t, fixture, runID)
	stale.Disposition, stale.Reason = Waiting, ReasonGoalStateReached
	if held, err := reconciler.awaitsHumanDecision(stale); err != nil || !held {
		t.Fatalf("an undecided observation with an unavailable report did not hold the run: held=%t err=%v", held, err)
	}
	if _, settled, _ := reconciler.settledRun(stale); settled {
		t.Fatal("dependent work could start against a run whose observed report is undecided")
	}

	// The admission owner decides it - a durable refusal - and only then is the run released.
	for range 5 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	decided, err := fixture.store.DecidedMessageInvocations(runID)
	if err != nil || !decided[messageInvocationKey(runID, observed.OperationID, observed.Attempt)] {
		t.Fatalf("admission never decided the observation: %v %v", decided, err)
	}
	if held, err := reconciler.awaitsHumanDecision(stale); err != nil || held {
		t.Fatalf("a durably refused observation still holds the run: held=%t err=%v", held, err)
	}
}
