package runtime

// A run that owes a human decision its answer has not settled on a head that
// dependent work may consume - whatever its stored reason says. Each clause of
// awaitsHumanDecision is proved against real store state, with the run row
// forced to the stale goal_state_reached a plan pass can observe between the
// question's admission and the run's own next reconcile.

import (
	"context"
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
