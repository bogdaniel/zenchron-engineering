package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A completed independent review is not reusable once the work it reviewed has
// moved.
//
// Reopening the gate above it is not enough. The shape is
// `implementation -> independent review -> gate`: the reviewer's frozen
// assignment names the exact candidate it consumed, and a producer run at
// goal_state_reached is not finished - reviewer feedback re-activates it and it
// produces a different candidate. The gate reopens because its proof names the
// producer's run; the REVIEW stayed completed, so the gate could be satisfied
// again by an independent review that was never performed on the work now
// being gated.
func TestACompletedReviewIsInvalidatedWhenTheWorkItReviewedMoves(t *testing.T) {
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
	assignment, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(assignment.Context.UpstreamOutputs) == 0 || assignment.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the review assignment does not name the candidate it consumed: %#v", assignment.Context.UpstreamOutputs)
	}

	recordCandidateAndAssurance(t, fixture, review, "rrrrrrrrrrrr")
	settleRunAtGoalState(t, fixture, review, "rrrrrrrrrrrr")
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

	// Feedback re-activates the producer, which moves to candidate B. The
	// review that was performed was about A.
	//
	// The run's own record moves with the journal, exactly as the runtime's
	// reconcile of that run does it - the event and the row are written by the
	// same pass, and the plan compares against the row the assignment froze.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")
	fixture.reconcile(t)

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].State == PlanStageCompleted {
		t.Fatal("the review stayed completed after the work it reviewed was replaced")
	}
	if after.Stages["assurance"].State == PlanStageSatisfied {
		t.Fatal("the gate stayed satisfied over a review that was never performed on the current work")
	}

	// It does not churn. A stage's run identity is fixed within a revision, so
	// performing it again under this one would adopt the very run whose work
	// was just marked unusable. The plan says so and stops, rather than
	// re-settling the same run every tick and spending the child-run envelope
	// on it.
	before := after.Consumed.ChildRuns
	var blocked *PlanStageBlock
	for i := 0; i < 3; i++ {
		report := fixture.reconcile(t)
		for index, block := range report.Blocked {
			if block.StageID == "review" {
				blocked = &report.Blocked[index]
			}
		}
	}
	if blocked == nil || !strings.Contains(blocked.Reason, "propose a revision") {
		t.Fatalf("the plan does not say how the work gets done again: %#v", blocked)
	}
	settled, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Stages["review"].State != PlanStageInvalidated {
		t.Fatalf("the invalidated stage moved on its own: %#v", settled.Stages["review"])
	}
	if settled.Consumed.ChildRuns != before {
		t.Fatalf("child runs went from %d to %d: the invalidation is looping", before, settled.Consumed.ChildRuns)
	}
	// The freeze that describes what WAS performed is untouched: it is the
	// record of a performance, and the performance happened.
	if frozen, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, "review"); err != nil {
		t.Fatal(err)
	} else if !found || frozen.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the record of what the review consumed was rewritten: %#v", frozen.Context.UpstreamOutputs)
	}
}

// settleRunAtGoalState parks a child run where a producer waits: finished with
// what it was asked to do, still live and still able to move.
func settleRunAtGoalState(t *testing.T, fixture *planRunFixture, runID, head string) {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	run.Disposition = Waiting
	run.Reason = "goal_state_reached"
	run.Candidate.Revision, run.Candidate.Tree = head, head
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
}
