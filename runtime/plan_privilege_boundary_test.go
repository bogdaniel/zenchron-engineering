package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// The authority boundary has to find the performance it is comparing against,
// and that performance is not always recorded under the revision governing now.
//
// An assignment row is written under the revision that governed when the stage
// started. A revision that does not change a completed stage leaves its work -
// and its row - under the older revision, which is the ordinary outcome of
// propose, approve, propose, approve. Looking only under the current revision
// found nothing, and "nothing to compare" was read as "nothing changed": the
// re-performance was then frozen against whatever the registry resolves today,
// a different worker included, with no block and no approval.
func TestThePrivilegeBoundaryComparesAgainstAnEarlierRevisionsPerformance(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	stage := fixture.plan.Stages[0]
	performed := planFixtureAssignment(t, fixture.plan)
	if err := fixture.store.PutPlanAssignment(fixture.plan.ID, 1, 0, performed); err != nil {
		t.Fatal(err)
	}

	// The stage was performed under revision 1 and carried unchanged into the
	// approved revision 2, which is where its next generation runs.
	governing := fixture.plan
	governing.Revision = 2

	same := performed
	if block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, same); block != nil {
		t.Fatalf("the same obligation was refused: %s", block.Reason)
	}

	for _, change := range []struct {
		what  string
		apply func(*domain.AgentAssignment)
	}{
		{"worker", func(a *domain.AgentAssignment) { a.Agent.ID = "gemini" }},
		{"provider kind", func(a *domain.AgentAssignment) { a.Agent.ProviderKind = "gemini_cli" }},
		{"vendor family", func(a *domain.AgentAssignment) { a.Agent.VendorFamily = "google" }},
		{"model", func(a *domain.AgentAssignment) { a.Agent.Model = "some-other-model" }},
		{"profile", func(a *domain.AgentAssignment) { a.Profile.Digest = strings.Repeat("e", 64) }},
		{"trust mode", func(a *domain.AgentAssignment) {
			a.Agent.TrustMode = domain.TrustRequirementProtected
		}},
	} {
		next := performed
		change.apply(&next)
		block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, next)
		if block == nil {
			t.Fatalf("re-performing the stage would change its %s and nothing blocked", change.what)
		}
		if block.Kind != "authority" {
			t.Fatalf("a changed %s blocked as %q", change.what, block.Kind)
		}
		if !strings.Contains(block.Reason, "propose a revision") {
			t.Fatalf("the block does not say what to do: %s", block.Reason)
		}
		if !strings.Contains(block.Reason, change.what) {
			t.Fatalf("the block does not name what changed (%s): %s", change.what, block.Reason)
		}
	}

	// A stage with NO previous performance anywhere is a first generation
	// under a new identity, and there is nothing to renew: it is not blocked.
	other := domain.PlanStage{ID: "never-performed", Kind: domain.StageAgent, Role: domain.RoleImplementer}
	if block := fixture.reconciler.refusePrivilegeChange(governing, other, 1, performed); block != nil {
		t.Fatalf("a stage that was never performed was blocked: %s", block.Reason)
	}
}
