package planning_test

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// Resolution is deterministic and explainable. These tests assert on the
// EXPLANATION as much as on the selection, because "why did my reviewer not get
// this stage" is an operator question and an answer that exists only inside a
// resolver's control flow is not an answer.

func claudeAgent() domain.ExecutionAgentDescriptor {
	return domain.ExecutionAgentDescriptor{
		ID: "claude", ProviderKind: "claude_code", VendorFamily: "anthropic",
		TrustMode: domain.TrustRequirementOperatorTrusted, Model: "sonnet",
		Capabilities:    domain.EngineeringCapabilities(),
		InvocationModes: []domain.InvocationMode{domain.InvocationModeMutating, domain.InvocationModeNonMutatingPlanning},
		Available:       true, Unattended: true,
	}
}

func codexAgent() domain.ExecutionAgentDescriptor {
	return domain.ExecutionAgentDescriptor{
		ID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
		TrustMode:       domain.TrustRequirementOperatorTrusted,
		Capabilities:    domain.EngineeringCapabilities(),
		InvocationModes: []domain.InvocationMode{domain.InvocationModeMutating, domain.InvocationModeNonMutatingPlanning},
		Available:       true, Unattended: true,
	}
}

// Gemini is available and can edit, and it cannot prove a non-mutating mode.
func geminiAgent() domain.ExecutionAgentDescriptor {
	return domain.ExecutionAgentDescriptor{
		ID: "gemini", ProviderKind: "gemini_cli", VendorFamily: "google",
		TrustMode:       domain.TrustRequirementOperatorTrusted,
		Capabilities:    domain.EngineeringCapabilities(),
		InvocationModes: []domain.InvocationMode{domain.InvocationModeMutating},
		Available:       true, Unattended: true,
	}
}

func resolveInput(t *testing.T, plan domain.EngineeringPlan, agents ...domain.ExecutionAgentDescriptor) planning.ResolveInput {
	t.Helper()
	return planning.ResolveInput{
		Plan:     plan,
		Registry: planning.Registry{},
		Agents:   agents,
		Contract: contractFor(t, "security-sensitive.engineering-fact.json"),
		Model:    decodePlanningFixture[domain.ProjectModel](t, "security-sensitive.project-model.json"),
	}
}

// A security review that must be vendor-family independent of the material
// producer resolves onto a DIFFERENT vendor, and the assignment records the
// classes that were compared.
func TestIndependentReviewResolvesOntoADifferentVendorFamily(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	resolution, err := planning.Resolve(resolveInput(t, plan, claudeAgent(), codexAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Blocked) != 0 {
		t.Fatalf("two eligible vendors and the plan still blocked: %#v", resolution.Blocked)
	}
	implementation, ok := resolution.Assignment("implementation")
	if !ok {
		t.Fatalf("no implementation assignment: %#v", resolution.Assignments)
	}
	review, ok := resolution.Assignment("security-reviewer")
	if !ok {
		t.Fatalf("no security review assignment: %#v", resolution.Assignments)
	}
	if review.Agent.VendorFamily == implementation.Agent.VendorFamily {
		t.Fatalf("the review was assigned to the producer's vendor family %q", review.Agent.VendorFamily)
	}
	if len(review.Independence) != 1 {
		t.Fatalf("the assignment records no independence binding: %#v", review.Independence)
	}
	binding := review.Independence[0]
	if binding.Class != review.Agent.VendorFamily || len(binding.OtherClasses) == 0 {
		t.Fatalf("independence binding does not record what was compared: %#v", binding)
	}
	if binding.SatisfiedBy != domain.IndependenceSatisfiedByWorker {
		t.Fatalf("independence satisfied by %q", binding.SatisfiedBy)
	}
}

// With ONE eligible vendor and a vendor-family obligation, the stage BLOCKS.
// The same vendor is never assigned silently and the block names what would
// have to change.
func TestSingleVendorIndependenceShortageBlocksExplicitly(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	resolution, err := planning.Resolve(resolveInput(t, plan, claudeAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Blocked) != 1 {
		t.Fatalf("expected exactly one blocked stage, got %#v", resolution.Blocked)
	}
	blocked := resolution.Blocked[0]
	if blocked.Kind != planning.BlockIndependence {
		t.Fatalf("block kind = %q, want %q", blocked.Kind, planning.BlockIndependence)
	}
	if !strings.Contains(blocked.Reason, "independent") {
		t.Fatalf("block reason does not explain the shortage: %q", blocked.Reason)
	}
	if len(blocked.Explain.Considered) == 0 {
		t.Fatal("the block records no considered candidates, so an operator cannot see what was rejected")
	}
	// The producer stage still resolved: one blocked stage does not block the
	// plan's other work.
	if _, ok := resolution.Assignment("implementation"); !ok {
		t.Fatal("an independence shortage on the review stage blocked the implementation stage too")
	}
}

// A planner-role stage requires a provable non-mutating mode. A provider
// without one is INELIGIBLE and the resolver says so in those terms.
func TestPlannerStageRefusesAProviderWithoutANonMutatingMode(t *testing.T) {
	plan := compilePlan(t, planInput(t, "trivial.engineering-fact.json", nil))
	plan.Stages = append(plan.Stages, domain.PlanStage{
		ID: "decomposition", Kind: domain.StageAgent, Role: domain.RolePlanner,
		RequiresCapabilities: planning.RoleCapabilities(domain.RolePlanner),
		InvocationMode:       domain.InvocationModeNonMutatingPlanning,
	})
	resolution, err := planning.Resolve(resolveInput(t, plan, geminiAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Blocked) != 1 || resolution.Blocked[0].Kind != planning.BlockInvocationMode {
		t.Fatalf("expected an invocation-mode block, got %#v", resolution.Blocked)
	}
	if !strings.Contains(resolution.Blocked[0].Reason, "never degraded") {
		t.Fatalf("the refusal does not state that no permissive fallback exists: %q", resolution.Blocked[0].Reason)
	}

	// The same stage with a provider that CAN prove the mode resolves.
	resolved, err := planning.Resolve(resolveInput(t, plan, claudeAgent()))
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok := resolved.Assignment("decomposition")
	if !ok {
		t.Fatalf("a capable provider did not resolve the planner stage: %#v", resolved.Blocked)
	}
	if assignment.InvocationMode != domain.InvocationModeNonMutatingPlanning {
		t.Fatalf("planner assignment invocation mode = %q", assignment.InvocationMode)
	}
}

// One ExecutionAgent may back several profiles, and one role may resolve onto
// different profiles. The explanation states which and why.
func TestOneAgentBacksSeveralProfilesAndTheChoiceIsExplained(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "instructions/review.json", `{"instructions": ["Review the diff; do not implement."]}`)
	writeArtifact(t, dir, "profiles/zenchron-reviewer.json", `{
	  "execution_agent": "claude", "capabilities": ["repository_analysis", "security_review"],
	  "instructions": ["review"]
	}`)
	writeArtifact(t, dir, "profiles/zenchron-builder.json", `{
	  "execution_agent": "codex", "capabilities": ["repository_analysis", "code_change", "verification"]
	}`)
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}

	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	input := resolveInput(t, plan, claudeAgent(), codexAgent())
	input.Registry = registry
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	review, ok := resolution.Assignment("security-reviewer")
	if !ok {
		t.Fatalf("the review stage did not resolve: %#v", resolution.Blocked)
	}
	if review.Profile.ID != "zenchron-reviewer" {
		t.Fatalf("the review resolved onto profile %q", review.Profile.ID)
	}
	// The producer went to the OTHER operator profile, over the other worker:
	// one role resolves onto different profiles, and a specialized agent is
	// never reached through an implicit direct profile that would bypass the
	// operator's instructions.
	implementation, ok := resolution.Assignment("implementation")
	if !ok || implementation.Profile.ID != "zenchron-builder" {
		t.Fatalf("the implementation resolved onto %#v", implementation.Profile)
	}
	// The operator's instruction pack is frozen into the assignment by digest.
	if len(review.Profile.Instructions) != 1 || review.Profile.Instructions[0].Digest == "" {
		t.Fatalf("the assignment froze no instruction digest: %#v", review.Profile)
	}
	if review.Selection.Reason == "" || len(review.Selection.Considered) < 2 {
		t.Fatalf("selection is not explained: %#v", review.Selection)
	}
	rejected := false
	for _, candidate := range review.Selection.Considered {
		if !candidate.Eligible && len(candidate.Reasons) > 0 {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("no rejection reason was recorded for any candidate")
	}
}

// An unavailable worker is a distinct, actionable state: nothing is
// misconfigured and the block clears when the agent does.
func TestUnavailableWorkersBlockWithTheirOwnKind(t *testing.T) {
	agent := claudeAgent()
	agent.Available = false
	agent.Detail = "the CLI is not authenticated"
	plan := compilePlan(t, planInput(t, "trivial.engineering-fact.json", nil))
	resolution, err := planning.Resolve(resolveInput(t, plan, agent))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Blocked) != 1 || resolution.Blocked[0].Kind != planning.BlockUnavailable {
		t.Fatalf("expected an availability block, got %#v", resolution.Blocked)
	}
	if !strings.Contains(resolution.Blocked[0].Explain.Considered[0].Reasons[0], "not authenticated") {
		t.Fatalf("the readiness detail did not reach the explanation: %#v", resolution.Blocked[0].Explain)
	}
}

// Resolution is deterministic: the same plan and workforce resolve identically,
// including which profile won a tie.
func TestResolutionIsDeterministic(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	first, err := planning.Resolve(resolveInput(t, plan, claudeAgent(), codexAgent()))
	if err != nil {
		t.Fatal(err)
	}
	second, err := planning.Resolve(resolveInput(t, plan, codexAgent(), claudeAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Assignments) != len(second.Assignments) {
		t.Fatalf("assignment counts differ: %d vs %d", len(first.Assignments), len(second.Assignments))
	}
	for i := range first.Assignments {
		left, err := domain.Encode(first.Assignments[i])
		if err != nil {
			t.Fatal(err)
		}
		right, err := domain.Encode(second.Assignments[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Fatalf("assignment %d differs between resolutions:\n%s\n%s", i, left, right)
		}
	}
}

// A profile that would escalate its worker is ineligible everywhere, and the
// rejection says so rather than reporting a vague capability mismatch.
func TestAnEscalatingProfileIsIneligibleWithItsReason(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "profiles/protected-reviewer.json", `{
	  "execution_agent": "claude",
	  "capabilities": ["repository_analysis", "security_review"],
	  "trust_requirement": "protected"
	}`)
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	input := resolveInput(t, plan, claudeAgent(), codexAgent())
	input.Registry = registry
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	// The rejection is recorded wherever the stage settled: in the selection
	// explanation of an assignment, or in the explanation attached to a block.
	found := false
	explanations := []domain.ResolutionExplanation{}
	for _, assignment := range resolution.Assignments {
		explanations = append(explanations, assignment.Selection)
	}
	for _, blocked := range resolution.Blocked {
		explanations = append(explanations, blocked.Explain)
	}
	for _, explanation := range explanations {
		for _, candidate := range explanation.Considered {
			if candidate.Profile == "protected-reviewer" && !candidate.Eligible &&
				strings.Contains(strings.Join(candidate.Reasons, " "), "no configuration can raise it") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("an escalating profile was not recorded as ineligible with its reason: %#v", explanations)
	}
}

// The operator's answer to an independence shortage policy permits a person to
// fill. It converts the blocked stage into a human decision gate - which creates
// no run - and it refuses everywhere policy did not permit the substitution.
func TestHumanSubstitutionIsAvailableOnlyWherePolicyPermitsIt(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	stage, found := stageForRole(plan, domain.RoleSecurityReviewer)
	if !found {
		t.Fatal("the fixture plan has no security reviewer stage")
	}

	// Policy did not permit it here: the substitution is refused, with the
	// reason that the permission is not the operator's to grant.
	_, err := planning.SubstituteHumanReview(plan, stage.ID, []string{"claim-security-review"})
	if err == nil || !strings.Contains(err.Error(), "nothing else may grant it") {
		t.Fatalf("a substitution policy did not permit was accepted: %v", err)
	}

	// With the permission, the stage becomes a human decision gate that
	// references the claims a person answers, keeps the dependencies, and
	// carries no worker requirement at all.
	permitted := plan
	permitted.Stages = append([]domain.PlanStage{}, plan.Stages...)
	for i, existing := range permitted.Stages {
		if existing.ID != stage.ID {
			continue
		}
		independence := *existing.Independence
		independence.HumanSubstitutionPermitted = true
		permitted.Stages[i].Independence = &independence
	}
	stages, err := planning.SubstituteHumanReview(permitted, stage.ID, []string{"claim-security-review"})
	if err != nil {
		t.Fatalf("a policy-permitted substitution was refused: %v", err)
	}
	var substituted domain.PlanStage
	for _, candidate := range stages {
		if candidate.ID == stage.ID {
			substituted = candidate
		}
	}
	if substituted.Kind != domain.StageHumanDecisionGate {
		t.Fatalf("the substituted stage is a %s", substituted.Kind)
	}
	if substituted.Role != "" || substituted.Profile != "" || len(substituted.RequiresCapabilities) > 0 {
		t.Fatalf("the human decision gate kept worker requirements: %#v", substituted)
	}
	if !containsString(substituted.RequiredClaims, "claim-security-review") {
		t.Fatalf("the gate does not state what the person answers: %#v", substituted.RequiredClaims)
	}
	if len(substituted.DependsOn) != len(stage.DependsOn) {
		t.Fatalf("the substitution changed the graph: %#v vs %#v", substituted.DependsOn, stage.DependsOn)
	}
}

// The substitution is checked where it lands: at the revision boundary, and
// against the policy obligation it claims to answer. Turning an
// independence-carrying stage into a gate is the easiest way to escape the
// obligation, so a gate stands in for a role only when it SAYS it does and only
// where policy permitted a person to answer.
func TestARevisionCannotEscapeIndependenceByBecomingAGate(t *testing.T) {
	previous := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	stage, _ := stageForRole(previous, domain.RoleSecurityReviewer)

	// The policy fixture's own permission, which is what decides.
	permittedContract := contractFor(t, "security-sensitive.engineering-fact.json")
	permitRequirement := func(permit bool) domain.EngineeringWorkContract {
		contract := permittedContract
		requirements := *contract.PlanRequirements
		roles := append([]domain.RoleRequirement{}, requirements.Roles...)
		for i := range roles {
			if roles[i].Independence == nil {
				continue
			}
			independence := *roles[i].Independence
			independence.HumanSubstitutionPermitted = permit
			roles[i].Independence = &independence
		}
		requirements.Roles = roles
		contract.PlanRequirements = &requirements
		return contract
	}

	revise := func(marked, permitted bool) error {
		source := previous
		source.Stages = append([]domain.PlanStage{}, previous.Stages...)
		for i, existing := range source.Stages {
			if existing.ID != stage.ID || existing.Independence == nil {
				continue
			}
			independence := *existing.Independence
			independence.HumanSubstitutionPermitted = permitted
			source.Stages[i].Independence = &independence
		}
		stages := make([]domain.PlanStage, 0, len(source.Stages))
		for _, existing := range source.Stages {
			if existing.ID != stage.ID {
				stages = append(stages, existing)
				continue
			}
			gate := domain.PlanStage{
				ID: existing.ID, Kind: domain.StageHumanDecisionGate,
				DependsOn: existing.DependsOn, RequiredClaims: []string{"claim-security-review"},
			}
			if marked {
				gate.SubstitutesRole = existing.Role
			}
			stages = append(stages, gate)
		}
		next := source
		next.Revision = source.Revision + 1
		revision := source.Revision
		next.Provenance.PreviousRevision = &revision
		next.Stages = stages
		return planning.Validate(next, planning.ValidationInput{
			Contract: permitRequirement(permitted), Envelope: operatorEnvelope(), Previous: &source,
		})
	}

	// Unmarked: the role obligation is simply unfulfilled. A gate that does not
	// say what it stands in for stands in for nothing.
	if err := revise(false, true); err == nil || !strings.Contains(err.Error(), `requires role "security_reviewer"`) {
		t.Fatalf("an unmarked gate silently fulfilled a role obligation: %v", err)
	}
	// Marked, but policy did not permit a person to answer.
	if err := revise(true, false); err == nil || !strings.Contains(err.Error(), "cannot be escaped by changing what the stage is") {
		t.Fatalf("an unpermitted substitution passed revision validation: %v", err)
	}
	// Marked and permitted: the same obligation, answered by a person.
	if err := revise(true, true); err != nil {
		t.Fatalf("a policy-permitted substitution was refused at the revision boundary: %v", err)
	}
}

// A stage that has already run is NOT re-resolved. The work is executing under
// the configuration an operator approved, and a downstream independence
// obligation is about the worker that actually produced the change rather than
// whichever worker would be chosen for it today.
func TestAFrozenAssignmentIsUsedRatherThanRecomputed(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	frozen := domain.AgentAssignment{
		SchemaVersion: domain.SchemaVersion, ID: "assignment-frozen", StageID: "implementation",
		Role: domain.RoleImplementer,
		Profile: domain.ProfileBinding{
			ID: "retired-builder", Version: 1, Digest: strings.Repeat("b", 64),
			Capabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}, TrustRequirement: domain.TrustRequirementOperatorTrusted,
		},
		Agent: domain.AgentBinding{
			ID: "claude", ProviderKind: "claude_code", VendorFamily: "anthropic",
			TrustMode: domain.TrustRequirementOperatorTrusted,
		},
		InvocationMode: domain.InvocationModeMutating, TrustRequirement: domain.TrustRequirementOperatorTrusted,
		Contract:  domain.ObjectRevision{ID: "contract", Revision: "1"},
		Context:   domain.ContextPack{Objective: "the approved objective", AcceptanceCriteria: []string{"it works"}, Included: []domain.ContextClass{domain.ContextObjective}},
		Selection: domain.ResolutionExplanation{Reason: "recorded when the stage was created"},
	}
	input := resolveInput(t, plan, claudeAgent(), codexAgent())
	input.Frozen = map[string]domain.AgentAssignment{"implementation": frozen}

	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	implementation, ok := resolution.Assignment("implementation")
	if !ok || implementation.ID != "assignment-frozen" || implementation.Profile.ID != "retired-builder" {
		t.Fatalf("the frozen assignment was recomputed: %#v", implementation)
	}
	// And the downstream independence is judged against what ACTUALLY ran: the
	// frozen assignment used Claude, so the reviewer must not.
	review, ok := resolution.Assignment("security-reviewer")
	if !ok {
		t.Fatalf("the review stage did not resolve: %#v", resolution.Blocked)
	}
	if review.Agent.VendorFamily == "anthropic" {
		t.Fatalf("the review resolved onto the frozen producer's vendor family %q", review.Agent.VendorFamily)
	}
}

// An independence obligation is only PROVEN if the stage it names is already
// resolved when this stage resolves, and nothing but a dependency orders two
// stages. A review with no dependency on the producer used to resolve first,
// find nothing to compare, and be stamped as independently satisfied on the
// very same vendor.
func TestAnIndependenceObligationIsNeverProvenVacuously(t *testing.T) {
	// Alphabetically first, so a tie-break on id would resolve it before the
	// producer it must differ from.
	stages := []domain.PlanStage{
		{ID: "a-review", Kind: domain.StageAgent, Role: domain.RoleSecurityReviewer,
			Objective:            "Review the change independently.",
			RequiresCapabilities: planning.RoleCapabilities(domain.RoleSecurityReviewer),
			InvocationMode:       domain.InvocationModeMutating,
			Independence:         &domain.IndependenceRequirement{Dimension: domain.IndependenceVendorFamily, DifferentFrom: []string{"z-impl"}}},
		{ID: "z-impl", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Make the change.",
			RequiresCapabilities: planning.RoleCapabilities(domain.RoleImplementer),
			InvocationMode:       domain.InvocationModeMutating},
	}

	// The compiler states the ordering the obligation already meant.
	input := planInput(t, "security-sensitive.engineering-fact.json", nil)
	input.Proposed = stages
	plan, err := planning.Compile(input)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	review, ok := plan.Stage("a-review")
	if !ok {
		t.Fatal("the review stage did not survive compilation")
	}
	depends := false
	for _, dependency := range review.DependsOn {
		if dependency == "z-impl" {
			depends = true
		}
	}
	if !depends {
		t.Fatalf("the review does not depend on the stage it must differ from: %#v", review.DependsOn)
	}

	// And a plan that states the obligation WITHOUT the ordering is refused
	// rather than resolved vacuously.
	unordered := plan
	unordered.Stages = append([]domain.PlanStage(nil), plan.Stages...)
	for i, stage := range unordered.Stages {
		if stage.ID == "a-review" {
			stage.DependsOn = nil
			unordered.Stages[i] = stage
		}
	}
	if err := planning.Validate(unordered, planning.ValidationInput{
		Contract: contractFor(t, "security-sensitive.engineering-fact.json"),
	}); err == nil {
		t.Fatal("a plan whose reviewer could resolve before the work it judges was accepted")
	}

	// One vendor cannot satisfy it, and the block says so rather than passing.
	single, err := planning.Resolve(resolveInput(t, plan, claudeAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Blocked) == 0 {
		t.Fatalf("a single-vendor plan resolved an independence obligation: %#v", single.Assignments)
	}
}

// A profile's constraints are frozen INTO the assignment, narrowed only.
// They used to be accepted, digested, schema-validated and then ignored
// everywhere - a silent weakening of "narrow, never escalate".
func TestAProfilesConstraintsAreFrozenIntoTheAssignment(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "profiles/narrow.json", `{
	  "execution_agent": "codex", "capabilities": ["repository_analysis", "code_change", "verification"],
	  "constraints": {"max_wall_seconds": 300, "max_execution_attempts": 1, "deny_permission_bypass": true}
	}`)
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}

	plan := compilePlan(t, planInput(t, "trivial.engineering-fact.json", nil))
	stage := plan.Stages[0]
	input := resolveInput(t, plan, codexAgent())
	input.Registry = registry
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok := resolution.Assignment(stage.ID)
	if !ok {
		t.Fatalf("the stage did not resolve: %#v", resolution.Blocked)
	}
	if assignment.Profile.Constraints == nil || !assignment.Profile.Constraints.DenyPermissionBypass {
		t.Fatalf("the assignment does not carry the profile's refusal of the bypass: %#v", assignment.Profile.Constraints)
	}
	if assignment.Budget.MaxWallSeconds != 300 || assignment.Budget.MaxExecutionAttempts != 1 {
		t.Fatalf("the assignment budget is %#v, want it narrowed to the profile's 300 seconds and 1 attempt", assignment.Budget)
	}

	// A profile stating a LARGER bound than the stage narrows nothing.
	wider := t.TempDir()
	writeArtifact(t, wider, "profiles/wide.json", `{
	  "execution_agent": "codex", "capabilities": ["repository_analysis", "code_change", "verification"],
	  "constraints": {"max_wall_seconds": 1048576, "max_execution_attempts": 99}
	}`)
	wideRegistry, err := planning.LoadRegistry(wider)
	if err != nil {
		t.Fatal(err)
	}
	input.Registry = wideRegistry
	widened, err := planning.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	relaxed, ok := widened.Assignment(stage.ID)
	if !ok {
		t.Fatalf("the stage did not resolve: %#v", widened.Blocked)
	}
	if relaxed.Budget != stage.Budget {
		t.Fatalf("a profile widened the stage budget to %#v from %#v", relaxed.Budget, stage.Budget)
	}
}

// The revision ratchet compares more than matching stage ids. Deleting the
// stage that carried an obligation and adding a fresh one without it changed
// nothing the old comparison could see, so the easiest way to drop an
// independent review was to rename it.
func TestAnObligationCannotBeDroppedByRenamingItsStage(t *testing.T) {
	previous := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	reviewer, found := stageForRole(previous, domain.RoleSecurityReviewer)
	if !found || reviewer.Independence == nil {
		t.Fatalf("the fixture has no independent review stage to drop: %#v", previous.Stages)
	}

	renamed := previous
	renamed.Revision = previous.Revision + 1
	previousRevision := previous.Revision
	renamed.Provenance.PreviousRevision = &previousRevision
	renamed.Stages = nil
	for _, stage := range previous.Stages {
		if stage.ID == reviewer.ID {
			// Same role, new id, no independence: the rename escape.
			stage.ID = reviewer.ID + "-2"
			stage.Independence = nil
		}
		renamed.Stages = append(renamed.Stages, stage)
	}

	err := planning.Validate(renamed, planning.ValidationInput{
		Contract: contractFor(t, "security-sensitive.engineering-fact.json"),
		Previous: &previous,
	})
	if err == nil {
		t.Fatal("a revision dropped an independence obligation by renaming the stage that held it")
	}
	if !strings.Contains(err.Error(), "renaming") {
		t.Fatalf("the refusal does not name what happened: %v", err)
	}

	// Nor by replacing it with a human decision gate that policy never
	// permitted: the same escape, one step further round.
	gated := renamed
	gated.Stages = nil
	for _, stage := range previous.Stages {
		if stage.ID == reviewer.ID {
			stage = domain.PlanStage{
				ID: reviewer.ID + "-by-person", Kind: domain.StageHumanDecisionGate,
				DependsOn: stage.DependsOn, SubstitutesRole: reviewer.Role,
				RequiredClaims: []string{"claim-independent-review"},
			}
		}
		gated.Stages = append(gated.Stages, stage)
	}
	err = planning.Validate(gated, planning.ValidationInput{
		Contract: contractFor(t, "security-sensitive.engineering-fact.json"),
		Previous: &previous,
	})
	if err == nil {
		t.Fatal("an obligation was answered by a human gate policy never permitted")
	}
	// The reason matters: passing on some unrelated validation error would
	// leave the rule under test unexercised.
	if !strings.Contains(err.Error(), "policy did not permit that substitution") {
		t.Fatalf("the refusal is not the substitution rule: %v", err)
	}
}

// An artifact the plan NAMES and nobody installed is refused with the
// artifact's own name. A pinned profile that was never installed used to be
// reported as "no eligible profile" - true, and useless - and a missing
// context policy surfaced later as a hard error from the middle of resolution.
func TestAMissingOperatorArtifactIsNamed(t *testing.T) {
	plan := compilePlan(t, planInput(t, "trivial.engineering-fact.json", nil))
	pinned := plan
	pinned.Stages = append([]domain.PlanStage(nil), plan.Stages...)
	pinned.Stages[0].Profile = "never-installed"

	resolution, err := planning.Resolve(resolveInput(t, pinned, codexAgent()))
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Blocked) != 1 || resolution.Blocked[0].Kind != planning.BlockMissingArtifact {
		t.Fatalf("blocked = %#v, want one %q block", resolution.Blocked, planning.BlockMissingArtifact)
	}
	if !strings.Contains(resolution.Blocked[0].Reason, "never-installed") {
		t.Fatalf("the block does not name the artifact: %q", resolution.Blocked[0].Reason)
	}

	policy := plan
	policy.Stages = append([]domain.PlanStage(nil), plan.Stages...)
	policy.Stages[0].ContextPolicy = "absent-policy"
	blocked, err := planning.Resolve(resolveInput(t, policy, codexAgent()))
	if err != nil {
		t.Fatalf("a missing context policy failed resolution outright rather than blocking the stage: %v", err)
	}
	if len(blocked.Blocked) != 1 || !strings.Contains(blocked.Blocked[0].Reason, "absent-policy") {
		t.Fatalf("blocked = %#v, want the missing context policy named", blocked.Blocked)
	}
}
