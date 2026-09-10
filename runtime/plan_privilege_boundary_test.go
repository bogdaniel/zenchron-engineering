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
//
// This exercises the comparison directly, field by field. The production path -
// approve, perform, approve again, renew - is proved separately in
// plan_cross_revision_renewal_test.go, and the two are not interchangeable:
// this one states WHICH differences are authority, that one states that a real
// renewal reaches the comparison in a shape it accepts.
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
	// approved revision 2, which is where its next generation runs. Revision 2
	// was planned against its OWN stored contract: same obligations, a new
	// revision string, because contracts are stored per plan revision.
	governing := fixture.plan
	governing.Revision = 2
	governing.Provenance.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}
	digest, err := governing.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	governing.Digest = digest
	secondContract := planFixtureContract(fixture.phase8Fixture)
	secondContract.Revision = "2"
	if err := fixture.store.PutPlanContract(governing.ID, governing.Revision, secondContract); err != nil {
		t.Fatal(err)
	}

	// The renewal as the resolver actually produces it under revision 2: a new
	// plan revision, a new plan digest and the revision-2 contract pointer.
	// Every one of those differs from what was performed, and NONE of them is
	// an authority change - which is the whole of #108 and #114.
	renewedUnderTwo := func() domain.AgentAssignment {
		next := performed
		next.Plan = domain.PlanRef{ID: governing.ID, Revision: governing.Revision, Digest: governing.Digest}
		next.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}
		return next
	}
	if block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, renewedUnderTwo()); block != nil {
		t.Fatalf("an unchanged stage was refused renewal across an approved revision: %s", block.Reason)
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
		// The contract this stage answers, by IDENTITY. The revision beside it
		// may advance with the approved plan; which contract it is may not.
		{"contract", func(a *domain.AgentAssignment) {
			a.Contract = domain.ObjectRevision{ID: "some-other-contract", Revision: "2"}
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
		next := renewedUnderTwo()
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
		{"the context the worker is shown", func(a *domain.AgentAssignment) {
			a.Context.PolicyExcerpts = []string{"a policy excerpt nobody approved"}
		}},
		// The plan's durable IDENTITY. Its revision and digest advance; which
		// plan this assignment belongs to does not.
		{"the plan", func(a *domain.AgentAssignment) { a.Plan.ID = "some-other-plan" }},
	} {
		next := renewedUnderTwo()
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
	renewed := renewedUnderTwo()
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
	block := fixture.reconciler.refusePrivilegeChange(governing, other, 1, renewedUnderTwo())
	if block == nil {
		t.Fatal("a re-performance with no recoverable predecessor was allowed to start")
	}
	if !strings.Contains(block.Reason, "propose a revision") {
		t.Fatalf("the block does not say what to do: %s", block.Reason)
	}
}

// The plan revision an assignment carries is APPROVED-PLAN PROVENANCE, and
// letting it advance is only safe while the direction of travel is stated.
//
// #108 asks for a renewal to cross into a later approved revision. What it does
// not ask for is a comparison against an assignment resolved under something
// else: erasing the revision from the structural comparison without these
// checks would accept a renewal resolved under an unapproved revision, or a
// "previous performance" recorded under a revision that has not executed yet.
func TestARenewalMustBeResolvedUnderTheExecutingRevision(t *testing.T) {
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
	governing := fixture.plan
	governing.Revision = 2
	secondContract := planFixtureContract(fixture.phase8Fixture)
	secondContract.Revision = "2"
	if err := fixture.store.PutPlanContract(governing.ID, governing.Revision, secondContract); err != nil {
		t.Fatal(err)
	}

	// A revision-1 assignment offered as the revision-2 renewal. This is the
	// exact shape a hand-copied fixture has, and the exact shape a stale
	// resolution would have.
	stale := performed
	block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, stale)
	if block == nil {
		t.Fatal("an assignment resolved under revision 1 was accepted as revision 2's renewal")
	}
	if block.Kind != "authority" || !strings.Contains(block.Reason, "revision 2 is executing") {
		t.Fatalf("the refusal does not name the mismatch: %q %s", block.Kind, block.Reason)
	}

	// A renewal naming plan CONTENT the executing revision does not have was
	// not resolved from the approved document, whatever revision number it
	// carries. The assignment it would freeze is a durable provenance claim,
	// and a false one is refused rather than written.
	forged := performed
	forged.Plan.Revision = 2
	forged.Plan.Digest = strings.Repeat("f", 64)
	forged.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}
	block = fixture.reconciler.refusePrivilegeChange(governing, stage, 1, forged)
	if block == nil {
		t.Fatal("a renewal naming plan content the executing revision does not have was accepted")
	}
	if block.Kind != "authority" || !strings.Contains(block.Reason, "plan content") {
		t.Fatalf("the refusal does not name the content mismatch: %q %s", block.Kind, block.Reason)
	}

	// And a predecessor whose DOCUMENT claims a revision later than the one
	// executing is not a performance this one renews. The row is found by its
	// storage key; what it says about itself is what the comparison then reads,
	// and a row that disagrees with its key proves nothing.
	drifted := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	ahead := planFixtureAssignment(t, drifted.plan)
	ahead.Plan.Revision = 3
	if err := drifted.store.PutPlanAssignment(drifted.plan.ID, 1, 0, ahead); err != nil {
		t.Fatal(err)
	}
	driftedGoverning := drifted.plan
	driftedGoverning.Revision = 2
	if err := drifted.store.PutPlanContract(driftedGoverning.ID, 2, secondContract); err != nil {
		t.Fatal(err)
	}
	next := planFixtureAssignment(t, drifted.plan)
	next.Plan.Revision = 2
	next.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}
	block = drifted.reconciler.refusePrivilegeChange(driftedGoverning, drifted.plan.Stages[0], 1, next)
	if block == nil {
		t.Fatal("a performance from a later revision was accepted as the one being renewed")
	}
	if block.Kind != "authority" || !strings.Contains(block.Reason, "later than the executing revision") {
		t.Fatalf("the refusal does not name the direction of travel: %q %s", block.Kind, block.Reason)
	}
}

// The contract POINTER may advance; the compiled OBLIGATIONS may not.
//
// #114: contracts are stored per plan revision, so a stage carried forward
// unchanged into revision 2 references a different contract revision while the
// document says the same thing. Erasing the pointer without comparing content
// would let a materially different contract execute unapproved; comparing the
// pointer refused renewals that changed nothing. Both halves are checked here,
// and the third case is what happens when neither can be established.
func TestRenewalComparesCompiledContractContentAndNotThePointer(t *testing.T) {
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
	governing := fixture.plan
	governing.Revision = 2
	renewal := performed
	renewal.Plan.Revision = 2
	renewal.Contract = domain.ObjectRevision{ID: "contract", Revision: "2"}

	// No contract stored for revision 2 at all: FAIL CLOSED, naming the
	// revision whose obligations could not be read.
	block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, renewal)
	if block == nil {
		t.Fatal("a renewal was permitted with no readable contract for the executing revision")
	}
	if block.Kind != "authority" || !strings.Contains(block.Reason, "contract revision 2") {
		t.Fatalf("the refusal does not name the unreadable contract: %q %s", block.Kind, block.Reason)
	}

	// Same obligations, new revision string. This is the legitimate case.
	carried := planFixtureContract(fixture.phase8Fixture)
	carried.Revision = "2"
	previous := "1"
	carried.Provenance.PreviousContractRevision = &previous
	if err := fixture.store.PutPlanContract(governing.ID, 2, carried); err != nil {
		t.Fatal(err)
	}
	if block := fixture.reconciler.refusePrivilegeChange(governing, stage, 1, renewal); block != nil {
		t.Fatalf("an identical compiled contract under a new revision refused the renewal: %s", block.Reason)
	}

	// Materially changed obligations under revision 3: refused, with the
	// authority remedy.
	third := fixture.plan
	third.Revision = 3
	changed := planFixtureContract(fixture.phase8Fixture)
	changed.Revision = "3"
	changed.Obligations = map[string]domain.Requirement{
		"new-obligation": {Statement: "Something nobody approved.", Material: true},
	}
	if err := fixture.store.PutPlanContract(third.ID, 3, changed); err != nil {
		t.Fatal(err)
	}
	underThree := performed
	underThree.Plan.Revision = 3
	underThree.Contract = domain.ObjectRevision{ID: "contract", Revision: "3"}
	block = fixture.reconciler.refusePrivilegeChange(third, stage, 1, underThree)
	if block == nil {
		t.Fatal("a materially changed compiled contract renewed automatically")
	}
	if block.Kind != "authority" {
		t.Fatalf("changed obligations blocked as %q", block.Kind)
	}
	if !strings.Contains(block.Reason, "propose a revision") || !strings.Contains(block.Reason, "different compiled obligations") {
		t.Fatalf("the refusal does not name the obligations: %s", block.Reason)
	}
}
