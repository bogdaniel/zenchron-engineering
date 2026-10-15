package runtime

// Whether a governed registry reload (ADR-0008) actually reaches the
// background plan reconciler, not only an operator's next plan proposal.

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// A plan may be approved with a stage pinning a profile nobody has installed
// yet (the dashboard deliberately allows this; the stage simply blocks). An
// operator who then authors that profile and reloads the registry (ADR-0008)
// expects the stage to become resolvable without restarting serve - that is
// the entire point of a reload verb existing. SupervisorDependencies.Plans is
// a startup snapshot, so without PlanRegistry wired the background
// reconciler keeps resolving against it forever; this proves both halves.
func TestPlanReconcilerResolvesTheLiveRegistryNotTheStartupSnapshot(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{{
		ID: "impl", Kind: domain.StageAgent, Role: domain.RoleImplementer,
		Objective:            "Implement the change.",
		Profile:              "late-profile",
		RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange},
		InvocationMode:       domain.InvocationModeMutating,
	}})
	fixture.approve(t)

	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	baseDeps := SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:          OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:      []GitHubRepo{repo},
		MaxConcurrentRuns: 2, PollInterval: time.Minute,
		Agents: supervisorRegistry(t), Plans: fixture.service,
		Runtime: func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) { return fixture.runtime, nil },
	}

	stale, err := NewSupervisor(baseDeps)
	if err != nil {
		t.Fatal(err)
	}
	report, err := stale.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plans) != 1 || len(report.Plans[0].Started) != 0 {
		t.Fatalf("a profile that was never installed must stay blocked: %#v", report.Plans)
	}

	// Author "late-profile" exactly as the dashboard's governed write does,
	// then make it visible exactly as ControlPlanningReload does.
	dir := t.TempDir()
	if _, err := planning.WriteAgentProfile(dir, "late-profile", []byte(`{
		"execution_agent": "codex",
		"capabilities": ["code_change"],
		"trust_requirement": "operator_trusted"
	}`), nil); err != nil {
		t.Fatal(err)
	}
	reloaded, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}

	withoutReload := baseDeps
	withoutReload.PlanRegistry = nil
	stillStale, err := NewSupervisor(withoutReload)
	if err != nil {
		t.Fatal(err)
	}
	report, err = stillStale.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plans) != 1 || len(report.Plans[0].Started) != 0 {
		t.Fatalf("without PlanRegistry wired, a reload must not reach the reconciler: %#v", report.Plans)
	}

	withReload := baseDeps
	withReload.PlanRegistry = func() planning.Registry { return reloaded }
	fresh, err := NewSupervisor(withReload)
	if err != nil {
		t.Fatal(err)
	}
	report, err = fresh.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plans) != 1 || len(report.Plans[0].Started) != 1 {
		t.Fatalf("a profile reloaded after approval must become resolvable without a restart: %#v", report.Plans)
	}
}
