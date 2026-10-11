package planning_test

import (
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"testing"
)

func TestCompileExplicitIntegrationAlgorithm(t *testing.T) {
	input := planInput(t, "trivial.engineering-fact.json", nil)
	input.Proposed = []domain.PlanStage{
		{ID: "a", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "a"},
		{ID: "b", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "b"},
		{ID: "compose", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "compose", DependsOn: []string{"a", "b"}, ExecutionKind: "integration_compose"},
	}
	plan, err := planning.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := plan.Stage("compose")
	if !ok || stage.ExecutionKind != "integration_compose" {
		t.Fatalf("algorithm lost: %+v", stage)
	}
	for _, kind := range []string{"invented", "integration_compose"} {
		input.Proposed[0].ExecutionKind = kind
		if _, err := planning.Compile(input); err == nil {
			t.Fatalf("invalid implementer algorithm %q accepted", kind)
		}
	}
	input.Proposed[0].ExecutionKind = ""
	input.Proposed[2].DependsOn = []string{"a"}
	if _, err := planning.Compile(input); err == nil {
		t.Fatal("single-input composition accepted")
	}
}
