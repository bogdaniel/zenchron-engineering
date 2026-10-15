package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestDashboardFeatureWalkthrough drives a REAL browser through the exact
// operator sequence #471/#479's goal names - create a product, author an
// agent profile and a plan template, propose and approve an objective
// against them, and see the resulting WorkGraph - entirely through the
// React app's own forms. Nothing here is pre-seeded through Go: the browser
// creates every artifact itself, the same way an operator would.
//
// Opt in on a host with Chrome and a local Playwright install (never a
// project dependency - point ZENCHRON_PLAYWRIGHT_MODULE at your own):
//
//	ZENCHRON_REACT_BROWSER_E2E=1 \
//	ZENCHRON_PLAYWRIGHT_MODULE=/path/to/node_modules/playwright/index.mjs \
//	go test ./cmd/zenchron-engineering -run '^TestDashboardFeatureWalkthrough$' -v
func TestDashboardFeatureWalkthrough(t *testing.T) {
	if os.Getenv("ZENCHRON_REACT_BROWSER_E2E") != "1" {
		t.Skip("opt-in browser host required")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err := os.MkdirTemp("/tmp", "zc-walkthrough-")
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
	// Readiness is an observation: the configured agent is reported
	// unavailable until its credential can be inspected. This content is
	// never read as a real credential, only its presence checked - the same
	// boundary fixture every other browser E2E test here uses.
	if err := os.WriteFile(built.agent.CredentialPath, []byte("unused-walkthrough-boundary-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	role, err := runtime.AcquireControllerRole(stateDir)
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
	build := self.Build
	if _, err := built.store.ReadoptController(runtime.ControllerReadoption{
		ID: "dashboard-walkthrough", Reason: "isolated browser acceptance",
		Binding: runtime.ControllerBinding{Controller: "test", Build: &build, Config: built.config.Digest}, RecordedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	runBrowserFixtureWithScript(t, root, built, driver, "check_dashboard_walkthrough.mjs", []string{"ZENCHRON_E2E_REACT=1"})
}
