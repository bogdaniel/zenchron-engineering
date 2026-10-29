package planning_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// The #120 dogfood shape, compiled.
//
// The failed real planning attempt against #119 proposed an ACYCLIC
// decomposition - two implementers in sequence, then an independent reviewer -
// and the compiler turned it into a cycle while completing independence
// semantics. These tests fix the meaning of an independence requirement that
// names no peer, in every state it can be in:
//
//	absent on a producer            no obligation, no edge
//	present with explicit peers     obligation over exactly those, each an edge
//	present and empty on a reviewer the shorthand: every material producer
//	present and empty on a producer REFUSED, typed, before any edge is added
//
// The rule is one sentence: the compiler never turns ambiguous planner
// shorthand into producer dependencies it was not asked for.

// dogfoodStages is the exact shape the planner proposed for #119, in its VALID
// form: the two implementers state no independence, and the reviewer states the
// independence it actually needs.
func dogfoodStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "harden-runtime-transitions", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "harden the runtime transitions"},
		{ID: "correct-operator-views", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "correct the operator views", DependsOn: []string{"harden-runtime-transitions"}},
		{ID: "verify-combined-candidate", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			Objective: "review the combined candidate",
			DependsOn: []string{"correct-operator-views"},
			Independence: &domain.IndependenceRequirement{
				Dimension:     domain.IndependenceExecutionAgent,
				DifferentFrom: []string{"harden-runtime-transitions", "correct-operator-views"},
			}},
	}
}

// The valid representation of the dogfood decomposition compiles, and the two
// implementers do not become dependencies of each other.
func TestDogfoodDecompositionCompilesWithoutAProducerCycle(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = dogfoodStages()

	plan := compilePlan(t, input)

	first, found := stageByID(plan, "harden-runtime-transitions")
	if !found {
		t.Fatalf("the first implementer is not in the compiled plan: %v", planStageIDs(plan))
	}
	if len(first.DependsOn) != 0 {
		t.Fatalf("the first implementer gained dependencies %v: it was proposed with none", first.DependsOn)
	}
	second, _ := stageByID(plan, "correct-operator-views")
	if containsString(second.DependsOn, "harden-runtime-transitions") == false {
		t.Fatalf("the second implementer lost its stated dependency: %v", second.DependsOn)
	}
	if containsString(first.DependsOn, "correct-operator-views") {
		t.Fatal("the compiler made the first implementer depend on the second: that is the manufactured cycle #120 names")
	}
	// The reviewer's independence still orders it after both producers, which
	// is the property that makes the obligation checkable at all.
	reviewer, _ := stageByID(plan, "verify-combined-candidate")
	for _, peer := range []string{"harden-runtime-transitions", "correct-operator-views"} {
		if !containsString(reviewer.Independence.DifferentFrom, peer) {
			t.Fatalf("the reviewer's independence lost peer %q: %#v", peer, reviewer.Independence)
		}
		if !containsString(reviewer.DependsOn, peer) {
			t.Fatalf("the reviewer does not depend on %q, so its independence could resolve before the work it judges", peer)
		}
	}
}

// The exact defective shape - a material producer carrying an independence
// requirement over nothing - is refused with an explanation, and never with a
// cycle.
func TestEmptyIndependenceOnAProducerIsRefusedRatherThanCompleted(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	stages := dogfoodStages()
	for i := range stages[:2] {
		stages[i].Independence = &domain.IndependenceRequirement{
			Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{},
		}
	}
	input.Proposed = stages

	_, err := planning.Compile(input)
	if err == nil {
		t.Fatal("a material producer requiring independence from nothing compiled")
	}
	var invalid *planning.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the refusal is not typed: %T %v", err, err)
	}
	joined := strings.Join(invalid.Reasons, "; ")
	if strings.Contains(joined, "cycle") {
		t.Fatalf("the refusal is still reported as a cycle, which is the symptom rather than the cause: %s", joined)
	}
	if !strings.Contains(joined, "harden-runtime-transitions") || !strings.Contains(joined, "different_from") {
		t.Fatalf("the refusal does not name the stage and the ambiguous member: %s", joined)
	}
	if !strings.Contains(joined, "omit its independence requirement") {
		t.Fatalf("the refusal does not say what to do instead: %s", joined)
	}
}

// A stage that produces nothing keeps the shorthand. It is unambiguous there -
// a reviewer reviews producers - and it can only ever add the edges the
// obligation already implied.
func TestEmptyIndependenceOnAReviewerStillBindsToEveryProducer(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	stages := dogfoodStages()
	stages[2].Independence = &domain.IndependenceRequirement{
		Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{},
	}
	input.Proposed = stages

	plan := compilePlan(t, input)

	reviewer, _ := stageByID(plan, "verify-combined-candidate")
	if reviewer.Independence == nil {
		t.Fatal("the reviewer's independence requirement was dropped")
	}
	for _, producer := range []string{"harden-runtime-transitions", "correct-operator-views"} {
		if !containsString(reviewer.Independence.DifferentFrom, producer) {
			t.Fatalf("the shorthand did not bind producer %q: %#v", producer, reviewer.Independence)
		}
		if !containsString(reviewer.DependsOn, producer) {
			t.Fatalf("the shorthand bound %q without ordering the reviewer after it", producer)
		}
	}
	if containsString(reviewer.Independence.DifferentFrom, "verify-combined-candidate") {
		t.Fatal("the reviewer is required to be independent of itself")
	}
}

// A producer that states no independence at all keeps none, and gains no edge.
// This is the ordinary case the planner contract now asks for, and it has to
// compile without ceremony.
func TestProducersWithoutIndependenceCompileUnchanged(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = dogfoodStages()

	plan := compilePlan(t, input)

	for _, id := range []string{"harden-runtime-transitions", "correct-operator-views"} {
		stage, found := stageByID(plan, id)
		if !found {
			t.Fatalf("stage %q is missing from the compiled plan", id)
		}
		if stage.Independence != nil {
			t.Fatalf("stage %q gained an independence requirement nobody asked for: %#v", id, stage.Independence)
		}
	}
}

// The #368 plan dogfood shape, compiled.
//
// A real planning attempt proposed an ACYCLIC four-stage decomposition: an
// implementer, two reviewers independent of "whatever produced the change",
// and an integrator that depends on both reviewers. The compiler is required
// to add the assurance gate the contract's claims obligate - that much is
// #64 - but completing the reviewers' empty "different_from" bound them to
// EVERY material producer, including the integrator, which already depended
// on them. The peer edge that obligation manufactured then closed a loop
// through the integrator: review -> integrate -> review.
//
// A producer downstream of a stage is never a candidate for that stage's
// unbound independence completion, for the same reason #120 refuses an
// unbound requirement on a producer itself: the compiler never turns
// ambiguous shorthand into a dependency edge that reverses one the plan
// already states.
func dogfoodDownstreamIntegratorStages() []domain.PlanStage {
	independent := &domain.IndependenceRequirement{Dimension: domain.IndependenceExecutionAgent}
	return []domain.PlanStage{
		{ID: "implement-digest-warning", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "implement the digest warning"},
		{ID: "security-review-digest-warning", Kind: domain.StageAgent, Role: domain.RoleSecurityReviewer,
			Objective: "security-review the digest warning", Independence: independent},
		{ID: "review-digest-warning", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			Objective: "review the digest warning", Independence: independent},
		{ID: "integrate-digest-warning", Kind: domain.StageAgent, Role: domain.RoleIntegrator,
			Objective: "integrate the digest warning",
			DependsOn: []string{"security-review-digest-warning", "review-digest-warning"}},
	}
}

func TestAssuranceInsertionOverADownstreamIntegratorCompilesWithoutACycle(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = dogfoodDownstreamIntegratorStages()
	if len(input.Contract.RequiredClaims) == 0 {
		t.Fatal("the fixture contract requires no claims: this test needs one that does")
	}

	plan := compilePlan(t, input)

	assurance, found := stageByKind(plan, domain.StageAssuranceGate)
	if !found {
		t.Fatalf("the contract requires claims and no assurance gate was compiled: %v", planStageIDs(plan))
	}
	if len(assurance.RequiredClaims) == 0 {
		t.Fatal("the assurance gate references no claim: the contract's obligation was dropped")
	}

	for _, id := range []string{"security-review-digest-warning", "review-digest-warning"} {
		reviewer, found := stageByID(plan, id)
		if !found {
			t.Fatalf("stage %q is missing from the compiled plan", id)
		}
		if reviewer.Independence == nil {
			t.Fatalf("stage %q lost its independence requirement", id)
		}
		if !containsString(reviewer.Independence.DifferentFrom, "implement-digest-warning") {
			t.Fatalf("stage %q is no longer independent of the implementer: %#v", id, reviewer.Independence)
		}
		if containsString(reviewer.Independence.DifferentFrom, "integrate-digest-warning") {
			t.Fatalf("stage %q was bound to the downstream integrator, which is how #368 manufactures a cycle: %#v", id, reviewer.Independence)
		}
		if containsString(reviewer.DependsOn, "integrate-digest-warning") {
			t.Fatalf("stage %q depends on the downstream integrator that already depends on it: %v", id, reviewer.DependsOn)
		}
	}
}

func stageByKind(plan domain.EngineeringPlan, kind domain.StageKind) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.Kind == kind {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func planStageIDs(plan domain.EngineeringPlan) []string {
	ids := make([]string, 0, len(plan.Stages))
	for _, stage := range plan.Stages {
		ids = append(ids, stage.ID)
	}
	return ids
}

// A plan with NO material producer refuses a policy independence obligation for
// its own reason. A reviewing stage does not produce material change, and
// telling an operator that it does would send them looking for a defect that is
// not there.
func TestIndependenceInAPlanWithNoProducerIsRefusedOnItsOwnTerms(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	// Only non-producing stages, and a policy obligation on the reviewer.
	input.Proposed = []domain.PlanStage{
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer, Objective: "review"},
	}
	requirements := domain.PlanRequirements{Roles: []domain.RoleRequirement{{
		Role: domain.RoleReviewer, Statement: "an independent reviewer is required",
		Independence: &domain.IndependenceRequirement{Dimension: domain.IndependenceVendorFamily},
	}}}
	contract := input.Contract
	contract.PlanRequirements = &requirements
	input.Contract = contract

	_, err := planning.Compile(input)
	if err == nil {
		t.Fatal("an independence obligation over nothing compiled")
	}
	var invalid *planning.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the refusal is not typed: %T %v", err, err)
	}
	joined := strings.Join(invalid.Reasons, "; ")
	if strings.Contains(joined, `stage "review" performs role`) {
		t.Fatalf("a reviewer was described as producing material change: %s", joined)
	}
	if !strings.Contains(joined, "no stage in it produces material change") {
		t.Fatalf("the refusal does not name the real condition: %s", joined)
	}
}

// A stage told to be independent of ITSELF and nothing else is refused for that
// reason, not for naming nothing.
//
// The self-entry is dropped before the emptiness check, so the refusal used to
// describe an empty "different_from" the plan never contained. The record is the
// product here: an operator reading a refusal has to be reading about the
// proposal they made.
func TestIndependenceFromOnlyItselfIsRefusedForWhatItSays(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "work"},
	}
	requirements := domain.PlanRequirements{Roles: []domain.RoleRequirement{{
		Role: domain.RoleImplementer, Statement: "the producer is independent",
		Independence: &domain.IndependenceRequirement{
			Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"implementation"},
		},
	}}}
	contract := input.Contract
	contract.PlanRequirements = &requirements
	input.Contract = contract

	_, err := planning.Compile(input)
	if err == nil {
		t.Fatal("a stage required to be independent of only itself compiled")
	}
	var invalid *planning.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the refusal is not typed: %T %v", err, err)
	}
	joined := strings.Join(invalid.Reasons, "; ")
	if !strings.Contains(joined, "independence from itself") {
		t.Fatalf("the refusal does not say what the plan actually asked for: %s", joined)
	}
	if strings.Contains(joined, `whose "different_from" is empty`) {
		t.Fatalf("the refusal describes an empty different_from the plan never contained: %s", joined)
	}
}
