package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func planningArtifactRequest(kind planning.ArtifactKind, id, document string) runtime.ControlRequest {
	return runtime.ControlRequest{
		Command:          runtime.ControlPlanningArtifactPropose,
		PlanningArtifact: &planning.ArtifactWrite{Kind: kind, ID: id, Document: json.RawMessage(document)},
	}
}

func stagesByID(view runtime.PlanView) map[string]domain.PlanStage {
	out := make(map[string]domain.PlanStage, len(view.Plan.Stages))
	for _, s := range view.Plan.Stages {
		out[s.ID] = s
	}
	return out
}

// TestDashboardAuthoredProfileAndTemplateDriveARealApprovedWorkGraph is the
// end-to-end acceptance walkthrough #471/#479 asks for, expressed as a real
// dispatch chain rather than a manual browser click-through (which this
// session could not run without the operator's own control-plane token):
//
//  1. author an AgentProfile and a two-stage EngineeringPlanTemplate through
//     the SAME governed planning-artifact write the dashboard form sends
//     (ControlPlanningArtifactPropose);
//  2. reload the registry (ControlPlanningReload) so a new objective sees
//     them, with no process restart;
//  3. propose an objective naming that template, through the exact
//     ControlRequest shape ObjectiveIntake's POST /v1/objectives sends
//     (ControlPlanRevise);
//  4. approve it (ControlPlanApprove).
//
// It asserts the newly-authored profile was actually bound to the stage
// that named it, the template's dependency survived compilation, and the
// plan is bound to a WorkGraph whose units match - the same WorkGraph panel
// frontend/src/pages.tsx already renders once one exists (proven generally
// by TestEveryPlanProposalBindsAWorkGraph).
func TestDashboardAuthoredProfileAndTemplateDriveARealApprovedWorkGraph(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)

	built, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Readiness is an OBSERVATION (docs/getting-started.md): the configured
	// agent is reported unavailable until its credential can be inspected.
	// This is the same boundary fixture browser_e2e_test.go uses - the
	// content is never read as a real credential, only its presence checked.
	if err := os.WriteFile(built.agent.CredentialPath, []byte("unused-walkthrough-boundary-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handle := func(r runtime.ControlRequest) runtime.ControlResponse {
		return built.handleControl(ctx, driver, func() {}, r)
	}

	// Step 1: author the profile and the template exactly as the dashboard
	// forms do.
	if r := handle(planningArtifactRequest(planning.KindAgentProfile, "walkthrough-implementer",
		`{"execution_agent":"openai-responses","capabilities":["code_change","repository_analysis","verification"],"trust_requirement":"protected"}`,
	)); !r.OK {
		t.Fatalf("author profile: %+v", r)
	}
	if r := handle(planningArtifactRequest(planning.KindEngineeringPlanTemplate, "walkthrough-feature",
		`{"stages":[
			{"id":"implementation","kind":"agent","role":"implementer","profile":"walkthrough-implementer"},
			{"id":"review","kind":"agent","role":"reviewer","depends_on":["implementation"]}
		]}`,
	)); !r.OK {
		t.Fatalf("author template: %+v", r)
	}

	// Step 2: reload, so the objective proposed next resolves what was just
	// authored rather than what the process started with.
	if r := handle(runtime.ControlRequest{Command: runtime.ControlPlanningReload}); !r.OK {
		t.Fatalf("reload: %+v", r)
	}

	// Step 3: propose the objective, the exact ControlRequest shape
	// ObjectiveIntake's POST /v1/objectives sends.
	proposed := handle(runtime.ControlRequest{
		Command: runtime.ControlPlanRevise, Repository: "zenchron/seeded", Issue: 41,
		Template: "walkthrough-feature", Deterministic: true,
	})
	if !proposed.OK {
		t.Fatalf("propose objective: %+v", proposed)
	}
	var view runtime.PlanView
	if err := json.Unmarshal(proposed.Payload, &view); err != nil {
		t.Fatal(err)
	}

	// The authored template's dependency survived compilation.
	stages := stagesByID(view)
	if _, ok := stages["implementation"]; !ok {
		t.Fatalf("plan has no implementation stage: %+v", view.Plan.Stages)
	}
	review, ok := stages["review"]
	if !ok || len(review.DependsOn) != 1 || review.DependsOn[0] != "implementation" {
		t.Fatalf("review stage lost its dependency on implementation: %+v", review)
	}

	// The authored profile was actually bound to the stage that named it.
	var assignedToImplementer bool
	for _, a := range view.Assigned {
		if a.StageID == "implementation" && a.Profile.ID == "walkthrough-implementer" {
			assignedToImplementer = true
		}
	}
	if !assignedToImplementer {
		t.Fatalf("the authored profile was never bound to its stage: assigned=%+v blocked=%+v", view.Assigned, view.Blocked)
	}

	// The plan is already bound to a WorkGraph at propose time; checked here
	// against THIS plan's own units.
	if view.WorkGraphID == "" {
		t.Fatal("the proposed objective carries no WorkGraphID")
	}
	graph, found, err := built.store.WorkGraph(view.WorkGraphID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("WorkGraph %s does not exist in the durable store", view.WorkGraphID)
	}
	// Only "implementation" is a unit: a WorkGraph unit exists for a
	// candidate-PRODUCING stage (planning.ProducesMaterialChange), and
	// "review" deliberately is not one - it is governed through the plan's
	// own stage-review mechanism, never duplicated as a graph unit.
	unitIDs := map[string]bool{}
	for _, u := range graph.Units {
		unitIDs[u.ID] = true
	}
	if !unitIDs["implementation"] {
		t.Fatalf("WorkGraph is missing the implementation unit: %+v", graph.Units)
	}
	if unitIDs["review"] {
		t.Fatalf("a review stage must never become its own WorkGraph unit: %+v", graph.Units)
	}

	// Step 4: approve. Approval requires the exact assignments digest the
	// compiled preview carries - the same binding the dashboard's
	// GovernedAction review step reads before submitting.
	approved := handle(runtime.ControlRequest{
		Command: runtime.ControlPlanApprove, PlanID: view.Plan.ID, Revision: view.Plan.Revision,
		Digest: view.Plan.Digest, AssignmentsDigest: view.AssignmentsDigest,
	})
	if !approved.OK {
		t.Fatalf("approve: %+v", approved)
	}
}
