package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// The operator's request names the feature; the real compiler binds it to the
// one final integration, the stored plan carries it in its digest, the objective
// graph's units resolve it from that pinned plan, and a revision cannot drop it.
func TestTheOperatorsDeclarationSurvivesTheRealPlanServiceAndPinsTheUnitOrigin(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	fleet.runtime = fleet.newRuntime(fleet.deps)
	harness := newPlanRunHarnessWith(fleet.phase8Fixture, fleet.deps.ConfigDigest, defaultPlanFixtureEnvelope)
	subject := domain.Subject{Repository: "acme/repo", Revision: fleet.base}
	stages := []domain.PlanStage{producerStage("backend", "a"), producerStage("frontend", "b"), integratorStage("backend", "frontend")}
	propose := func(feature string, reasoned []domain.PlanStage) (domain.EngineeringPlan, error) {
		return harness.service.Propose(context.Background(), ProposeInput{
			PlanID: "plan-fixture", Objective: "Make the widget idempotent.", Subject: subject,
			Contract: planFixtureContract(fleet.phase8Fixture), Issue: fleet.issue,
			Model:    domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject},
			Reasoned: reasoned, AcceptanceFeature: feature,
		})
	}

	first, err := propose(journeyFeature, stages)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeclaredAcceptanceFeature() != journeyFeature {
		t.Fatalf("the compiled plan declares %q", first.DeclaredAcceptanceFeature())
	}
	graphID, err := harness.service.BindObjectivePlan(first, fleet.issue, harness.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	for unit, want := range map[string]string{"integrate": journeyFeature, "backend": "", "frontend": ""} {
		got, err := declaredAcceptanceFeature(fleet.store, graphID, unit)
		if err != nil || got != want {
			t.Errorf("unit %s resolves %q (%v), want %q", unit, got, err, want)
		}
	}

	// A re-proposal that does not restate the feature inherits it: the planner's
	// next answer cannot remove what the operator declared.
	second, err := propose("", stages)
	if err != nil {
		t.Fatal(err)
	}
	if second.DeclaredAcceptanceFeature() != journeyFeature {
		t.Fatalf("a re-proposal dropped the operator's declaration: %q", second.DeclaredAcceptanceFeature())
	}

	// A proposal cannot declare it, and a declaration with no final integration
	// is refused rather than silently dropped.
	declared := append([]domain.PlanStage{}, stages...)
	declared[2].AcceptanceFeature = "invented-by-planner"
	if _, err := propose("", declared); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("a planner-declared feature was accepted by the plan service: %v", err)
	}
	other := newPlanRunHarnessWith(fleet.phase8Fixture, fleet.deps.ConfigDigest, defaultPlanFixtureEnvelope)
	if _, err := other.service.Propose(context.Background(), ProposeInput{
		PlanID: "plan-no-integration", Objective: "Make the widget idempotent.", Subject: subject,
		Contract: planFixtureContract(fleet.phase8Fixture), Issue: fleet.issue,
		Model:    domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject},
		Reasoned: stages[:2], AcceptanceFeature: journeyFeature,
	}); err == nil {
		t.Fatal("a declaration with no final integration stage was accepted")
	}
}
