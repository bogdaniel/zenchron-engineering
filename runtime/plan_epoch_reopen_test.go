package runtime

import (
	"testing"
)

func TestAChargedEpochReopensAFailedStageAndOnlyAFailedOne(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	fixture.approve(t)
	fixture.reconcile(t)
	stageState := func() PlanStageProjection {
		t.Helper()
		snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.Stages["backend"]
	}
	original := stageState().RunID
	if original == "" {
		t.Fatal("the stage never started")
	}
	record := func(eventType string, payload any) {
		t.Helper()
		if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID, eventType, payload); err != nil {
			t.Fatal(err)
		}
	}
	charge := PlanBudgetConsumedPayload{Key: "child_run:run-epoch-2", StageID: "backend", RunID: "run-epoch-2", ChildRuns: 1}

	// A charge to a stage that is still running changes nothing.
	record(EventPlanBudgetConsumed, PlanBudgetConsumedPayload{Key: "child_run:early", StageID: "backend", RunID: "run-early", ChildRuns: 1})
	if got := stageState(); got.State != PlanStageRunning || got.RunID != original {
		t.Fatalf("a charge to a running stage moved it: %+v", got)
	}

	record(EventPlanStageSettled, PlanStageSettledPayload{StageID: "backend", Outcome: "failed", Reason: "run_assurance_budget_exhausted"})
	if got := stageState(); got.State != PlanStageFailed {
		t.Fatalf("the stage is %s after its failure was settled", got.State)
	}
	// Spend that is not a new child run (an invocation attributed to the failed
	// run) does not reopen it.
	record(EventPlanBudgetConsumed, PlanBudgetConsumedPayload{Key: "invoke:1", StageID: "backend", RunID: original, ProviderInvocations: 1})
	if got := stageState(); got.State != PlanStageFailed {
		t.Fatalf("attributed spend reopened a failed stage: %+v", got)
	}

	record(EventPlanBudgetConsumed, charge)
	if got := stageState(); got.State != PlanStageRunning || got.RunID != "run-epoch-2" || got.Reason != "" {
		t.Fatalf("an authorized epoch's charge did not reopen the failed stage: %+v", got)
	}
	// Replaying the same charge is not a second reopening.
	record(EventPlanBudgetConsumed, charge)
	if got := stageState(); got.RunID != "run-epoch-2" {
		t.Fatalf("a repeated charge moved the stage: %+v", got)
	}

	// The new run's own failure ends the stage again, and stays ended.
	record(EventPlanStageSettled, PlanStageSettledPayload{StageID: "backend", Outcome: "failed", Reason: "again"})
	if got := stageState(); got.State != PlanStageFailed || got.RunID != "run-epoch-2" {
		t.Fatalf("the epoch run's failure did not settle the stage: %+v", got)
	}

	// A COMPLETED stage is never reopened by a charge.
	record(EventPlanBudgetConsumed, PlanBudgetConsumedPayload{Key: "child_run:run-epoch-3", StageID: "backend", RunID: "run-epoch-3", ChildRuns: 1})
	record(EventPlanStageSettled, PlanStageSettledPayload{StageID: "backend", Outcome: "completed"})
	record(EventPlanBudgetConsumed, PlanBudgetConsumedPayload{Key: "child_run:run-epoch-4", StageID: "backend", RunID: "run-epoch-4", ChildRuns: 1})
	if got := stageState(); got.State != PlanStageCompleted || got.RunID != "run-epoch-3" {
		t.Fatalf("a charge reopened a completed stage: %+v", got)
	}
}
