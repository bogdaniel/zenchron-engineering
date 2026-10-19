package main

// #639 journey: a plan proposed the way `autonomy plan issue N` proposes it
// (no product, no browser intake) must be discoverable and approvable from the
// Product Workspace with no hand-copied digest. Opt-in, like the #481
// rehearsal, because it needs a real Chrome:
//
//	ZENCHRON_OPERATOR_REHEARSAL=1 ZENCHRON_PLAYWRIGHT_MODULE=<playwright-core/index.mjs> \
//	  go test ./cmd/zenchron-engineering -run '^TestOperatorPlanApprovalJourney$' -count=1 -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestOperatorPlanApprovalJourney(t *testing.T) {
	if os.Getenv("ZENCHRON_OPERATOR_REHEARSAL") != "1" {
		t.Skip("opt-in: ZENCHRON_OPERATOR_REHEARSAL=1 with Chrome and Playwright")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "zc-approval-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	bin := filepath.Join(state, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	codex := writeFakeAgentCLI(t, bin, "codex")
	dir, config, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git", agentsConfig(map[string]any{
		"codex": map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted", "command": codex},
	}, "codex"), func(c map[string]any) {
		c["state_dir"] = state
		c["operator"] = map[string]any{"id": "operator-1"}
		c["plan"] = map[string]any{"max_child_runs": 6, "max_concurrency": 2, "max_provider_invocations": 12}
	})
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	built.model, built.policy = humanPublicationGovernance(planForge(t, 41).Refs["main"])
	role, err := runtime.AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = role.Release() })
	built.role = role
	attestedBuild(t)
	self, err := controllerSelf()
	if err != nil {
		t.Fatal(err)
	}
	governs(t, built, self)
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	// The request `autonomy plan issue 41` sends to a running supervisor
	// (plan.go delegatePlanRevision): no product, no expected controller.
	response := built.handleControl(context.Background(), driver, func() {}, runtime.ControlRequest{
		Command: runtime.ControlPlanRevise, Issue: 41, Deterministic: true,
		Repository: "zenchron/seeded", DefaultBranch: "main", Operator: "operator-1", ObjectiveWorkflow: true,
	})
	if !response.OK {
		t.Fatalf("CLI-shaped proposal refused: %+v", response)
	}
	runBrowserFixtureWithScript(t, root, built, driver, "operator_plan_approval.mjs", []string{"ZENCHRON_E2E_REACT=1"})
}
