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
