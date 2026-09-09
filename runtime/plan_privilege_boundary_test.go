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
	// The profile names its instruction packs and context policy by id; the
	// registry freezes their CONTENT digests here.
	performed.Profile.Instructions = []domain.PackRef{
		{ID: "security-review-core", Revision: "3", Digest: strings.Repeat("1", 64)},
	}
	performed.Profile.ContextPolicy = &domain.PackRef{
		ID: "reviewer-context", Revision: "2", Digest: strings.Repeat("2", 64),
	}
	performed.Budget = domain.StageBudget{MaxExecutionAttempts: 2, MaxProviderInvocations: 6, MaxWallSeconds: 900}
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
		// The pack was EDITED IN PLACE: same profile document, same id,
		// version and digest, different instructions. This is the reachable
		// path from operator configuration to what a worker is actually told.
		{"instruction packs", func(a *domain.AgentAssignment) {
			a.Profile.Instructions = []domain.PackRef{
				{ID: "security-review-core", Revision: "4", Digest: strings.Repeat("9", 64)},
			}
		}},
		{"context policy", func(a *domain.AgentAssignment) {
			a.Profile.ContextPolicy = &domain.PackRef{
				ID: "reviewer-context", Revision: "3", Digest: strings.Repeat("8", 64),
			}
		}},
		{"provider invocation ceiling", func(a *domain.AgentAssignment) {
			a.Budget.MaxProviderInvocations = 60
		}},
		{"wall clock ceiling", func(a *domain.AgentAssignment) { a.Budget.MaxWallSeconds = 0 }},
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

	// Everything the named rows do NOT enumerate, structurally. These are the
	// fields a future edit of that list would silently stop protecting, and the
	// whole point of comparing the canonical record is that they are covered
	// without anybody remembering them.
	for _, change := range []struct {
		what  string
		apply func(*domain.AgentAssignment)
	}{
		{"profile capabilities", func(a *domain.AgentAssignment) {
			a.Profile.Capabilities = []domain.EngineeringCapability{domain.CapabilityVerification}
		}},
		{"required capabilities", func(a *domain.AgentAssignment) {
			a.RequiredCapabilities = []domain.EngineeringCapability{domain.CapabilityVerification}
		}},
		{"contract", func(a *domain.AgentAssignment) {
			a.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}
		}},
		{"the context the worker is shown", func(a *domain.AgentAssignment) {
			a.Context.PolicyExcerpts = []string{"a policy excerpt nobody approved"}
		}},
	} {
		next := performed
		change.apply(&next)
		block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, next)
		if block == nil {
			t.Fatalf("re-performing the stage would change %s and nothing blocked", change.what)
		}
		if block.Kind != "authority" {
			t.Fatalf("a changed %s blocked as %q", change.what, block.Kind)
		}
	}

	// What a generation IS allowed to move: which performance it is, the run it
	// became, the resolver's explanation, the upstream candidate whose
	// replacement caused it - and a budget that NARROWS.
	renewed := performed
	renewed.ID = performed.ID + "-g1"
	renewed.RunID = "run-something-else"
	renewed.Selection = domain.ResolutionExplanation{
		Considered: []domain.CandidateEvaluation{}, Selected: "codex", Reason: "a differently worded explanation",
	}
	renewed.Context.UpstreamOutputs = []domain.UpstreamOutput{
		{StageID: "implementation", RunID: "run-b", Candidate: strings.Repeat("b", 40), Tree: strings.Repeat("b", 40)},
	}
	renewed.Budget = domain.StageBudget{MaxExecutionAttempts: 1, MaxProviderInvocations: 3, MaxWallSeconds: 600}
	if block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, renewed); block != nil {
		t.Fatalf("renewing the same obligation was refused: %s", block.Reason)
	}

	// And a stage being performed AGAIN whose previous performance cannot be
	// found FAILS CLOSED. A later generation exists because an earlier one
	// happened; not finding it proves nothing about whether the obligation is
	// the same, and reading absence as sameness is how this boundary would be
	// bypassed by deleting a row.
	other := domain.PlanStage{ID: "never-performed", Kind: domain.StageAgent, Role: domain.RoleImplementer}
	block := fixture.reconciler.refusePrivilegeChange(governing, other, 1, performed)
	if block == nil {
		t.Fatal("a re-performance with no recoverable predecessor was allowed to start")
	}
	if !strings.Contains(block.Reason, "propose a revision") {
		t.Fatalf("the block does not say what to do: %s", block.Reason)
	}
}
