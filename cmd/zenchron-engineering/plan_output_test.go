package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// What the operator's terminal says about a plan that is not the simple case.
//
// Three things were only in the snapshot JSON or were said wrongly: a REJECTED
// revision was still offered as "what approving this revision would leave",
// which describes a decision nobody is being asked for; a stage being performed
// again showed no sign of it; and an envelope with exactly one child run per
// agent stage reads as sufficient right up to the first time anything upstream
// moves and the re-performance blocks on budget.
func TestThePlanTextSaysWhatTheSnapshotOnlyHeld(t *testing.T) {
	plan := domain.EngineeringPlan{
		ID: "plan-text", Revision: 2, Objective: "Make the widget idempotent.",
		Stages: []domain.PlanStage{
			{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer},
			{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer, DependsOn: []string{"implementation"}},
		},
	}
	view := runtime.PlanView{
		Plan: plan,
		Snapshot: runtime.PlanSnapshot{
			PlanID: plan.ID, Revision: 2,
			Rejected: map[int]bool{2: true},
			Stages: map[string]runtime.PlanStageProjection{
				"implementation": {StageID: "implementation", State: runtime.PlanStageCompleted},
				"review":         {StageID: "review", State: runtime.PlanStageRunning, Generation: 1},
			},
		},
		Preview:  &runtime.PlanPreview{GoverningRevision: 1},
		Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 2, MaxConcurrency: 2, MaxProviderInvocations: 8},
	}

	var out bytes.Buffer
	if _, err := planOutput(autonomyFlags{Text: true}, view, &out, ""); err != nil {
		t.Fatal(err)
	}
	text := out.String()

	if strings.Contains(text, "what approving this revision would leave") {
		t.Fatalf("a rejected revision is offered for approval:\n%s", text)
	}
	if !strings.Contains(text, "this revision was rejected") {
		t.Fatalf("the preview does not say the revision was rejected:\n%s", text)
	}
	if !strings.Contains(text, "execution 2") {
		t.Fatalf("the re-performed stage does not say which execution it is on:\n%s", text)
	}
	if !strings.Contains(text, "no re-performance headroom") {
		t.Fatalf("an envelope with no room to perform a stage again does not say so:\n%s", text)
	}

	// And the ordinary case says none of it: a first performance is not
	// annotated, and an envelope with room is not warned about.
	roomy := view
	roomy.Envelope.MaxChildRuns = 4
	roomy.Snapshot.Stages = map[string]runtime.PlanStageProjection{
		"implementation": {StageID: "implementation", State: runtime.PlanStageCompleted},
		"review":         {StageID: "review", State: runtime.PlanStageRunning},
	}
	roomy.Snapshot.Rejected = nil
	out.Reset()
	if _, err := planOutput(autonomyFlags{Text: true}, roomy, &out, ""); err != nil {
		t.Fatal(err)
	}
	text = out.String()
	if strings.Contains(text, "execution ") {
		t.Fatalf("a first performance was annotated as a later one:\n%s", text)
	}
	if strings.Contains(text, "no re-performance headroom") {
		t.Fatalf("an envelope with room was warned about:\n%s", text)
	}
	if !strings.Contains(text, "what approving this revision would leave") {
		t.Fatalf("an undecided revision is not offered for approval:\n%s", text)
	}
}
