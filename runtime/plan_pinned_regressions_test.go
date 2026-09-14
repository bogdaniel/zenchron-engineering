package runtime

import (
	"context"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"strings"
	"testing"
)

func restartPinnedFixture(t *testing.T, f *planRunFixture) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.service.Store = reopened
	f.reconciler.Store = reopened
	f.reconciler.Service = f.service
}

func TestMissingFrozenUpstreamBlocksAfterRestart(t *testing.T) {
	f := newPlanRunFixture(t, orphanStages())
	f.approve(t)
	snapshot := mustReplayApproval(t, f)
	resolution, err := f.service.Resolve(f.plan, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok := resolution.Assignment("implementation")
	if !ok {
		t.Fatal("missing assignment")
	}
	assignment.Context.UpstreamOutputs = []domain.UpstreamOutput{{StageID: "missing-producer", RunID: "missing-run", Candidate: "aaaaaaaaaaaa"}}
	if err := f.store.PutPlanAssignment(f.plan.ID, f.plan.Revision, 0, assignment); err != nil {
		t.Fatal(err)
	}
	restartPinnedFixture(t, f)
	for i := 0; i < 2; i++ {
		started, block, err := f.reconciler.startAgentStage(context.Background(), f.plan, f.plan.Stages[0], snapshot, resolution)
		if err != nil {
			t.Fatal(err)
		}
		if started != nil || block == nil || block.Kind != "upstream" || !strings.Contains(block.Reason, "missing-run") {
			t.Fatalf("missing frozen run did not block: %#v %#v", started, block)
		}
	}
	if len(f.engineCalls) != 0 || mustReplayApproval(t, f).Consumed.ChildRuns != 0 {
		t.Fatal("missing input created or charged a child run")
	}
}

func TestApprovedUnstartedUnboundSurvivesRestart(t *testing.T) {
	f := newPlanRunFixture(t, orphanStages())
	f.service.Agents = nil
	f.approve(t)
	rows, err := f.store.ApprovedAssignments(f.plan.ID, f.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("blocked stages unexpectedly bound at approval")
	}
	f.service.Agents = planAgents()
	restartPinnedFixture(t, f)
	view, err := f.service.View(f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Assigned) == 0 || len(view.Unbound) != 2 {
		t.Fatalf("live assignments hid absence of durable approval binding: %#v", view.Unbound)
	}
}

func TestFailedReplacementPreservesCompletedDependentVerdict(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
		{ID: "assurance", Kind: domain.StageAssuranceGate, DependsOn: []string{"review"},
			RequiredClaims: []string{"verification"}},
	})
	fixture.approve(t)
	fixture.reconcile(t)

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}

	// Candidate A, verified, and the producer parks at goal state - which is
	// not "finished".
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")
	fixture.reconcile(t)

	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Stages["review"].RunID
	if review == "" {
		t.Fatal("the review stage created no run against candidate A")
	}
	// The reviewer's frozen assignment must actually name what it consumed, or
	// this test is asserting nothing.
	assignment, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(assignment.Context.UpstreamOutputs) == 0 || assignment.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the review assignment does not name the candidate it consumed: %#v", assignment.Context.UpstreamOutputs)
	}

	recordCandidateAndAssurance(t, fixture, review, "rrrrrrrrrrrr")
	settleRunAtGoalState(t, fixture, review, "rrrrrrrrrrrr")
	acceptReview(t, fixture, "review", review)
	fixture.reconcile(t)
	fixture.reconcile(t)

	satisfied, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if satisfied.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the review did not complete against candidate A: %#v", satisfied.Stages["review"])
	}
	if satisfied.Stages["assurance"].State != PlanStageSatisfied {
		t.Fatalf("the gate was not satisfied: %#v", satisfied.Stages["assurance"])
	}

	reactivateRunAtHead(t, fixture, implementation, "bbbbbbbbbbbb")
	run, _, err := fixture.store.Run(implementation)
	if err != nil {
		t.Fatal(err)
	}
	run.Disposition = Failed
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	restartPinnedFixture(t, fixture)
	report := fixture.reconcile(t)
	after := mustReplayApproval(t, fixture)
	if after.Stages["review"].State != PlanStageCompleted || after.Stages["review"].Generation != 0 {
		t.Fatalf("failed replacement discarded the completed verdict: %#v", after.Stages["review"])
	}
	if after.Stages["implementation"].State != PlanStageFailed {
		t.Fatalf("failed dependency hidden: %#v (%#v)", after, report)
	}
}

func TestGateInvalidationReplaysWithoutExecutionGeneration(t *testing.T) {
	f := newPlanRunFixture(t, []domain.PlanStage{{ID: "gate", Kind: domain.StageHumanDecisionGate}})
	f.approve(t)
	for i := 0; i < 2; i++ {
		if err := f.service.appendPlanEvent(f.plan.ID, EventPlanGateSatisfied, PlanGateSatisfiedPayload{StageID: "gate", Kind: string(domain.StageHumanDecisionGate), HumanEvidenceID: "human-proof"}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.appendPlanEvent(f.plan.ID, EventPlanStageSettled, PlanStageSettledPayload{StageID: "gate", Outcome: planStageInvalidated, Revision: f.plan.Revision, Reason: "upstream changed"}); err != nil {
			t.Fatal(err)
		}
		restartPinnedFixture(t, f)
		gate := mustReplayApproval(t, f).Stages["gate"]
		if gate.Generation != 0 || gate.State != PlanStageInvalidated || gate.Gate != nil || gate.RunID != "" {
			t.Fatalf("gate acquired execution state or retained proof: %#v", gate)
		}
	}
}
