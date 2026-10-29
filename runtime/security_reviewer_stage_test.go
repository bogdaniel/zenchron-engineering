package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestSecurityReviewerStageCompletesFromItsTypedVerdict(t *testing.T) {
	stages := closedLoopStages()
	stages[1].Role = domain.RoleSecurityReviewer
	fixture := newPlanRunFixture(t, stages)
	reviewer := reviewerEngine(t, fixture, ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
	})
	fixture.approve(t)
	fixture.reconcile(t)
	producer := planStageState(t, fixture, "implementation").RunID
	if producer == "" {
		t.Fatal("the producer stage created no run")
	}
	produceCandidate(t, fixture, producer, "package a\n", true)
	fixture.reconcile(t)
	reviewStage := planStageState(t, fixture, "review")
	if reviewStage.RunID == "" {
		t.Fatal("the security reviewer stage created no run")
	}
	driveReviewer(t, fixture, reviewStage.RunID)
	settleRunAtGoalState(t, fixture, reviewStage.RunID, "rrrrrrrrrrrr")
	fixture.reconcile(t)
	if len(reviewer.Reviewed) == 0 {
		t.Fatal("the security reviewer received no structured verdict path")
	}
	accepted := planStageState(t, fixture, "review")
	if accepted.Review == nil || accepted.Review.Verdict != StageReviewAccepted || accepted.State != PlanStageCompleted {
		t.Fatalf("the security reviewer verdict did not complete its stage: %+v", accepted)
	}
}
