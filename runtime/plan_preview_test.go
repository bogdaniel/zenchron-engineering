package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// `plan show --revision N` is an approval PREVIEW, not the current revision's
// execution wearing another document's stage list.
//
// It loaded the exact revision N document and then resolved it against LIVE
// state. So a stage that revision 2 materially changes still rendered as
// "completed" by the worker that performed revision 1's version of it - work
// approving revision 2 would immediately invalidate and redo. An operator
// deciding on revision 2 was reading revision 1's execution.
func TestARevisionPreviewShowsWhatApprovingItWouldLeave(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "documentation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Write it down.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)
	fixture.reconcile(t)

	// Both stages have run under revision 1.
	governing, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"implementation", "documentation"} {
		if governing.Stages[id].RunID == "" {
			t.Fatalf("stage %s did not run under the governing revision", id)
		}
		settleRunAtGoalState(t, fixture, governing.Stages[id].RunID, "aaaaaaaaaaaa")
	}
	fixture.reconcile(t)
	governing, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if governing.Stages["implementation"].State != PlanStageCompleted {
		t.Fatalf("the fixture never completed a stage under revision 1: %#v", governing.Stages["implementation"])
	}

	// Revision 2 materially changes ONE of them and is stored, unapproved.
	next := fixture.plan
	next.Revision = 2
	next.Stages = []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work, differently.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "documentation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Write it down.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	}
	digest, err := next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	next.Digest = digest
	if _, err := fixture.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	// A revision is resolved against the contract it was planned under, so the
	// preview needs revision 2's contract stored exactly as a real proposal
	// stores it.
	if err := fixture.store.PutPlanContract(next.ID, next.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}

	view, err := fixture.service.ViewRevision(fixture.plan.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if view.Preview == nil {
		t.Fatal("a non-governing revision was not marked as a preview")
	}
	if view.Preview.GoverningRevision != 1 {
		t.Fatalf("the preview names revision %d as governing", view.Preview.GoverningRevision)
	}
	if state := view.Snapshot.Stages["implementation"].State; state == PlanStageCompleted {
		t.Fatal("a stage revision 2 changes was shown as completed by revision 1's worker")
	}
	if view.Snapshot.Stages["implementation"].RunID != "" {
		t.Fatal("a stage revision 2 changes was shown bound to revision 1's run")
	}
	if len(view.Preview.Invalidated) != 1 || view.Preview.Invalidated[0] != "implementation" {
		t.Fatalf("the preview does not name what approving would redo: %#v", view.Preview.Invalidated)
	}
	// The UNCHANGED stage keeps its work, because approving would keep it.
	if state := view.Snapshot.Stages["documentation"].State; state != PlanStageCompleted {
		t.Fatalf("an unchanged completed stage was shown as %q, so the preview throws away work approval would keep", state)
	}

	// And the governing view is unaffected: this is non-destructive.
	current, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Preview != nil {
		t.Fatal("the governing view was marked as a preview")
	}
	if current.Snapshot.Stages["implementation"].State != PlanStageCompleted {
		t.Fatal("previewing a revision changed what the governing view reports")
	}
}
