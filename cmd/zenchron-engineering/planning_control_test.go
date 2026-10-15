package main

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const testProfileDocument = `{
  "execution_agent": "claude",
  "capabilities": ["repository_analysis"],
  "trust_requirement": "operator_trusted"
}`

// TestProposePlanningArtifactWritesButDoesNotReload pins ADR-0008's two-step
// shape: a write lands on disk immediately, but the held registry a live
// engine resolves packs through does not change until a caller asks for a
// reload.
func TestProposePlanningArtifactWritesButDoesNotReload(t *testing.T) {
	dir := t.TempDir()
	c := &composition{config: runtime.Config{OperatorConfig: runtime.OperatorConfig{PlanningDir: dir}}}

	if _, err := c.proposePlanningArtifact(runtime.ControlRequest{
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(testProfileDocument)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.currentPlanning().Profile("reviewer"); err == nil {
		t.Fatal("a write must not be visible before a reload")
	}

	registry, err := c.reloadPlanning()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Profile("reviewer"); err != nil {
		t.Fatalf("reload must see what was just written: %v", err)
	}
	if _, err := c.currentPlanning().Profile("reviewer"); err != nil {
		t.Fatalf("reload must swap the held snapshot every later reader sees: %v", err)
	}
}

// TestProposePlanningArtifactRequiresAProposal mirrors AdoptProduct's and
// AdoptWorkGraph's refusal of a request naming the command but carrying no
// document to write.
func TestProposePlanningArtifactRequiresAProposal(t *testing.T) {
	c := &composition{config: runtime.Config{OperatorConfig: runtime.OperatorConfig{PlanningDir: t.TempDir()}}}
	if _, err := c.proposePlanningArtifact(runtime.ControlRequest{}); err == nil {
		t.Fatal("a planning artifact request with no document must be refused")
	}
}

// TestBrowserGovernedCommandsIncludePlanningAuthoring is a one-line regression
// for the allowlist: these two verbs must be reachable from the browser
// control plane, the same way plan and product mutations are.
func TestBrowserGovernedCommandsIncludePlanningAuthoring(t *testing.T) {
	for _, command := range []string{runtime.ControlPlanningArtifactPropose, runtime.ControlPlanningReload} {
		if !browserGovernedCommand(command) {
			t.Errorf("%q must be browser-governed", command)
		}
	}
}
