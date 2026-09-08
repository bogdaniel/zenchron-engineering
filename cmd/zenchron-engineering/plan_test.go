package main

// The operator's plan surface, driven through the real composition root: real
// configuration, real SQLite state, real journal. What is faked is the forge
// and the execution provider, because neither of those may be contacted by a
// test.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// planForge is a forge holding one issue and one branch, which is everything a
// plan proposal reads.
func planForge(t *testing.T, issue int) *runtime.FakeGitHubAdapter {
	t.Helper()
	forge := runtime.NewFakeGitHubAdapter()
	forge.Issues[issue] = runtime.GitHubIssue{
		Number: issue, URL: "https://github.com/zenchron/seeded/issues/41",
		Title: "make the widget idempotent", Body: "The widget must be idempotent.",
		State: runtime.GitHubOpen, UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Author: runtime.GitHubActor{Login: "operator", ID: 7},
	}
	forge.Refs["main"] = strings.Repeat("a", 40)
	return forge
}

func planOverrides(t *testing.T, issue int) autonomyOverrides {
	t.Helper()
	return autonomyOverrides{
		GitHub:          planForge(t, issue),
		Provider:        &refusingPlanner{},
		ControllerBuild: &runtime.ControllerBuild{Kind: runtime.ControllerUnattested},
	}
}

// refusingPlanner models a provider with no provable non-mutating mode. It is
// the default in these tests so nothing can accidentally depend on a model
// answering.
type refusingPlanner struct{}

func (p *refusingPlanner) Execute(_ context.Context, request runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	return runtime.ExecutionResult{}, &runtime.InvocationModeUnsupportedError{
		AgentID: "test", Kind: "test_provider", Mode: request.Mode,
	}
}

func (p *refusingPlanner) Isolation() runtime.ProviderIsolation {
	return runtime.ProviderIsolation{
		FilesystemRead: runtime.IsolationProven, FilesystemWrite: runtime.IsolationProven,
		NetworkDenied: runtime.IsolationProven, CredentialScope: runtime.IsolationProven,
	}
}

func planWorkspace(t *testing.T) (dir, configPath string) {
	t.Helper()
	// An operator-owned customization directory, so the composition under test
	// is the one an operator who defined a profile actually has.
	planningDir := t.TempDir()
	writePlanningFile(t, planningDir, "instructions/review.json", `{"instructions": ["Review the diff; do not implement."]}`)
	writePlanningFile(t, planningDir, "profiles/zenchron-reviewer.json", `{
	  "execution_agent": "openai-responses", "capabilities": ["repository_analysis", "security_review"],
	  "instructions": ["review"], "trust_requirement": "protected"
	}`)
	dir, configPath, _ = seededWorkspace(t, "https://github.com/zenchron/seeded.git", func(config map[string]any) {
		config["operator"] = map[string]any{"id": "operator-1"}
		config["plan"] = map[string]any{"max_child_runs": 4, "max_concurrency": 2, "max_provider_invocations": 8}
		config["planning_dir"] = planningDir
	})
	return dir, configPath
}

func writePlanningFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A deterministic proposal compiles with NO model invocation, records itself
// awaiting approval, and says plainly that nothing executes yet.
func TestPlanProposalAwaitsApproval(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "issue", "41", "--deterministic", "--text", "--config", configPath},
		planOverrides(t, 41), &out)
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out.String())
	}
	if code != runtime.ExitWaiting {
		t.Fatalf("exit = %d, want the waiting status for a plan nobody has approved", code)
	}
	printed := out.String()
	for _, want := range []string{
		"proposed plan",
		"planned by: the deterministic compiler; no model was invoked",
		"nothing executes until this revision is approved",
		"cost: unknown",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("output does not state %q:\n%s", want, printed)
		}
	}
}

// Approval is an operator act against ONE exact revision, and it is durable.
func TestPlanApprovalIsRecordedAgainstTheExactRevision(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	planID := proposePlan(t, configPath, 41)

	var approved bytes.Buffer
	code, err := autonomy([]string{"plan", "approve", planID, "--note", "read it", "--text", "--config", configPath},
		planOverrides(t, 41), &approved)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("approve: code=%d err=%v\n%s", code, err, approved.String())
	}
	if !strings.Contains(approved.String(), "approved by operator-1") {
		t.Fatalf("approval output = %q", approved.String())
	}

	var shown bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", planID, "--text", "--config", configPath},
		planOverrides(t, 41), &shown); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(shown.String(), "(approved)") {
		t.Fatalf("show does not report the approval:\n%s", shown.String())
	}
	if strings.Contains(shown.String(), "nothing executes until") {
		t.Fatalf("an approved plan still says it is waiting:\n%s", shown.String())
	}
}

// A provider that cannot prove a non-mutating mode is REFUSED, with the reason.
// It is never run in its ordinary editing mode to produce a plan.
func TestPlanningRefusesAProviderWithoutANonMutatingMode(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "issue", "41", "--text", "--config", configPath},
		planOverrides(t, 41), &out)
	if err == nil {
		t.Fatalf("a provider with no non-mutating mode produced a plan: %s", out.String())
	}
	if !strings.Contains(err.Error(), "cannot perform a \"non_mutating_planning\" invocation") {
		t.Fatalf("refusal = %v", err)
	}
	if code == runtime.ExitCompleted {
		t.Fatalf("a refused planning invocation exited %d", code)
	}
}

// An unknown plan is a subject that does not exist, not an operational failure.
func TestUnknownPlanIsReportedAsMissing(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "show", "plan-nope", "--config", configPath},
		planOverrides(t, 41), &out)
	if err == nil {
		t.Fatal("an unknown plan was shown")
	}
	if code != exitRunNotFound {
		t.Fatalf("exit = %d, want %d", code, exitRunNotFound)
	}
}

// The plan list is the fleet view for plans.
func TestPlanListShowsProposedPlans(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)

	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "list", "--text", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), planID) || !strings.Contains(out.String(), "pending") {
		t.Fatalf("list output = %q", out.String())
	}
}

// proposePlan runs a deterministic proposal and returns the plan id.
func proposePlan(t *testing.T, configPath string, issue int) string {
	t.Helper()
	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "issue", "41", "--deterministic", "--config", configPath},
		planOverrides(t, issue), &out); err != nil {
		t.Fatalf("propose: %v\n%s", err, out.String())
	}
	var view struct {
		Plan struct {
			ID string `json:"id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("decode plan view: %v\n%s", err, out.String())
	}
	if view.Plan.ID == "" {
		t.Fatalf("proposal returned no plan id:\n%s", out.String())
	}
	return view.Plan.ID
}

// Every engine this composition builds carries the operator's customization
// registry. A plan stage run resolves its frozen instruction packs through it,
// and an engine built without it refuses work the operator approved on the
// grounds that a pack it was never given is "no longer installed" - which is
// exactly what the first live dogfood run did.
func TestEveryEngineCarriesTheCustomizationRegistry(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	built, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	defer built.release()

	if built.planning.Dir() == "" {
		t.Fatal("the composition loaded no planning registry at all")
	}
	target := runtime.RepositoryTarget{
		Identity: "zenchron/seeded", Remote: "https://github.com/zenchron/seeded.git", DefaultBranch: "main",
	}
	for _, agent := range built.agents.All() {
		engine, err := built.engineFor(target, agent)
		if err != nil {
			t.Fatalf("engine for %s: %v", agent.ID, err)
		}
		if engine.PlanningRegistry().Dir() != built.planning.Dir() {
			t.Fatalf("the engine for %s was built without the customization registry", agent.ID)
		}
	}
}
