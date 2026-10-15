package main

import (
	"os"
	"path/filepath"
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
	agents, err := (runtime.OperatorConfig{
		Agents:       map[string]runtime.AgentConfig{"claude": {Kind: runtime.AgentKindClaudeCode, TrustMode: string(runtime.TrustOperatorTrusted)}},
		DefaultAgent: "claude",
	}).AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	c := &composition{agents: agents, config: runtime.Config{OperatorConfig: runtime.OperatorConfig{PlanningDir: dir}}}

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

// TestProposePlanningArtifactRefusesAnEscalatingProfile exercises
// composition.proposePlanningArtifact itself, not a mock of it: the
// described, configured workforce it builds internally is what refuses an
// escalating profile, end to end through the real control-plane dispatch
// path an operator's dashboard write actually takes.
func TestProposePlanningArtifactRefusesAnEscalatingProfile(t *testing.T) {
	dir := t.TempDir()
	agents, err := (runtime.OperatorConfig{
		Agents:       map[string]runtime.AgentConfig{"claude": {Kind: runtime.AgentKindClaudeCode, TrustMode: string(runtime.TrustOperatorTrusted)}},
		DefaultAgent: "claude",
	}).AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	c := &composition{agents: agents, config: runtime.Config{OperatorConfig: runtime.OperatorConfig{PlanningDir: dir}}}

	if _, err := c.proposePlanningArtifact(runtime.ControlRequest{
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
			"execution_agent": "does-not-exist",
			"capabilities": ["repository_analysis"],
			"trust_requirement": "operator_trusted"
		}`)},
	}); err == nil {
		t.Fatal("a profile naming an unconfigured agent must be refused through the real composition path")
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "reviewer.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
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
