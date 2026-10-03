package controlplane

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestPlanDetailKeepsGatesRunlessAndProposalsLabelled pins the #425 laws at
// the one projection both the JSON route and the page use: only agent stages
// link to runs, a stage with no replayed record is unknown rather than
// pending, a proposal is never labelled as governing, and an unknown cost
// is not zero.
func TestPlanDetailKeepsGatesRunlessAndProposalsLabelled(t *testing.T) {
	view := rt.PlanView{
		Plan: domain.EngineeringPlan{ID: "p", Revision: 3, Stages: []domain.PlanStage{
			{ID: "build", Kind: domain.StageAgent, Role: "implementer"},
			{ID: "assure", Kind: domain.StageAssuranceGate, DependsOn: []string{"build"}},
			{ID: "decide", Kind: domain.StageHumanDecisionGate, DependsOn: []string{"assure"}},
			{ID: "later", Kind: domain.StageAgent, DependsOn: []string{"decide"}},
		}},
		Snapshot: rt.PlanSnapshot{
			Approved: rt.PlanApproval{Status: domain.ApprovalApproved, Revision: 2},
			Stages: map[string]rt.PlanStageProjection{
				"build":  {State: rt.PlanStageCompleted, RunID: "run-1", AgentID: "claude", ProviderKind: "claude_code", TrustMode: "operator_trusted"},
				"assure": {State: rt.PlanStageSatisfied, RunID: "run-fabricated"},
				"decide": {State: rt.PlanStagePending},
			},
		},
		Preview: &rt.PlanPreview{GoverningRevision: 2, Invalidated: []string{"assure"}},
	}
	d := planDetailProjection(view)
	if d.Shown != "proposed" || d.GoverningRevision != 2 {
		t.Fatalf("revision 3 over governing 2 must read as proposed, got %q/%d", d.Shown, d.GoverningRevision)
	}
	byID := map[string]PlanStageDetail{}
	for _, s := range d.Stages {
		byID[s.ID] = s
	}
	if byID["build"].RunID != "run-1" || byID["assure"].RunID != "" {
		t.Fatalf("only agent stages may link a run: %+v", d.Stages)
	}
	if byID["later"].State != "" {
		t.Fatalf("a stage with no replayed record is unknown, got %q", byID["later"].State)
	}
	if d.Budget.CostKnown || d.Budget.CostMicros != nil {
		t.Fatalf("unknown cost must stay unknown: %+v", d.Budget)
	}
	out := renderTemplate(t, planDetailTemplate, planDetailData{Plan: d})
	for _, want := range []string{`href="/runs/run-1"`, "gate: no run", "not recorded", ">proposed<", "approving this would invalidate", `?revision=2`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered plan detail lacks %q", want)
		}
	}
	if strings.Contains(out, "run-fabricated") {
		t.Fatal("a gate rendered a run link")
	}
}
