package planning_test

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

func integrationStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "a", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "a"},
		{ID: "b", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "b"},
		{ID: "compose", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "compose", DependsOn: []string{"a", "b"}, ExecutionKind: "integration_compose"},
	}
}

// The operator's declaration lands on the one final integration and nowhere
// else, and a plan without a declaration is the document it always was.
func TestTheOperatorsAcceptanceFeatureIsBoundToTheFinalIntegrationOnly(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = integrationStages()
	undeclared, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if undeclared.DeclaredAcceptanceFeature() != "" {
		t.Fatalf("a plan with no declaration carries %q", undeclared.DeclaredAcceptanceFeature())
	}
	input.AcceptanceFeature = "delivery-report"
	declared, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range declared.Stages {
		if want := stage.ID == "compose"; (stage.AcceptanceFeature == "delivery-report") != want {
			t.Errorf("stage %q carries %q", stage.ID, stage.AcceptanceFeature)
		}
	}
	if declared.Digest == undeclared.Digest {
		t.Error("the declaration is not part of the plan digest")
	}
}

// A planner proposes topology. It cannot invent the declaration, and an
// operator declaration with no final integration or several is refused rather
// than dropped or guessed.
func TestAProposalCannotDeclareAcceptanceAndTheOperatorsDeclarationIsNeverDropped(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = integrationStages()
	input.Proposed[2].AcceptanceFeature = "delivery-report"
	if _, err := planning.Compile(input); err == nil || !strings.Contains(err.Error(), "only the operator") {
		t.Fatalf("a planner-declared feature was accepted: %v", err)
	}

	for name, mutate := range map[string]func(*[]domain.PlanStage){
		"no integration": func(s *[]domain.PlanStage) { *s = (*s)[:2] },
		"two final integrations": func(s *[]domain.PlanStage) {
			*s = append(*s, domain.PlanStage{ID: "compose2", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "c2", DependsOn: []string{"a", "b"}, ExecutionKind: "integration_compose"})
		},
	} {
		input := planInput(t, "trivial.engineering-fact.json", nil)
		input.Proposed = integrationStages()
		mutate(&input.Proposed)
		input.AcceptanceFeature = "delivery-report"
		if _, err := planning.Compile(input); err == nil {
			t.Errorf("%s: the operator's declaration was accepted with no single final integration", name)
		}
	}

	// An earlier integration that a later one consumes is not final.
	input = planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = append(integrationStages(),
		domain.PlanStage{ID: "c", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "c"},
		domain.PlanStage{ID: "final", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "f", DependsOn: []string{"compose", "c"}, ExecutionKind: "integration_compose"})
	input.AcceptanceFeature = "delivery-report"
	plan, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if stage, _ := plan.Stage("compose"); stage.AcceptanceFeature != "" {
		t.Error("the declaration landed on an intermediate integration")
	}
	if stage, _ := plan.Stage("final"); stage.AcceptanceFeature != "delivery-report" {
		t.Error("the declaration did not land on the final integration")
	}

	for _, bad := range []string{"Delivery", "../x", "a b", "-x", strings.Repeat("a", 49)} {
		input.AcceptanceFeature = bad
		if _, err := planning.Compile(input); err == nil {
			t.Errorf("feature name %q was accepted", bad)
		}
	}
}

// A document that carries the field anywhere but the final integration is not a
// valid plan, whoever wrote it.
func TestValidationRefusesADeclarationOnTheWrongStage(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = integrationStages()
	input.AcceptanceFeature = "delivery-report"
	plan, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.EngineeringPlan){
		"on a producer": func(p *domain.EngineeringPlan) { p.Stages[0].AcceptanceFeature = "delivery-report" },
		"on two stages": func(p *domain.EngineeringPlan) {
			for i := range p.Stages {
				p.Stages[i].AcceptanceFeature = "delivery-report"
			}
		},
		"on a terminal stage that is no integration": func(p *domain.EngineeringPlan) {
			p.Stages[2].AcceptanceFeature = ""
			p.Stages = append(p.Stages, domain.PlanStage{ID: "lonely", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "x", AcceptanceFeature: "delivery-report"})
		},
		"on two final integrations": func(p *domain.EngineeringPlan) {
			p.Stages = append(p.Stages, domain.PlanStage{ID: "compose2", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "c2",
				DependsOn: []string{"a", "b"}, ExecutionKind: "integration_compose", AcceptanceFeature: "delivery-report"})
		},
		"an invalid name": func(p *domain.EngineeringPlan) {
			for i := range p.Stages {
				if p.Stages[i].AcceptanceFeature != "" {
					p.Stages[i].AcceptanceFeature = "Not Valid"
				}
			}
		},
	} {
		mutated := plan
		mutated.Stages = append([]domain.PlanStage{}, plan.Stages...)
		mutate(&mutated)
		if err := planning.Validate(mutated, planning.ValidationInput{Contract: input.Contract}); err == nil {
			t.Errorf("%s: an invalid declaration passed validation", name)
		}
	}
}

// A revision that carries untouched stages back verbatim (a human substitution)
// keeps the operator's declaration; a proposal naming any other feature is
// still refused.
func TestARevisionEchoingTheOperatorsDeclarationIsStrippedAndRestamped(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = integrationStages()
	input.AcceptanceFeature = "delivery-report"
	first, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Proposed = first.Stages // the declaration travels back on the final integration
	again, err := planning.Compile(input)
	if err != nil {
		t.Fatalf("an echoed declaration was refused: %v", err)
	}
	if stage, _ := again.Stage("compose"); stage.AcceptanceFeature != "delivery-report" {
		t.Errorf("the echoed declaration was lost: %q", stage.AcceptanceFeature)
	}
	input.Proposed = append([]domain.PlanStage{}, first.Stages...)
	input.Proposed[2].AcceptanceFeature = "another-feature"
	if _, err := planning.Compile(input); err == nil {
		t.Error("a proposal replacing the operator's declaration was accepted")
	}
	// Echoing a value when the operator declared nothing is an invention.
	input.AcceptanceFeature = ""
	input.Proposed = first.Stages
	if _, err := planning.Compile(input); err == nil {
		t.Error("a proposal carrying a declaration the operator never made was accepted")
	}
}
