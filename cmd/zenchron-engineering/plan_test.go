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
	"strconv"
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
	return planWorkspaceIn(t, "")
}

// planWorkspaceIn is the same workspace with an operator-chosen state
// directory, for the tests that need one short enough for a socket address.
func planWorkspaceIn(t *testing.T, stateDir string) (dir, configPath string) {
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
		if stateDir != "" {
			config["state_dir"] = stateDir
		}
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
		"nothing executes until it is approved: `autonomy plan approve",
		"--revision 1 --digest ",
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

	revision, digest := pendingDecision(t, configPath, planID, 41)
	var approved bytes.Buffer
	code, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(revision), "--digest", digest,
		"--note", "read it", "--text", "--config", configPath},
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
// pendingDecision is the revision an operator is being asked about, with its
// digest - what `plan show` prints and what a decision has to name.
func pendingDecision(t *testing.T, configPath, planID string, issue int) (int, string) {
	t.Helper()
	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", planID, "--config", configPath},
		planOverrides(t, issue), &out); err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	var view struct {
		Snapshot struct {
			Approval struct {
				Revision int    `json:"revision"`
				Digest   string `json:"digest"`
			} `json:"approval"`
		} `json:"state"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("decode plan view: %v\n%s", err, out.String())
	}
	if view.Snapshot.Approval.Revision == 0 {
		t.Fatalf("plan show reports no revision awaiting a decision:\n%s", out.String())
	}
	return view.Snapshot.Approval.Revision, view.Snapshot.Approval.Digest
}

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

// An operator's decision is about the revision they were ASKED about. A
// decomposition proposal is stored as a new unapproved revision while the
// approved one keeps executing, so a decision that targeted the governing
// revision would answer a question nobody asked.
func TestApprovalTargetsTheRevisionAwaitingADecision(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)

	// Approve revision 1, then propose revision 2 by re-planning.
	first, firstDigest := pendingDecision(t, configPath, planID, 41)
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(first), "--digest", firstDigest, "--config", configPath},
		planOverrides(t, 41), &bytes.Buffer{}); err != nil {
		t.Fatalf("approve r1: %v", err)
	}
	proposePlan(t, configPath, 41)

	second, secondDigest := pendingDecision(t, configPath, planID, 41)
	if second != 2 {
		t.Fatalf("the plan is awaiting a decision on revision %d, want 2", second)
	}
	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(second), "--digest", secondDigest,
		"--text", "--config", configPath}, planOverrides(t, 41), &out); err != nil {
		t.Fatalf("approve r2: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "revision 2 approved") {
		t.Fatalf("the decision did not target the revision awaiting one: %q", out.String())
	}
}

// A decision names what the operator READ. Deciding without naming it, or
// naming a revision that has since been replaced, is refused rather than
// silently redirected onto whatever is newest - `serve` stores decomposition
// proposals as new unapproved revisions on its own, so the window is real.
func TestADecisionMustNameTheRevisionTheOperatorRead(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)
	revision, digest := pendingDecision(t, configPath, planID, 41)

	var bare bytes.Buffer
	code, err := autonomy([]string{"plan", "approve", planID, "--config", configPath}, planOverrides(t, 41), &bare)
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("an approval that named no revision was accepted: code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "--revision") || !strings.Contains(err.Error(), "--digest") {
		t.Fatalf("the refusal does not say what to run: %v", err)
	}

	// A digest from a revision that is no longer the one awaiting a decision is
	// refused by the service, not quietly applied to the newer one.
	proposePlan(t, configPath, 41)
	var stale bytes.Buffer
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(revision + 1), "--digest", digest,
		"--config", configPath}, planOverrides(t, 41), &stale); err == nil {
		t.Fatal("an approval carrying a digest from another revision was recorded")
	}
}

// The operator plan lifecycle works WHILE a supervisor owns the state
// directory. That is the whole point of the approval boundary: `serve`
// reconciles the plan, a decomposition proposes a material revision, and a
// person decides it without stopping the supervisor.
//
// The runtime ownership lock is per-OWNER liveness evidence - its path carries
// the owner identity - so it is not an exclusion lock between processes. This
// test holds one as a live supervisor would and drives the real CLI entry
// points against the same state directory.
func TestThePlanLifecycleWorksWhileASupervisorOwnsTheStateDirectory(t *testing.T) {
	// A SHORT state directory, because the control endpoint is a Unix socket
	// and its address is bounded by the operating system. `serve` refuses a
	// state_dir that would exceed it, with that message; a test that used the
	// default temporary path would be testing that refusal instead.
	stateDir, err := os.MkdirTemp("", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)
	// A REAL supervisor: the same composition `serve` builds - same
	// newComposition, same store, same ownership lock - AND the same
	// owner-only control endpoint it listens on. Both halves matter: the
	// ownership is what a plan command used to collide with, and the endpoint
	// is where a decision about work this process owns now goes.
	supervisor, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatalf("a supervisor composition could not be built: %v", err)
	}
	t.Cleanup(supervisor.release)
	listener, err := runtime.ListenControl(supervisor.config.StateDir)
	if err != nil {
		t.Fatalf("the supervisor could not listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	// A real Supervisor too: an operator decision is applied under the same
	// lock the plan reconciler holds, so the handler needs the supervisor that
	// owns it rather than a stand-in.
	driver, err := supervisor.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}})
	if err != nil {
		t.Fatalf("the supervisor could not be built: %v", err)
	}
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return supervisor.handleControl(context.Background(), driver, func() {}, request)
		})
	}()

	var shown bytes.Buffer
	if code, err := autonomy([]string{"plan", "show", planID, "--text", "--config", configPath},
		planOverrides(t, 41), &shown); err != nil {
		t.Fatalf("plan show while a supervisor owns the state dir: code=%d err=%v", code, err)
	}
	if !strings.Contains(shown.String(), "plan "+planID) {
		t.Fatalf("plan show printed no plan:\n%s", shown.String())
	}

	revision, digest := pendingDecision(t, configPath, planID, 41)
	var approved bytes.Buffer
	if code, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(revision),
		"--digest", digest, "--text", "--config", configPath}, planOverrides(t, 41), &approved); err != nil {
		t.Fatalf("plan approve while a supervisor owns the state dir: code=%d err=%v\n%s", code, err, approved.String())
	}
	if !strings.Contains(approved.String(), "approved by") {
		t.Fatalf("approval output = %q", approved.String())
	}

	// And the decision is durable for the supervisor's next tick to read.
	var status bytes.Buffer
	if _, err := autonomy([]string{"plan", "status", planID, "--text", "--config", configPath},
		planOverrides(t, 41), &status); err != nil {
		t.Fatalf("plan status: %v", err)
	}
	if !strings.Contains(status.String(), "(approved)") {
		t.Fatalf("the approval is not visible to a reader:\n%s", status.String())
	}
}

// An operator can read the revision they are being ASKED about, not only the
// one that governs. A decomposition proposal is stored as a new unapproved
// revision while the approved one keeps executing, and being asked to decide
// something unreadable is not a decision.
func TestPlanShowRendersOneExactRevision(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)
	first, firstDigest := pendingDecision(t, configPath, planID, 41)
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(first),
		"--digest", firstDigest, "--config", configPath}, planOverrides(t, 41), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	proposePlan(t, configPath, 41)

	var governing bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", planID, "--text", "--config", configPath},
		planOverrides(t, 41), &governing); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(governing.String(), "revision 1 (approved)") {
		t.Fatalf("the default view does not show the governing revision:\n%s", governing.String())
	}

	var pending bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", planID, "--revision", "2", "--text", "--config", configPath},
		planOverrides(t, 41), &pending); err != nil {
		t.Fatalf("show --revision 2: %v", err)
	}
	if !strings.Contains(pending.String(), "revision 2") {
		t.Fatalf("--revision 2 did not render revision 2:\n%s", pending.String())
	}

	var missing bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", planID, "--revision", "9", "--config", configPath},
		planOverrides(t, 41), &missing); err == nil {
		t.Fatal("a revision that does not exist was rendered")
	}
}

// A decision records WHO made it, not which process applied it.
//
// The supervisor resolves its own operator identity from its own configuration
// and account; a decision delegated to it would therefore be journalled under
// the supervisor - a service account, or another person's login - for a
// decision somebody else made in their terminal.
func TestADelegatedDecisionRecordsTheRequester(t *testing.T) {
	stateDir, err := os.MkdirTemp("", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)

	supervisor, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.release)
	// The supervisor's own identity differs from the requester's, which is what
	// makes the difference observable at all.
	supervisor.config.OperatorConfig.Operator.ID = "the-supervisors-service-account"
	listener, err := runtime.ListenControl(supervisor.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	driver, err := supervisor.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return supervisor.handleControl(context.Background(), driver, func() {}, request)
		})
	}()

	revision, digest := pendingDecision(t, configPath, planID, 41)
	var approved bytes.Buffer
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(revision),
		"--digest", digest, "--text", "--config", configPath}, planOverrides(t, 41), &approved); err != nil {
		t.Fatalf("approve: %v\n%s", err, approved.String())
	}
	if !strings.Contains(approved.String(), "operator-1") {
		t.Fatalf("the decision was not recorded against the requester: %q", approved.String())
	}
	if strings.Contains(approved.String(), "service-account") {
		t.Fatalf("the decision was recorded against the supervisor: %q", approved.String())
	}
}

// A first plan can be started while `serve` owns the state directory.
//
// Delegation was only attempted for a plan that already existed, on the
// reasoning that a first proposal races nothing. Racing was never the obstacle:
// the local path builds a composition, and a composition takes the exclusive
// ownership lock a running `serve` already holds. So an operator could revise
// and decide plans while the persistent runtime ran, and could not START one
// without stopping it - which is the runtime's whole point.
func TestAFirstPlanIsProposedThroughARunningSupervisor(t *testing.T) {
	stateDir, err := os.MkdirTemp("", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)

	// A real supervisor, holding the ownership lock and answering on the
	// control endpoint, exactly as `serve` leaves it.
	supervisor, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.release)
	listener, err := runtime.ListenControl(supervisor.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	driver, err := supervisor.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return supervisor.handleControl(context.Background(), driver, func() {}, request)
		})
	}()

	var proposed bytes.Buffer
	if _, err := autonomy([]string{"plan", "issue", "41", "--deterministic", "--config", configPath},
		planOverrides(t, 41), &proposed); err != nil {
		t.Fatalf("a first proposal was refused while a supervisor was running: %v\n%s", err, proposed.String())
	}

	// It is a real plan, in the supervisor's own store, and it can be decided
	// through the same running supervisor.
	var view struct {
		Plan struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(proposed.Bytes(), &view); err != nil {
		t.Fatalf("proposal output is not a plan view: %v\n%s", err, proposed.String())
	}
	if view.Plan.ID == "" || view.Plan.Digest == "" {
		t.Fatalf("the proposal named no plan: %s", proposed.String())
	}
	var approved bytes.Buffer
	if _, err := autonomy([]string{"plan", "approve", view.Plan.ID, "--revision", strconv.Itoa(view.Plan.Revision),
		"--digest", view.Plan.Digest, "--text", "--config", configPath}, planOverrides(t, 41), &approved); err != nil {
		t.Fatalf("approve: %v\n%s", err, approved.String())
	}
	if !strings.Contains(approved.String(), "approved") {
		t.Fatalf("the plan proposed through the supervisor could not be approved: %q", approved.String())
	}
}
