package main

// #481 operator-usability rehearsal. A real browser supervises a controlled
// objective through the Product Workspace against the REAL composition,
// SQLite store, controller socket, supervisor scheduler, plan reconciler and
// HTTP Control Plane. Only the execution providers, the forge and the agent
// CLIs' help/version output are faked, at their boundaries. Nothing here
// touches a live provider, GitHub or a production controller.
//
//	ZENCHRON_OPERATOR_REHEARSAL=1 ZENCHRON_PLAYWRIGHT_MODULE=<playwright-core/index.mjs> \
//	  ZENCHRON_E2E_ARTIFACTS=<dir> go test ./cmd/zenchron-engineering -run '^TestOperatorRehearsal$' -count=1 -v

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const rehearsalHandoff = `{"schema_version":"0.1","outcome":"completed","summary":"Implemented the change.","recommended_next":["add a benchmark"]}`

const rehearsalHelp = `Usage: agent [options]
  --sandbox <mode>             Sandbox (choices: "read-only", "workspace-write")
  --ignore-user-config         Ignore user config
  --cd <dir>                   Working directory
  --config <k=v>               Override
  --output-last-message <f>    Write last message
  --ask-for-approval <policy>  Approval policy
  --print                      Print mode
  --permission-mode <mode>     Permission mode (choices: "acceptEdits", "plan", "bypassPermissions")
  --model <model>              Model
  --safe-mode                  Safe mode
  --output-format <format>     Output format (choices: "text", "json", "stream-json")
  --verbose                    Verbose
`

// rehearsalWorker is one provider for every agent. Producers wait for the
// browser's release file so concurrent execution is observable, the backend
// producer asks a human decision once, and the reviewer answers through the
// runtime-owned verdict channel.
type rehearsalWorker struct {
	gates    string
	store    *runtime.SQLiteOperationStore
	reviewer *runtime.FakeReviewerProvider
	mu       sync.Mutex
	attempts map[string]int
	invoked  []string
}

func (w *rehearsalWorker) WritesTypedResults() bool { return true }

func (w *rehearsalWorker) Isolation() runtime.ProviderIsolation {
	return runtime.ProviderIsolation{FilesystemRead: runtime.IsolationProven, FilesystemWrite: runtime.IsolationProven,
		NetworkDenied: runtime.IsolationProven, CredentialScope: runtime.IsolationProven}
}

func (w *rehearsalWorker) stageOf(runID string) string {
	run, ok, err := w.store.Run(runID)
	if err != nil || !ok || run.Plan == nil {
		return ""
	}
	return run.Plan.StageID
}

func (w *rehearsalWorker) awaitRelease(ctx context.Context, stage string) error {
	for {
		if _, err := os.Stat(filepath.Join(w.gates, "go-"+stage)); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (w *rehearsalWorker) Execute(ctx context.Context, request runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	stage := w.stageOf(request.RunID)
	w.mu.Lock()
	w.invoked = append(w.invoked, stage+" reviewerChannel="+strconv.FormatBool(request.ReviewerResultPath != ""))
	w.mu.Unlock()
	if request.ReviewerResultPath != "" {
		return w.reviewer.Execute(ctx, request)
	}
	w.mu.Lock()
	w.attempts[request.RunID]++
	attempt := w.attempts[request.RunID]
	w.mu.Unlock()
	if stage == "backend" || stage == "frontend" {
		if err := w.awaitRelease(ctx, stage); err != nil {
			return runtime.ExecutionResult{}, err
		}
	}
	if stage == "backend" && attempt == 1 {
		document, err := json.Marshal(orchestration.MessageReport{SchemaVersion: orchestration.MessageSchemaVersion, Messages: []orchestration.MessageDraft{{
			Kind: orchestration.KindDecisionRequest, Purpose: "Which migration strategy for the backend half?",
			Body:                "Approach A keeps the old table (low risk, slower). Approach B migrates in place (faster, riskier).",
			ExpectedOutcomeKind: string(orchestration.DecisionSelectedOption), PermittedOptions: []string{"approach-a", "approach-b"},
		}}})
		if err != nil {
			return runtime.ExecutionResult{}, err
		}
		if err := os.WriteFile(request.MessagePath, document, 0o600); err != nil {
			return runtime.ExecutionResult{}, err
		}
	}
	file := filepath.Join(request.CandidateDir, "work-"+stage+"-"+strconv.Itoa(attempt)+".go")
	if err := os.WriteFile(file, []byte("package candidate\n"), 0o600); err != nil {
		return runtime.ExecutionResult{}, err
	}
	if request.HandoffPath != "" {
		if err := os.WriteFile(request.HandoffPath, []byte(rehearsalHandoff), 0o600); err != nil {
			return runtime.ExecutionResult{}, err
		}
	}
	return runtime.ExecutionResult{ProviderID: "rehearsal-" + stage, Outcome: execution.Succeeded, ChangedPaths: []string{filepath.Base(file)}}, nil
}

func writeFakeAgentCLI(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"" + name + " 1.0.0 (rehearsal stand-in)\" ;;\n  *) cat <<'EOF'\n" + rehearsalHelp + "EOF\n;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// rehearsalFixture is the isolated controller both rehearsal-shaped browser
// tests drive: the real composition, store, supervisor and reconciler, with only
// the providers, the forge and the agent CLIs' help output faked.
type rehearsalFixture struct {
	root, gates string
	built       *composition
	driver      *runtime.Supervisor
	worker      *rehearsalWorker
}

func newRehearsalFixture(t *testing.T) rehearsalFixture {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "zc-rehearsal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	gates := filepath.Join(state, "gates")
	bin := filepath.Join(state, "bin")
	for _, dir := range []string{gates, bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	origin := filepath.Join(state, "origin")
	if err := os.MkdirAll(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"config", "user.name", "rehearsal"}, {"config", "user.email", "rehearsal@example.test"}} {
		reactFixtureGit(t, origin, args...)
	}
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("rehearsal base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reactFixtureGit(t, origin, "add", "README.md")
	reactFixtureGit(t, origin, "commit", "--no-gpg-sign", "--quiet", "-m", "rehearsal base")
	base := reactFixtureGit(t, origin, "rev-parse", "HEAD")

	planning := t.TempDir()
	for name, body := range map[string]string{
		"profiles/rehearsal-producer-a.json": `{"execution_agent":"codex","capabilities":["code_change","repository_analysis"],"trust_requirement":"operator_trusted"}`,
		"profiles/rehearsal-producer-b.json": `{"execution_agent":"codex","capabilities":["code_change","repository_analysis"],"trust_requirement":"operator_trusted"}`,
		"profiles/rehearsal-integrator.json": `{"execution_agent":"codex","capabilities":["code_change","repository_analysis"],"trust_requirement":"operator_trusted"}`,
		"profiles/rehearsal-reviewer.json":   `{"execution_agent":"claude","capabilities":["repository_analysis"],"trust_requirement":"operator_trusted"}`,
		"templates/rehearsal-team.json": `{"description":"Two parallel producers, a deterministic integrator and an independent reviewer.","stages":[
		  {"id":"backend","kind":"agent","role":"implementer","profile":"rehearsal-producer-a"},
		  {"id":"frontend","kind":"agent","role":"implementer","profile":"rehearsal-producer-b"},
		  {"id":"integrate","kind":"agent","role":"integrator","execution_kind":"integration_compose","depends_on":["backend","frontend"],"profile":"rehearsal-integrator"},
		  {"id":"review","kind":"agent","role":"reviewer","depends_on":["integrate"],"profile":"rehearsal-reviewer",
		   "independence":{"dimension":"vendor_family","different_from":["backend","frontend","integrate"]}}]}`,
	} {
		writePlanningFile(t, planning, name, body)
	}
	codex, claude := writeFakeAgentCLI(t, bin, "codex"), writeFakeAgentCLI(t, bin, "claude")
	dir, config, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git", agentsConfig(map[string]any{
		"codex":  map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted", "command": codex},
		"claude": map[string]any{"kind": "claude_code", "trust_mode": "operator_trusted", "command": claude},
		// An installed-looking worker whose executable is absent: the Agents
		// page must show it as ineligible, not hide it.
		"gemini": map[string]any{"kind": "gemini_cli", "trust_mode": "operator_trusted", "command": "gemini-not-installed-rehearsal"},
	}, "codex"), func(c map[string]any) {
		c["state_dir"] = state
		c["operator"] = map[string]any{"id": "operator-1"}
		c["plan"] = map[string]any{"max_child_runs": 6, "max_concurrency": 2, "max_provider_invocations": 12}
		c["planning_dir"] = planning
		c["supervisor"] = map[string]any{"max_concurrent_runs": 3}
	})
	t.Chdir(dir)

	forge := planForge(t, 41)
	forge.Refs["main"] = base
	worker := &rehearsalWorker{gates: gates, attempts: map[string]int{},
		reviewer: runtime.NewFakeReviewerProvider(runtime.ReviewerResult{
			SchemaVersion: runtime.ReviewerResultSchemaVersion, Verdict: runtime.StageReviewAccepted, Reason: "every obligation was observed"})}
	built, err := newComposition(autonomyFlags{Config: config}, autonomyOverrides{
		GitHub: forge, Provider: worker, Assurance: newRehearsalAssurance(), SemanticAssurance: &runtime.FakeSemanticAssuranceProvider{},
		ControllerBuild: &runtime.ControllerBuild{Kind: runtime.ControllerUnattested},
		CloneRemote:     func(runtime.GitHubRepo) string { return origin },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	built.model, built.policy = humanPublicationGovernance(base)
	worker.store = built.store
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
	adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	adoptTestProduct(t, built, "checkout", []string{"zenchron/seeded"})
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	ticks, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	seen := map[string]bool{}
	go func() {
		defer close(done)
		for ticks.Err() == nil {
			if _, err := driver.Tick(ticks); err != nil && ticks.Err() == nil {
				t.Logf("tick: %v", err)
			}
			logRehearsalTransitions(t, built.store, seen)
			select {
			case <-ticks.Done():
			case <-time.After(700 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { stop(); <-done })
	return rehearsalFixture{root: root, gates: gates, built: built, driver: driver, worker: worker}
}

func TestOperatorRehearsal(t *testing.T) {
	if os.Getenv("ZENCHRON_OPERATOR_REHEARSAL") != "1" {
		t.Skip("opt-in: ZENCHRON_OPERATOR_REHEARSAL=1 with Chrome and Playwright")
	}
	f := newRehearsalFixture(t)
	runBrowserFixtureWithScript(t, f.root, f.built, f.driver, "operator_rehearsal.mjs", []string{
		"ZENCHRON_E2E_REACT=1", "ZENCHRON_REHEARSAL_GATES=" + f.gates,
	})
	logRehearsalRuns(t, f.built.store)
	t.Logf("provider invocations: %v", f.worker.invoked)
}

// logRehearsalTransitions logs each new (run, disposition, reason) once, so the
// browser evidence can be lined up against what the runtime was doing.
func logRehearsalTransitions(t *testing.T, store *runtime.SQLiteOperationStore, seen map[string]bool) {
	runs, err := store.Runs()
	if err != nil {
		return
	}
	for _, run := range runs {
		key := run.ID + string(run.Disposition) + run.Reason
		if seen[key] {
			continue
		}
		seen[key] = true
		stage := ""
		if run.Plan != nil {
			stage = run.Plan.StageID
		}
		t.Logf("transition %s %s -> %s/%s", time.Now().Format("15:04:05"), stage, run.Disposition, run.Reason)
	}
}

// logRehearsalRuns records each run's durable disposition and reason beside
// the browser evidence, so a stalled step can be explained from the journal.
func logRehearsalRuns(t *testing.T, store *runtime.SQLiteOperationStore) {
	t.Helper()
	runs, err := store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		stage := ""
		if run.Plan != nil {
			stage = run.Plan.StageID
		}
		t.Logf("run %s stage=%s disposition=%s reason=%q", run.ID, stage, run.Disposition, run.Reason)
		events, err := store.Events(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		types := make([]string, 0, len(events))
		for _, event := range events {
			types = append(types, string(event.Type))
		}
		t.Logf("run %s journal: %s", run.ID, strings.Join(types, " "))
	}
}

// rehearsalAssurance answers every verification passing, serialised because
// concurrent producers share one provider.
type rehearsalAssurance struct {
	mu    sync.Mutex
	inner *runtime.FakeAssuranceProvider
}

func newRehearsalAssurance() *rehearsalAssurance {
	passing := make([]runtime.AssuranceResult, 64)
	for i := range passing {
		passing[i] = runtime.AssuranceResult{ProviderID: "rehearsal-independent-verifier", VerifierDefinition: "verifier-v1", Passed: true}
	}
	return &rehearsalAssurance{inner: &runtime.FakeAssuranceProvider{Results: passing}}
}

func (a *rehearsalAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return a.inner.ProducedEvidenceClasses()
}

func (a *rehearsalAssurance) Assure(ctx context.Context, request runtime.AssuranceRequest) (runtime.AssuranceResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inner.Assure(ctx, request)
}
