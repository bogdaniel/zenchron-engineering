package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
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

// The approval preview cannot drive the terminal either.
//
// preview.Invalidated holds STAGE IDS, and the graph laws constrain only that
// an id is non-blank - never its character set - so a model-chosen id carrying
// ESC or a bare CSI reaches "approving would redo" unescaped. That is the worst
// line in the product to render unsafely: it is what an operator reads while
// deciding whether to approve.
//
// The sweep that found this one also wrapped every other dynamic string in the
// plan view, which is the lesson of having missed this perimeter three times:
// the boundary is "every value that did not originate here", not "the ones we
// thought of".
func TestThePlanViewCannotDriveTheTerminal(t *testing.T) {
	hostile := "redo-me\x1b[2J\rnow\u009b31m"
	plan := domain.EngineeringPlan{
		ID: "plan-text\x1b[1m", Revision: 2, Objective: "Make the widget idempotent.",
		Stages: []domain.PlanStage{
			{ID: hostile, Kind: domain.StageAgent, Role: domain.RoleImplementer},
		},
		Provenance: domain.PlanProvenance{
			Reasoning: &domain.PlanReasoningProvenance{
				AgentID: "codex\x1b[7m", ProviderKind: "codex_cli", VendorFamily: "openai",
				InvocationMode: domain.InvocationModeNonMutatingPlanning, WorkspaceUnchanged: true,
			},
		},
	}
	view := runtime.PlanView{
		Plan: plan,
		Snapshot: runtime.PlanSnapshot{
			PlanID: plan.ID, Revision: 2,
			Approval: runtime.PlanApproval{Status: domain.ApprovalPending, Revision: 2, Digest: "d\x1b[0m"},
			Stages:   map[string]runtime.PlanStageProjection{hostile: {StageID: hostile}},
			References: []runtime.PlanSourceReferencePayload{
				{Repository: "acme/repo", Issue: 110, Available: false, Detail: "forge said\x1b[2Jthings"},
			},
		},
		Preview:  &runtime.PlanPreview{GoverningRevision: 1, Invalidated: []string{hostile}},
		Blocked:  []planning.Blocked{{StageID: hostile, Kind: "independence", Reason: "no eligible\x1bworker"}},
		Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 2, MaxProviderInvocations: 8},
	}

	var out bytes.Buffer
	if _, err := planOutput(autonomyFlags{Text: true}, view, &out, "proposed"); err != nil {
		t.Fatal(err)
	}
	printed := out.String()
	if !strings.Contains(printed, "approving would redo") {
		t.Fatalf("the preview line is missing entirely:\n%s", printed)
	}
	for _, raw := range []string{"\x1b", "\r", "\u009b", "\x7f"} {
		if strings.Contains(printed, raw) {
			t.Fatalf("the plan view emitted raw control %q:\n%q", raw, printed)
		}
	}
	// Escaped VISIBLY, and the readable part still readable.
	for _, want := range []string{`\x1b`, `\x0d`, `\x9b`, "redo-me", "approving would redo"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the plan view does not show %q:\n%s", want, printed)
		}
	}

	// The JSON surface is untouched: it escapes on its own and carries the
	// exact evidence.
	var encoded bytes.Buffer
	if _, err := planOutput(autonomyFlags{}, view, &encoded, ""); err != nil {
		t.Fatal(err)
	}
	var decoded runtime.PlanView
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatalf("the JSON surface is not valid JSON: %v", err)
	}
	if decoded.Preview == nil || len(decoded.Preview.Invalidated) != 1 ||
		decoded.Preview.Invalidated[0] != hostile {
		t.Fatalf("the JSON surface lost the exact invalidated stage id: %#v", decoded.Preview)
	}
	if strings.Contains(encoded.String(), "\x1b") {
		t.Fatalf("the JSON surface emitted a raw escape:\n%q", encoded.String())
	}
}
