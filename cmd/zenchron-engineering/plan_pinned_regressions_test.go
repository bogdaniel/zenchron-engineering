package main

import (
	"bytes"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
	"strings"
	"testing"
)

func TestHeadroomCountsOnlyChildRunCreatingStages(t *testing.T) {
	view := runtime.PlanView{Plan: domain.EngineeringPlan{Stages: []domain.PlanStage{
		{ID: "planner", Kind: domain.StageAgent, InvocationMode: domain.InvocationModeNonMutatingPlanning},
		{ID: "worker", Kind: domain.StageAgent, InvocationMode: domain.InvocationModeMutating},
		{ID: "gate", Kind: domain.StageAssuranceGate},
	}}, Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 2}}
	var out bytes.Buffer
	writePlanBudget(&out, view)
	if strings.Contains(out.String(), "no re-performance headroom") {
		t.Fatal(out.String())
	}
	view.Envelope.MaxChildRuns = 1
	out.Reset()
	writePlanBudget(&out, view)
	if !strings.Contains(out.String(), "no re-performance headroom: 1 agent stages") {
		t.Fatal(out.String())
	}
}

func TestUnboundStageIsDistinctInText(t *testing.T) {
	view := runtime.PlanView{Plan: domain.EngineeringPlan{Stages: []domain.PlanStage{{ID: "worker", Kind: domain.StageAgent}}}, Unbound: []string{"worker"}}
	var out bytes.Buffer
	if _, err := planOutput(autonomyFlags{Text: true}, view, &out, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unbound (approval bound no assignment)") {
		t.Fatal(out.String())
	}
}
