package planning_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

// The deterministic planner, with no model anywhere.
//
// Every #64 compilation law is testable here because the reasoning planner is
// deliberately built AFTER this: a proposal from a live agent is validated by
// exactly the code these tests drive.

// A trivial change compiles to the work and nothing else. An architect stage
// nobody asked for is the rigid workflow #64 exists to avoid.
func TestTrivialChangeCompilesWithoutCeremony(t *testing.T) {
	plan := compilePlan(t, planInput(t, "trivial.engineering-fact.json", nil))

	roles := agentRoles(plan)
	if len(roles) != 1 || roles[0] != domain.RoleImplementer {
		t.Fatalf("a trivial change compiled to roles %v", roles)
	}
	for _, stage := range plan.Stages {
		if stage.Role == domain.RoleSecurityReviewer || stage.Role == domain.RoleSystemArchitect {
			t.Fatalf("a trivial change gained stage %q for role %q", stage.ID, stage.Role)
		}
	}
	// The gate references the claims the contract already requires and creates
	// no run of its own.
	gate, found := stageOfKind(plan, domain.StageAssuranceGate)
	if !found {
		t.Fatal("a trivial change compiled with no assurance gate at all")
	}
	if len(gate.RequiredClaims) == 0 {
		t.Fatal("the assurance gate references no claim, so nothing could satisfy it")
	}
	if gate.Role != "" {
		t.Fatalf("the gate named role %q: only agent stages become EngineeringRuns", gate.Role)
	}
}

// A security-sensitive change gains the obligations POLICY states, not
// obligations the planner invented.
func TestSecuritySensitiveChangeGainsPolicyObligations(t *testing.T) {
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))

	reviewer, found := stageForRole(plan, domain.RoleSecurityReviewer)
	if !found {
		t.Fatalf("policy required a security reviewer and the plan has roles %v", agentRoles(plan))
	}
	if reviewer.Independence == nil || reviewer.Independence.Dimension != domain.IndependenceVendorFamily {
		t.Fatalf("security review independence = %#v, want vendor_family", reviewer.Independence)
	}
	// The obligation policy states as a RELATIONSHIP is bound to the exact
	// stages that produce material change, which is what makes it checkable.
	if !containsString(reviewer.Independence.DifferentFrom, "implementation") {
		t.Fatalf("independence is not bound to the material producer: %#v", reviewer.Independence)
	}
	if !containsCapability(reviewer.RequiresCapabilities, domain.CapabilitySecurityReview) {
		t.Fatalf("security reviewer capabilities = %v", reviewer.RequiresCapabilities)
	}
	if _, ok := stageOfKind(plan, domain.StageHumanDecisionGate); !ok {
		t.Fatal("policy required a human decision gate and the plan has none")
	}
}

// A template contributes shape. Policy adds what the template omitted, and the
// template cannot remove it.
func TestPolicyAddsObligationsATemplateOmitted(t *testing.T) {
	template := featureTemplate(t)
	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", &template))

	if _, found := stageForRole(plan, domain.RoleSystemArchitect); !found {
		t.Fatal("the template's architecture stage did not reach the plan")
	}
	reviewer, found := stageForRole(plan, domain.RoleSecurityReviewer)
	if !found {
		t.Fatalf("policy's security reviewer was lost when a template was supplied; roles = %v", agentRoles(plan))
	}
	if reviewer.Independence == nil {
		t.Fatal("policy's independence obligation was lost when a template was supplied")
	}
	if plan.Provenance.Template == nil || plan.Provenance.Template.Digest != template.Digest {
		t.Fatalf("plan provenance does not pin the exact template: %#v", plan.Provenance.Template)
	}
}

// A large feature decomposes into a graph with parallel implementation stages,
// and the compiler orders it deterministically.
func TestParallelImplementationStagesCompileIntoOneGraph(t *testing.T) {
	input := planInput(t, "normal-behavior.engineering-fact.json", nil)
	input.Proposed = []domain.PlanStage{
		{ID: "architecture", Kind: domain.StageAgent, Role: domain.RoleSystemArchitect},
		{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"architecture"}},
		{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"architecture"}},
		{ID: "integration", Kind: domain.StageAgent, Role: domain.RoleIntegrator, DependsOn: []string{"backend", "frontend"}},
	}
	plan := compilePlan(t, input)

	backend, _ := stageByID(plan, "backend")
	frontend, _ := stageByID(plan, "frontend")
	if len(backend.DependsOn) != 1 || len(frontend.DependsOn) != 1 {
		t.Fatalf("the two implementation stages are not siblings: %v / %v", backend.DependsOn, frontend.DependsOn)
	}
	if backend.DependsOn[0] != "architecture" || frontend.DependsOn[0] != "architecture" {
		t.Fatal("the parallel stages do not both depend on the architecture stage")
	}
	// Dependency order, deterministically: architecture before its dependents,
	// integration last.
	order := stageOrder(plan)
	if order["architecture"] > order["backend"] || order["architecture"] > order["frontend"] {
		t.Fatalf("stages are not in dependency order: %v", order)
	}
	if order["integration"] < order["backend"] || order["integration"] < order["frontend"] {
		t.Fatalf("the integration stage is ordered before the work it joins: %v", order)
	}
}

// The same inputs compile to the same document, byte for byte. A plan is
// content-addressed, an assignment pins that digest, and an operator approves
// what they read.
func TestCompilationIsDeterministic(t *testing.T) {
	first := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	second := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))
	if first.Digest != second.Digest {
		t.Fatalf("two compilations of one input produced %s and %s", first.Digest[:12], second.Digest[:12])
	}
	firstJSON, err := domain.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := domain.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("canonical documents differ:\n%s\n%s", firstJSON, secondJSON)
	}
}

// A reasoning agent's proposal is a PROPOSAL. One that drops a compiled policy
// obligation is refused, whatever recommended it.
func TestProposalsThatWeakenPolicyAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		stages   []domain.PlanStage
		expected string
	}{
		{
			name: "drops the required security review",
			stages: []domain.PlanStage{
				{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer},
			},
			expected: `requires role "security_reviewer"`,
		},
		{
			name: "degrades the required independence class",
			stages: []domain.PlanStage{
				{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer},
				{ID: "security-review", Kind: domain.StageAgent, Role: domain.RoleSecurityReviewer,
					DependsOn:            []string{"implementation"},
					RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilitySecurityReview},
					Independence: &domain.IndependenceRequirement{
						Dimension: domain.IndependenceAgentProfile, DifferentFrom: []string{"implementation"},
					}},
			},
			expected: "where policy requires",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := planInput(t, "security-sensitive.engineering-fact.json", nil)
			// The proposal is taken as the base stage set and then checked. The
			// compiler will ADD what policy requires; to prove the refusal the
			// stage set is validated as proposed.
			plan := domain.EngineeringPlan{
				SchemaVersion: domain.SchemaVersion, ID: input.PlanID, Revision: 1,
				Objective: input.Objective, Subject: input.Subject,
				BudgetEnvelope: input.Envelope, Stages: tc.stages,
				Provenance: domain.PlanProvenance{
					CompilerVersion: planning.CompilerVersion,
					Contract:        domain.ObjectRevision{ID: input.Contract.ID, Revision: input.Contract.Revision},
				},
			}
			err := planning.Validate(plan, planning.ValidationInput{Contract: input.Contract, Envelope: input.Envelope})
			if err == nil || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("expected a refusal containing %q, got %v", tc.expected, err)
			}
		})
	}
}

// Nested plans are not representable and cycles are refused before approval.
func TestGraphRefusalsAreDeterministic(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = []domain.PlanStage{
		{ID: "a", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"b"}},
		{ID: "b", Kind: domain.StageAgent, Role: domain.RoleReviewer, DependsOn: []string{"a"}},
	}
	if _, err := planning.Compile(input); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle refusal, got %v", err)
	}

	dangling := planInput(t, "trivial.engineering-fact.json", nil)
	dangling.Proposed = []domain.PlanStage{
		{ID: "a", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"ghost"}},
	}
	if _, err := planning.Compile(dangling); err == nil || !strings.Contains(err.Error(), "not a stage in this plan") {
		t.Fatalf("expected a dangling-dependency refusal, got %v", err)
	}
}

// The aggregate envelope is the operator's, and a template may only tighten it.
func TestTemplateMayTightenTheEnvelopeAndNeverWidenIt(t *testing.T) {
	template := featureTemplate(t)
	template.BudgetEnvelope = &domain.PlanBudgetEnvelope{
		MaxChildRuns: 99, MaxConcurrency: 1, MaxProviderInvocations: 99,
	}
	digest, err := template.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	template.Digest = digest

	plan := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", &template))
	if plan.BudgetEnvelope.MaxConcurrency != 1 {
		t.Fatalf("the template's tighter concurrency was ignored: %#v", plan.BudgetEnvelope)
	}
	if plan.BudgetEnvelope.MaxChildRuns != operatorEnvelope().MaxChildRuns {
		t.Fatalf("the template widened max_child_runs to %d", plan.BudgetEnvelope.MaxChildRuns)
	}
}

// A plan that cannot fit inside the envelope is refused at compilation, where
// an operator can still act on it, rather than stalling mid-execution.
func TestAPlanLargerThanItsEnvelopeIsRefused(t *testing.T) {
	input := planInput(t, "normal-behavior.engineering-fact.json", nil)
	input.Envelope = domain.PlanBudgetEnvelope{MaxChildRuns: 1, MaxConcurrency: 1, MaxProviderInvocations: 1}
	input.Proposed = []domain.PlanStage{
		{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer},
		{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer},
	}
	if _, err := planning.Compile(input); err == nil || !strings.Contains(err.Error(), "child runs") {
		t.Fatalf("expected a budget refusal, got %v", err)
	}
}

// A revision may tighten and may never widen privilege, and it must record the
// revision it replaces.
func TestRevisionsCannotWidenPrivilege(t *testing.T) {
	previous := compilePlan(t, planInput(t, "security-sensitive.engineering-fact.json", nil))

	weakened := previous
	weakened.Revision = previous.Revision + 1
	next := previous.Revision
	weakened.Provenance.PreviousRevision = &next
	weakened.Stages = append([]domain.PlanStage{}, previous.Stages...)
	for i, stage := range weakened.Stages {
		if stage.Role == domain.RoleSecurityReviewer && stage.Independence != nil {
			independence := *stage.Independence
			independence.Dimension = domain.IndependenceAgentProfile
			weakened.Stages[i].Independence = &independence
		}
	}
	err := planning.Validate(weakened, planning.ValidationInput{
		Contract: contractFor(t, "security-sensitive.engineering-fact.json"),
		Envelope: operatorEnvelope(), Previous: &previous,
	})
	if err == nil || !strings.Contains(err.Error(), "weakens independence") {
		t.Fatalf("expected a privilege refusal, got %v", err)
	}
}

// A revision may not shrink the envelope below what has already been spent:
// that would be a plan already past its own ceiling, which is how a reset gets
// mistaken for a limit.
func TestRevisionsCannotClaimBackConsumedBudget(t *testing.T) {
	plan := compilePlan(t, planInput(t, "normal-behavior.engineering-fact.json", nil))
	err := planning.Validate(plan, planning.ValidationInput{
		Contract: contractFor(t, "normal-behavior.engineering-fact.json"),
		Envelope: operatorEnvelope(),
		Consumed: domain.PlanConsumption{ChildRuns: plan.BudgetEnvelope.MaxChildRuns + 1},
	})
	if err == nil || !strings.Contains(err.Error(), "already been created") {
		t.Fatalf("expected a consumption refusal, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func operatorEnvelope() domain.PlanBudgetEnvelope {
	return domain.PlanBudgetEnvelope{
		MaxChildRuns: 8, MaxConcurrency: 3, MaxProviderInvocations: 24, MaxWallSeconds: 21600,
	}
}

// planInput builds a compile input from the SAME fixtures the policy compiler
// is tested against, through the SAME policy compiler. That is the point: the
// obligations a plan must satisfy arrive by the one governance path.
func planInput(t *testing.T, factFixture string, template *domain.EngineeringPlanTemplate) planning.CompileInput {
	t.Helper()
	contract := contractFor(t, factFixture)
	fact := decodePlanningFixture[domain.EngineeringFact](t, factFixture)
	model := decodePlanningFixture[domain.ProjectModel](t, "security-sensitive.project-model.json")
	return planning.CompileInput{
		PlanID:    "plan-" + strings.TrimSuffix(factFixture, ".engineering-fact.json"),
		Revision:  1,
		Objective: contract.Objective,
		Subject:   contract.Subject,
		Contract:  contract,
		Model:     model,
		Facts:     []domain.EngineeringFact{fact},
		Template:  template,
		Envelope:  operatorEnvelope(),
	}
}

func contractFor(t *testing.T, factFixture string) domain.EngineeringWorkContract {
	t.Helper()
	fact := decodePlanningFixture[domain.EngineeringFact](t, factFixture)
	model := decodePlanningFixture[domain.ProjectModel](t, "security-sensitive.project-model.json")
	policyDocument := decodePlanningFixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	// The security-sensitive rule gains the plan-shaped obligations #64 adds,
	// stated exactly as an operator would state them in policy.
	rules := map[string]domain.PolicyRule{}
	for id, rule := range policyDocument.Rules {
		rules[id] = rule
	}
	requirements := domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role:         domain.RoleSecurityReviewer,
			Capabilities: []domain.EngineeringCapability{domain.CapabilitySecurityReview},
			Independence: &domain.IndependenceRequirement{Dimension: domain.IndependenceVendorFamily},
			Statement:    "A security reviewer independent of the material producer's vendor family is required.",
		}},
		Gates: []domain.GateRequirement{{
			Kind:      domain.StageHumanDecisionGate,
			Action:    &domain.Action{Type: "git.merge", Target: "main"},
			Statement: "A person decides publication for a change on the authentication boundary.",
		}},
	}
	rules["RULE-PLAN-OBLIGATIONS"] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{EngineeringRequirements: &requirements},
	}
	policyDocument.Rules = rules

	contract, err := policy.Compile(policy.CompileInput{
		ContractID:       "contract-" + fact.ID,
		ContractRevision: "1",
		Objective:        "Change behaviour under test.",
		AcceptanceIntent: []string{"Works."},
		Subject:          fact.Subject,
		Scope:            domain.ContractScope{Stage: domain.StageObserved, AllowedPaths: []string{"internal/payments/retry.go"}},
		ProjectModel:     model,
		Policy:           policyDocument,
		Facts:            []domain.EngineeringFact{fact},
	})
	if err != nil {
		t.Fatalf("compile contract: %v", err)
	}
	return contract
}

func featureTemplate(t *testing.T) domain.EngineeringPlanTemplate {
	t.Helper()
	template := domain.EngineeringPlanTemplate{
		SchemaVersion: domain.SchemaVersion, ID: "zenchron-feature", Version: 1,
		Stages: []domain.TemplateStage{
			{ID: "architecture", Kind: domain.StageAgent, Role: domain.RoleSystemArchitect},
			{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"architecture"}},
		},
		Source: domain.ArtifactSource{Type: domain.SourceOperatorFile, Location: "/operator/templates/zenchron-feature.json"},
	}
	digest, err := template.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	template.Digest = digest
	return template
}

func compilePlan(t *testing.T, input planning.CompileInput) domain.EngineeringPlan {
	t.Helper()
	plan, err := planning.Compile(input)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	return plan
}

func decodePlanningFixture[T domain.Contract](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[T](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func agentRoles(plan domain.EngineeringPlan) []domain.EngineeringRole {
	var roles []domain.EngineeringRole
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent {
			roles = append(roles, stage.Role)
		}
	}
	return roles
}

func stageForRole(plan domain.EngineeringPlan, role domain.EngineeringRole) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent && stage.Role == role {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func stageOfKind(plan domain.EngineeringPlan, kind domain.StageKind) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.Kind == kind {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func stageByID(plan domain.EngineeringPlan, id string) (domain.PlanStage, bool) {
	return plan.Stage(id)
}

func stageOrder(plan domain.EngineeringPlan) map[string]int {
	order := make(map[string]int, len(plan.Stages))
	for i, stage := range plan.Stages {
		order[stage.ID] = i
	}
	return order
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsCapability(values []domain.EngineeringCapability, want domain.EngineeringCapability) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
