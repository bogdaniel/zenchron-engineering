package runtime

import (
	"context"
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
	// The discarded run is RETIRED: it does not stop existing because the plan
	// stopped looking at it, so it is attributed and stopped rather than left
	// executing work nobody will read.
	retired := false
	for _, runID := range settled.RetiredRuns {
		retired = retired || runID == review
	}
	if !retired {
		t.Fatalf("the invalidated stage's run was not retired: %#v", settled.RetiredRuns)
	}
	if settled.Stages["review"].InvalidatedUnder != fixture.plan.Revision {
		t.Fatalf("the invalidation does not record the revision it happened under: %#v", settled.Stages["review"])
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

// The invalidation reason is a JOURNAL FIELD, and production ids overflow it.
//
// Two full 40-hex candidate heads plus a stage id is 186 bytes before the id.
// The journal refuses a field above 200, so with ordinary stage ids the append
// was refused, the sweep errored, and the reconciler failed on that plan every
// tick, forever. The earlier test passed only because its heads were twelve
// characters long - a regression test that stops short of production sizes is
// how this ships green.
func TestTheInvalidationReasonFitsTheJournal(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation-core", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "independent-review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation-core"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation-core"].RunID

	// Real commit-sized heads, not twelve characters. The reason names both of
	// them and the upstream stage id: 76 + 40 + 12 + 19 + 18 + 40 = 205 bytes,
	// against a 200-byte field bound.
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	recordCandidateAndAssurance(t, fixture, implementation, first)
	settleRunAtGoalState(t, fixture, implementation, first)
	fixture.reconcile(t)

	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Stages["independent-review"].RunID
	if review == "" {
		t.Fatal("the review stage created no run")
	}
	recordCandidateAndAssurance(t, fixture, review, strings.Repeat("r", 40))
	settleRunAtGoalState(t, fixture, review, strings.Repeat("r", 40))
	fixture.reconcile(t)
	fixture.reconcile(t)

	recordCandidate(t, fixture, implementation, second)
	settleRunAtGoalState(t, fixture, implementation, second)

	// The whole assertion: this pass does not error.
	if _, err := fixture.reconciler.Reconcile(context.Background(), fixture.plan.ID); err != nil {
		t.Fatalf("the invalidation could not be journalled: %v", err)
	}
	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["independent-review"].State != PlanStageInvalidated {
		t.Fatalf("the review was not invalidated: %#v", after.Stages["independent-review"])
	}
	if reason := after.Stages["independent-review"].Reason; len(reason) > maxPayloadFieldBytes {
		t.Fatalf("the recorded reason is %d bytes", len(reason))
	}
}
