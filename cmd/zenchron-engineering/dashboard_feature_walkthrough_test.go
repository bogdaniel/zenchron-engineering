package main

import (
	"context"
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
	root, built, driver := walkthroughFixture(t, planOverrides(t, 41))
	runBrowserFixtureWithScript(t, root, built, driver, "check_dashboard_walkthrough.mjs", []string{"ZENCHRON_E2E_REACT=1"})
}

// TestDashboardDescribedObjective drives a REAL browser through describing an
// objective in words: the dashboard opens the GitHub issue after a dialog that
// states the write, and the new issue number flows into the proposal step.
func TestDashboardDescribedObjective(t *testing.T) {
	overrides := planOverrides(t, 41)
	// The fake already holds the issue the dashboard will "create", so the
	// proposal step can read it back without the double being written
	// concurrently.
	fake := overrides.GitHub.(*runtime.FakeGitHubAdapter)
	fake.Issues[42] = runtime.GitHubIssue{
		Number: 42, URL: "https://github.com/zenchron/seeded/issues/42", Title: "make the gadget idempotent",
		Body: "The gadget must be idempotent; prove it with a test.", State: runtime.GitHubOpen,
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(), Author: runtime.GitHubActor{Login: "operator", ID: 7},
	}
	overrides.GitHub = creatingForge{FakeGitHubAdapter: fake}
	root, built, driver := walkthroughFixture(t, overrides)
	runBrowserFixtureWithScript(t, root, built, driver, "check_described_objective.mjs", []string{"ZENCHRON_E2E_REACT=1"})
}

// creatingForge is a forge that can open the issue a described objective needs.
type creatingForge struct{ *runtime.FakeGitHubAdapter }

func (f creatingForge) CreateIssue(_ context.Context, repo runtime.GitHubRepo, _, _ string) (runtime.GitHubIssue, error) {
	return runtime.GitHubIssue{Number: 42, URL: "https://github.com/" + repo.String() + "/issues/42"}, nil
}

// walkthroughFixture is the isolated controller the browser cases drive: a
// temporary state directory, a held controller role and a re-adopted attested
// build, with the forge the caller supplies.
func walkthroughFixture(t *testing.T, overrides autonomyOverrides) (string, *composition, *runtime.Supervisor) {
	t.Helper()
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

	built, err := newComposition(autonomyFlags{Config: configPath}, overrides)
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
	return root, built, driver
}
