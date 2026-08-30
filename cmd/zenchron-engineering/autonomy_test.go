package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestMain doubles as the CLI entry point. When ZENCHRON_CLI_HELPER is set the
// test binary re-enters main() with the requested argv, so an exit-code
// assertion observes the REAL process status this program produced - not a
// value a handler returned to a test.
func TestMain(m *testing.M) {
	if os.Getenv("ZENCHRON_CLI_HELPER") == "1" {
		args := []string{"zenchron-engineering"}
		if raw := os.Getenv("ZENCHRON_CLI_ARGS"); raw != "" {
			args = append(args, strings.Split(raw, "\x1f")...)
		}
		// ZENCHRON_CLI_OFFLINE re-enters the SAME dispatcher main() calls, with
		// the forge, the provider and the verifier replaced by fakes and the
		// watch controller replaced by one that only idles. Everything under
		// test - configuration loading, the durable store, the ownership lock,
		// signal handling, the shutdown path and the process exit status - is
		// still the real thing running in a real child process.
		if os.Getenv("ZENCHRON_CLI_OFFLINE") == "1" {
			if len(args) < 2 || args[1] != "autonomy" {
				fmt.Fprintln(os.Stderr, "the offline helper only serves autonomy subcommands")
				os.Exit(exitUsage)
			}
			overrides := offlineOverrides()
			// A scripted authority boundary, so the process status a REFUSED
			// authorization produces is observable as a real exit code.
			if os.Getenv("ZENCHRON_CLI_STALE_AUTHORIZE") == "1" {
				overrides.Runtime = &scriptedRuntime{
					runID: "run-scripted",
					authorizeErr: &runtime.AuthorityRefusedError{
						Code:   runtime.RefusedStaleRequest,
						Detail: "request authreq-old is not the run's current request",
					},
				}
			}
			// The first tick reports the seeded run as one this watcher is
			// DRIVING. That is what makes the shutdown assertion sharp: a
			// shutdown path that cancelled the runs it was driving would have
			// this exact run to cancel.
			overrides.Watch = &scriptedWatch{reports: []runtime.TickReport{{
				ActiveRuns:     1,
				NextEligibleAt: time.Now().Add(time.Hour),
				Repositories: []runtime.RepositoryWatchReport{{
					Repository: runtime.GitHubRepo{Owner: "zenchron", Name: "seeded"},
					Discovered: 1,
					Driven:     []string{os.Getenv("ZENCHRON_CLI_WATCHED_RUN")},
				}},
			}}}
			code, err := autonomy(args[2:], overrides, os.Stdout)
			if err != nil {
				fmt.Fprintln(os.Stderr, "zenchron-engineering:", err)
			}
			os.Exit(code)
		}
		os.Args = args
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI executes this program as a child process and returns its real exit
// status.
func runCLI(t *testing.T, dir string, args ...string) (int, string) {
	return runCLIEnv(t, dir, nil, args...)
}

// runCLIEnv is the same re-exec with extra environment, which is how the
// offline helper's boundaries are selected for one case.
func runCLIEnv(t *testing.T, dir string, env []string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=ZenchronCLIHelperNoTest")
	command.Dir = dir
	command.Env = append(os.Environ(), "ZENCHRON_CLI_HELPER=1", "ZENCHRON_CLI_ARGS="+strings.Join(args, "\x1f"))
	command.Env = append(command.Env, env...)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), output.String()
	}
	if err != nil {
		t.Fatalf("running the CLI failed: %v\n%s", err, output.String())
	}
	return 0, output.String()
}

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

type scriptedRuntime struct {
	runID    string
	outcome  runtime.Outcome
	events   []runtime.EngineeringEvent
	report   runtime.StatusReport
	startErr error
	issues   []int
	runIDs   []string

	request         *runtime.AuthorityRequest
	requestErr      error
	authorizeResult runtime.AuthorizeResult
	authorizeErr    error
	authorized      []runtime.AuthorizeInput
}

func (s *scriptedRuntime) StartOrResumeIssueRun(_ context.Context, issue int) (string, error) {
	s.issues = append(s.issues, issue)
	return s.runID, s.startErr
}

func (s *scriptedRuntime) Reconcile(_ context.Context, runID string) (runtime.Outcome, error) {
	s.runIDs = append(s.runIDs, runID)
	outcome := s.outcome
	outcome.RunID = runID
	return outcome, nil
}

func (s *scriptedRuntime) Status(string) (runtime.StatusReport, error) { return s.report, nil }

func (s *scriptedRuntime) Journal(string) ([]runtime.EngineeringEvent, error) {
	return s.events, nil
}

func (s *scriptedRuntime) PendingAuthorityRequest(string) (*runtime.AuthorityRequest, error) {
	return s.request, s.requestErr
}

func (s *scriptedRuntime) Authorize(_ context.Context, in runtime.AuthorizeInput) (runtime.AuthorizeResult, error) {
	s.authorized = append(s.authorized, in)
	if s.authorizeErr != nil {
		return runtime.AuthorizeResult{}, s.authorizeErr
	}
	result := s.authorizeResult
	result.RunID = in.RunID
	return result, nil
}

// provenProvider is a fake execution provider that states a fully proven
// boundary, which is what the composition root requires before it will use one.
type provenProvider struct{ *runtime.FakeExecutionProvider }

func (provenProvider) Isolation() runtime.ProviderIsolation {
	return runtime.ProviderIsolation{
		FilesystemRead:  runtime.IsolationProven,
		FilesystemWrite: runtime.IsolationProven,
		NetworkDenied:   runtime.IsolationProven,
		CredentialScope: runtime.IsolationProven,
	}
}

func offlineOverrides() autonomyOverrides {
	return autonomyOverrides{
		GitHub:    runtime.NewFakeGitHubAdapter(),
		Provider:  provenProvider{FakeExecutionProvider: &runtime.FakeExecutionProvider{}},
		Assurance: &runtime.FakeAssuranceProvider{},
	}
}

// ---------------------------------------------------------------------------
// Exit codes
// ---------------------------------------------------------------------------

func TestReconcileOutcomeDrivesExitCode(t *testing.T) {
	for _, testCase := range []struct {
		disposition runtime.Disposition
		want        int
	}{
		{runtime.Completed, 0},
		{runtime.Waiting, 10},
		{runtime.Failed, 11},
		{runtime.Cancelled, 12},
	} {
		engine := &scriptedRuntime{runID: "run-1", outcome: runtime.Outcome{Disposition: testCase.disposition, Reason: "scripted"}}
		var out bytes.Buffer
		code, err := autonomy([]string{"run", "issue", "29"}, autonomyOverrides{Runtime: engine}, &out)
		if err != nil {
			t.Fatalf("%s: %v", testCase.disposition, err)
		}
		if code != testCase.want {
			t.Fatalf("%s: exit code %d, want %d", testCase.disposition, code, testCase.want)
		}
		if len(engine.issues) != 1 || engine.issues[0] != 29 {
			t.Fatalf("%s: issue was not forwarded: %v", testCase.disposition, engine.issues)
		}
		if !strings.Contains(out.String(), "scripted") {
			t.Fatalf("%s: outcome was not reported: %s", testCase.disposition, out.String())
		}
	}
}

func TestResumeReconcilesTheNamedRun(t *testing.T) {
	engine := &scriptedRuntime{outcome: runtime.Outcome{Disposition: runtime.Waiting}}
	code, err := autonomy([]string{"resume", "run-7"}, autonomyOverrides{Runtime: engine}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if code != 10 {
		t.Fatalf("exit code %d, want 10", code)
	}
	if len(engine.runIDs) != 1 || engine.runIDs[0] != "run-7" {
		t.Fatalf("resume did not reconcile the named run: %v", engine.runIDs)
	}
	if len(engine.issues) != 0 {
		t.Fatal("resume must not start a new run")
	}
}

func TestUsageAndInvalidInputExitInvalid(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nonsense"},
		{"run"},
		{"run", "issue"},
		{"run", "issue", "zero"},
		{"run", "issue", "0"},
		{"status"},
		{"events", "run-1", "--unknown", "x"},
		{"status", "run-1", "--repo"},
		{"stop"},
		{"stop", " "},
		{"watch", "--unknown", "x"},
		{"watch", "--config"},
	} {
		code, err := autonomy(args, autonomyOverrides{Runtime: &scriptedRuntime{}}, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("%v: expected a refusal", args)
		}
		if code != runtime.ExitInvalid {
			t.Fatalf("%v: exit code %d, want %d", args, code, runtime.ExitInvalid)
		}
	}
}

func TestEventsPrintsThePersistedJournalVerbatim(t *testing.T) {
	engine := &scriptedRuntime{events: []runtime.EngineeringEvent{
		{SchemaVersion: runtime.SchemaVersion, ID: "e1", RunID: "run-1", Sequence: 1, Type: runtime.EventRunCreated},
		{SchemaVersion: runtime.SchemaVersion, ID: "e2", RunID: "run-1", Sequence: 2, Type: runtime.EventRunFailed, Payload: json.RawMessage(`{"reason":"assurance"}`)},
	}}
	var out bytes.Buffer
	code, err := autonomy([]string{"events", "run-1"}, autonomyOverrides{Runtime: engine}, &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var decoded []runtime.EngineeringEvent
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("events output is not the journal: %v\n%s", err, out.String())
	}
	if len(decoded) != 2 || decoded[1].Type != runtime.EventRunFailed || decoded[0].ID != "e1" {
		t.Fatalf("journal was not rendered verbatim: %+v", decoded)
	}
}

// ---------------------------------------------------------------------------
// Repository selection
// ---------------------------------------------------------------------------

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", origin},
	} {
		command := exec.Command("git", args...)
		command.Dir = dir
		if out, err := command.CombinedOutput(); err != nil {
			t.Skipf("git is unavailable: %v %s", err, out)
		}
	}
	return dir
}

func TestExplicitRepositoryWinsAndRebindsTheRemote(t *testing.T) {
	dir := gitRepo(t, "https://github.com/inferred/origin.git")
	target, err := repositoryTarget(dir, "explicit/name")
	if err != nil {
		t.Fatal(err)
	}
	if target.Identity != "explicit/name" {
		t.Fatalf("explicit --repo did not win: %+v", target)
	}
	// Regression: ResolveRepository takes the identity from --repo but leaves
	// Remote as the cwd's origin. Cloning the inferred origin while reporting
	// the explicit identity would run against a different repository.
	if target.Remote != "https://github.com/explicit/name" {
		t.Fatalf("remote was not rebound to the explicit identity: %+v", target)
	}
}

func TestInferenceUsesAnUnambiguousOrigin(t *testing.T) {
	dir := gitRepo(t, "https://github.com/inferred/origin.git")
	target, err := repositoryTarget(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Identity != "inferred/origin" {
		t.Fatalf("inference failed: %+v", target)
	}
}

func TestAmbiguousInferenceIsRefused(t *testing.T) {
	dir := gitRepo(t, "https://gitlab.example/not/github.git")
	if _, err := repositoryTarget(dir, ""); err == nil {
		t.Fatal("an origin with no GitHub identity must be refused")
	}
	if _, err := repositoryTarget(dir, "not-owner-slash-name"); err == nil {
		t.Fatal("--repo must be owner/name")
	}
}

// ---------------------------------------------------------------------------
// Real wiring: the composition root, real SQLite state, real persisted journal
// ---------------------------------------------------------------------------

// seededWorkspace prepares a git checkout, an operator config, and a state
// directory holding one real run whose journal ends in run.failed.
func seededWorkspace(t *testing.T, origin string, tweaks ...func(map[string]any)) (dir, configPath, runID string) {
	t.Helper()
	dir = gitRepo(t, origin)
	support := t.TempDir()
	stateDir := filepath.Join(support, "state")

	copyFixture := func(name, into string) string {
		body, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "v0.1", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(support, into)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	model := copyFixture("security-sensitive.project-model.json", "model.json")
	policy := copyFixture("security-sensitive.engineering-policy.json", "policy.json")

	configPath = filepath.Join(support, "config.json")
	config := map[string]any{
		"state_dir":          stateDir,
		"project_model_path": model,
		"policy_path":        policy,
		"assurance":          map[string]any{"image": "sha256:" + strings.Repeat("0", 64)},
		"provider":           map[string]any{"kind": "openai", "model": "gpt-5", "credential_path": filepath.Join(support, "key")},
		"github":             map[string]any{"credential_mode": "none"},
		"budgets":            map[string]any{"wall_limit_seconds": 600, "max_execution_attempts": 2, "max_remediation_attempts": 2, "max_assurance_attempts": 2},
	}
	for _, tweak := range tweaks {
		tweak(config)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}

	runID = "run-seeded"
	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutRun(runtime.EngineeringRun{
		SchemaVersion: runtime.SchemaVersion,
		ID:            runID,
		Repository:    "zenchron/seeded",
		Goal:          "seeded run",
		Phase:         runtime.Assure,
		Disposition:   runtime.Active,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []runtime.EngineeringEvent{
		{SchemaVersion: runtime.SchemaVersion, ID: "seeded-1", RunID: runID, Type: runtime.EventRunCreated},
		{SchemaVersion: runtime.SchemaVersion, ID: "seeded-2", RunID: runID, Type: runtime.EventRunFailed, Payload: json.RawMessage(`{"reason":"seeded_failure"}`)},
	} {
		if _, err := store.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	return dir, configPath, runID
}

func TestStatusAndEventsReadRealPersistedState(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	var status bytes.Buffer
	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &status)
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if code != runtime.ExitFailed {
		t.Fatalf("status exit code %d, want %d", code, runtime.ExitFailed)
	}
	var report map[string]any
	if err := json.Unmarshal(status.Bytes(), &report); err != nil {
		t.Fatalf("status did not print a StatusReport: %v\n%s", err, status.String())
	}

	var journal bytes.Buffer
	code, err = autonomy([]string{"events", runID, "--config", configPath}, offlineOverrides(), &journal)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("events code=%d err=%v", code, err)
	}
	var events []runtime.EngineeringEvent
	if err := json.Unmarshal(journal.Bytes(), &events); err != nil {
		t.Fatalf("events did not print the journal: %v\n%s", err, journal.String())
	}
	if len(events) != 2 || events[1].Type != runtime.EventRunFailed || events[1].EventHash == "" {
		t.Fatalf("the persisted journal was not rendered: %+v", events)
	}
}

func TestExplicitRepositoryOverridesAnUnusableOrigin(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://gitlab.example/not/github.git")
	t.Chdir(dir)

	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("an ambiguous origin must refuse: code=%d err=%v", code, err)
	}

	code, err = autonomy([]string{"status", runID, "--repo", "zenchron/seeded", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("--repo must win over the unusable origin: %v", err)
	}
	if code != runtime.ExitFailed {
		t.Fatalf("status exit code %d, want %d", code, runtime.ExitFailed)
	}
}

func TestInvalidConfigurationRefusesBeforeAnythingRuns(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	// A repository layer that tries to raise the operator ceiling.
	if err := os.WriteFile(filepath.Join(dir, runtime.RepositoryConfigFile), []byte(`{"budgets": {"wall_limit_seconds": 999999}}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "only tighten") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestUnprovenProviderIsRefused(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	overrides := offlineOverrides()
	overrides.Provider = &runtime.FakeExecutionProvider{} // reports no isolation
	code, err := autonomy([]string{"status", runID, "--config", configPath}, overrides, &bytes.Buffer{})
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("an unproven provider must be refused: code=%d err=%v", code, err)
	}
}

// ---------------------------------------------------------------------------
// Real process exit status
// ---------------------------------------------------------------------------

func TestProcessExitStatus(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	for _, testCase := range []struct {
		name string
		args []string
		want int
	}{
		{"version", []string{"version"}, runtime.ExitCompleted},
		{"autonomy usage", []string{"autonomy", "nonsense"}, runtime.ExitInvalid},
		{"missing operator config", []string{"autonomy", "status", runID, "--config", filepath.Join(dir, "absent.json")}, runtime.ExitInvalid},
		{"failed run", []string{"autonomy", "status", runID, "--config", configPath}, runtime.ExitFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			code, output := runCLI(t, dir, testCase.args...)
			if code != testCase.want {
				t.Fatalf("process exited %d, want %d\n%s", code, testCase.want, output)
			}
		})
	}
}

func TestVersionStillPrints(t *testing.T) {
	var out bytes.Buffer
	code, err := run([]string{"version"}, osCommands{}, &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("unexpected version output %q", out.String())
	}
}

func TestTopLevelUsageKeepsItsHistoricalStatus(t *testing.T) {
	code, err := run([]string{"nonsense"}, osCommands{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected a usage error")
	}
	if code != exitUsage {
		t.Fatalf("exit code %d, want %d", code, exitUsage)
	}
}

// TestDoctorAnswersEveryCapability is the §13 wiring: the CLI supplies the
// dependency set and runtime.Doctor answers. The four verdicts asserted here
// are the ones an operator most often has to act on, and each is FAILED or
// WARNED for a stated reason rather than silently passing.
func TestDoctorAnswersEveryCapability(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	overrides := offlineOverrides()
	// A provider that reports no isolation at all, and no forge adapter: the
	// configuration already declares github.credential_mode "none".
	overrides.Provider = &runtime.FakeExecutionProvider{}
	overrides.GitHub = nil

	var out bytes.Buffer
	code, err := autonomy([]string{"doctor", "--config", configPath}, overrides, &out)
	if err != nil {
		t.Fatalf("doctor must answer rather than fail: %v", err)
	}
	if code != runtime.ExitFailed {
		t.Fatalf("doctor exit code %d, want %d for a report containing a FAIL", code, runtime.ExitFailed)
	}
	var report runtime.DoctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("doctor printed no report: %v\n%s", err, out.String())
	}
	if report.Status != runtime.DoctorFail {
		t.Fatalf("report status %q, want FAIL", report.Status)
	}
	for _, want := range []struct {
		id     string
		status runtime.DoctorStatus
	}{
		{"provider.isolation", runtime.DoctorFail},
		{"assurance.verifier_sandbox", runtime.DoctorFail},
		{"github.credential", runtime.DoctorFail},
		{"governance.publication_scope", runtime.DoctorWarn},
	} {
		check, ok := report.Check(want.id)
		if !ok {
			t.Fatalf("doctor did not answer %s at all", want.id)
		}
		if check.Status != want.status {
			t.Fatalf("%s = %s (%s), want %s", want.id, check.Status, check.Reason, want.status)
		}
		if strings.TrimSpace(check.Reason) == "" {
			t.Fatalf("%s reported %s with no reason", want.id, check.Status)
		}
	}
	// The text projection is over the same report, not a second one.
	var text bytes.Buffer
	if _, err := autonomy([]string{"doctor", "--config", configPath, "--text"}, overrides, &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "provider.isolation") || !strings.Contains(text.String(), "FAIL") {
		t.Fatalf("the text projection lost the report: %s", text.String())
	}
	if _, err := autonomy([]string{"doctor", "--config"}, autonomyOverrides{}, &bytes.Buffer{}); err == nil {
		t.Fatal("a malformed doctor flag must be refused")
	}
}

// ---------------------------------------------------------------------------
// Ownership lock
// ---------------------------------------------------------------------------

// TestSecondInvocationAgainstAHeldStateDirIsRefused proves the composition root
// takes the durable ownership lock before it builds a runtime, and that the
// refusal names the state directory instead of surfacing as a confusing
// downstream failure.
func TestSecondInvocationAgainstAHeldStateDirIsRefused(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	// A first invocation already owns this state directory.
	lock, err := runtime.AcquireOwnershipLock(config.StateDir, runtime.NewRuntimeOwner())
	if err != nil {
		t.Fatal(err)
	}
	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("a second invocation must be refused: code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), config.StateDir) || !strings.Contains(err.Error(), "already") {
		t.Fatalf("the refusal is not actionable: %v", err)
	}
	// Releasing the first invocation's ownership lets the next one start, so
	// the guard is liveness and not a permanent marker left in the state dir.
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{}); err != nil {
		t.Fatalf("a released ownership lock still blocked a new invocation: %v", err)
	}
}

// ---------------------------------------------------------------------------
// watch: doubles and helpers
// ---------------------------------------------------------------------------

// scriptedWatch stands in for runtime.WatchController. The zero value idles:
// every tick reports nothing to do and asks to be polled again in an hour.
type scriptedWatch struct {
	mu      sync.Mutex
	reports []runtime.TickReport
	errs    []error
	ticks   int
}

func (s *scriptedWatch) Tick(context.Context) (runtime.TickReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.ticks
	s.ticks++
	if at < len(s.errs) && s.errs[at] != nil {
		return runtime.TickReport{}, s.errs[at]
	}
	if at < len(s.reports) {
		return s.reports[at], nil
	}
	return runtime.TickReport{NextEligibleAt: time.Now().Add(time.Hour)}, nil
}

func (s *scriptedWatch) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ticks
}

// recordingWaiter replaces the loop's timer. It records every instant the loop
// was asked to wait for and cancels the run after `stopAfter` waits, so the
// schedule is asserted exactly instead of slept through.
type recordingWaiter struct {
	waited    []time.Time
	stopAfter int
	cancel    context.CancelFunc
}

func (w *recordingWaiter) Wait(_ context.Context, until time.Time) {
	w.waited = append(w.waited, until)
	if len(w.waited) >= w.stopAfter {
		w.cancel()
	}
}

func watchConfig(watch map[string]any) func(map[string]any) {
	return func(config map[string]any) { config["watch"] = watch }
}

// activeRun seeds a run whose journal holds only run.created, so it is
// unfinished and resumable - exactly the state a watcher shutdown must leave
// untouched.
func activeRun(t *testing.T, configPath, dir, runID string) string {
	t.Helper()
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutRun(runtime.EngineeringRun{
		SchemaVersion: runtime.SchemaVersion,
		ID:            runID,
		Repository:    "zenchron/seeded",
		Goal:          "an unfinished run",
		Phase:         runtime.Execute,
		Disposition:   runtime.Active,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(runtime.EngineeringEvent{
		SchemaVersion: runtime.SchemaVersion, ID: runID + "-created", RunID: runID, Type: runtime.EventRunCreated,
	}); err != nil {
		t.Fatal(err)
	}
	return config.StateDir
}

func journalOf(t *testing.T, stateDir, runID string) []runtime.EngineeringEvent {
	t.Helper()
	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func runDocument(t *testing.T, stateDir, runID string) runtime.EngineeringRun {
	t.Helper()
	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, found, err := store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run %q is not persisted: found=%v err=%v", runID, found, err)
	}
	return run
}

// ---------------------------------------------------------------------------
// watch: global configuration is validated before anything is polled
// ---------------------------------------------------------------------------

func TestWatchRefusesInvalidGlobalConfigurationBeforePolling(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		watch map[string]any
		want  string
	}{
		{"no enrolled repositories", map[string]any{}, "watch.repositories is empty"},
		{"polling below the floor", map[string]any{"repositories": []string{"zenchron/seeded"}, "poll_interval_seconds": 5}, "poll_interval_seconds"},
		{"not owner/name", map[string]any{"repositories": []string{"not-owner-name"}}, "owner/name"},
		{"enrolled twice", map[string]any{"repositories": []string{"zenchron/seeded", "Zenchron/Seeded"}}, "more than once"},
		{"another host smuggled in", map[string]any{"repositories": []string{"git@gitlab.example:owner"}}, "watch.repositories"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git", watchConfig(testCase.watch))
			t.Chdir(dir)
			spy := &scriptedWatch{}
			overrides := offlineOverrides()
			overrides.Watch = spy
			code, err := autonomy([]string{"watch", "--config", configPath}, overrides, &bytes.Buffer{})
			if err == nil {
				t.Fatal("an invalid global watch configuration must be refused")
			}
			if code != runtime.ExitInvalid {
				t.Fatalf("exit code %d, want %d (%v)", code, runtime.ExitInvalid, err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("the refusal is not actionable: %v", err)
			}
			if spy.count() != 0 {
				t.Fatalf("the watcher polled %d times before the configuration was accepted", spy.count())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// watch: the loop
// ---------------------------------------------------------------------------

func watchWorkspace(t *testing.T, repositories ...string) (dir, configPath string) {
	t.Helper()
	if len(repositories) == 0 {
		repositories = []string{"zenchron/seeded"}
	}
	dir, configPath, _ = seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		watchConfig(map[string]any{"repositories": repositories, "poll_interval_seconds": 60}))
	t.Chdir(dir)
	return dir, configPath
}

func TestWatchLoopHonoursTheControllersNextEligibleInstant(t *testing.T) {
	_, configPath := watchWorkspace(t)
	schedule := []time.Time{
		time.Date(2026, 8, 30, 12, 0, 30, 0, time.UTC),
		time.Date(2026, 8, 30, 12, 1, 30, 0, time.UTC),
		time.Date(2026, 8, 30, 12, 9, 0, 0, time.UTC),
	}
	controller := &scriptedWatch{}
	for _, at := range schedule {
		controller.reports = append(controller.reports, runtime.TickReport{NextEligibleAt: at})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &recordingWaiter{stopAfter: len(schedule), cancel: cancel}

	overrides := offlineOverrides()
	overrides.Watch, overrides.WatchWait = controller, waiter.Wait
	code, err := autonomyWatch(ctx, autonomyFlags{Config: configPath}, overrides, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	if controller.count() != len(schedule) {
		t.Fatalf("the loop ticked %d times, want %d", controller.count(), len(schedule))
	}
	if !reflect.DeepEqual(waiter.waited, schedule) {
		t.Fatalf("the loop did not wait on the controller's schedule:\n got %v\nwant %v", waiter.waited, schedule)
	}
}

func TestOneRepositoryFailureDoesNotStopTheOthers(t *testing.T) {
	_, configPath := watchWorkspace(t, "zenchron/seeded", "zenchron/broken", "zenchron/limited")
	failing := runtime.TickReport{
		ActiveRuns:     1,
		NextEligibleAt: time.Date(2026, 8, 30, 12, 0, 30, 0, time.UTC),
		Repositories: []runtime.RepositoryWatchReport{
			{Repository: runtime.GitHubRepo{Owner: "zenchron", Name: "seeded"}, Discovered: 2, Driven: []string{"run-a"}},
			{Repository: runtime.GitHubRepo{Owner: "zenchron", Name: "broken"}, ErrorClass: runtime.WatchErrorAuth, Detail: "github_auth_required"},
			{Repository: runtime.GitHubRepo{Owner: "zenchron", Name: "limited"}, ErrorClass: runtime.WatchErrorRateLimited, Detail: "rate limit exhausted"},
		},
	}
	controller := &scriptedWatch{reports: []runtime.TickReport{failing, failing}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &recordingWaiter{stopAfter: 2, cancel: cancel}

	overrides := offlineOverrides()
	overrides.Watch, overrides.WatchWait = controller, waiter.Wait
	var out bytes.Buffer
	code, err := autonomyWatch(ctx, autonomyFlags{Config: configPath}, overrides, &out)
	if err != nil {
		t.Fatalf("a per-repository failure must not fail the watcher: %v", err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	// The failing repositories did not end the tick, did not end the loop, and
	// did not remove the healthy repository from the next poll.
	if controller.count() != 2 {
		t.Fatalf("the loop stopped after %d ticks", controller.count())
	}
	for _, repository := range []string{"seeded", "broken", "limited"} {
		if !strings.Contains(out.String(), repository) {
			t.Fatalf("%q is missing from the watch report:\n%s", repository, out.String())
		}
	}
}

// TestWatchRendersEveryObservationTheControllerReported is the whole of watch
// observability: the report the controller derived from durable watch state is
// rendered as-is, so last poll, next eligible poll, auth/rate-limit/backoff
// state and the active run count reach the operator without a second, separate
// event log that could disagree with the journal.
func TestWatchRendersEveryObservationTheControllerReported(t *testing.T) {
	_, configPath := watchWorkspace(t)
	report := runtime.TickReport{
		ActiveRuns:     3,
		NextEligibleAt: time.Date(2026, 8, 30, 12, 1, 0, 0, time.UTC),
		Repositories: []runtime.RepositoryWatchReport{{
			Repository:     runtime.GitHubRepo{Owner: "zenchron", Name: "seeded"},
			LastPollAt:     time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
			NextEligibleAt: time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC),
			Discovered:     4,
			Driven:         []string{"run-a", "run-b"},
			ErrorClass:     runtime.WatchErrorRateLimited,
			Detail:         "secondary rate limit; backing off",
		}},
	}
	controller := &scriptedWatch{reports: []runtime.TickReport{report}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &recordingWaiter{stopAfter: 1, cancel: cancel}

	overrides := offlineOverrides()
	overrides.Watch, overrides.WatchWait = controller, waiter.Wait
	var out bytes.Buffer
	if _, err := autonomyWatch(ctx, autonomyFlags{Config: configPath}, overrides, &out); err != nil {
		t.Fatal(err)
	}
	var rendered runtime.TickReport
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatalf("watch did not print a TickReport: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(rendered, report) {
		t.Fatalf("the observation was not rendered verbatim:\n got %+v\nwant %+v", rendered, report)
	}
}

// TestWatchBuildsOneEngineForEachEnrolledRepository proves the Runtime factory
// watch is handed comes out of the SAME composition the single-repository
// commands use: watch never constructs a provider or a credential of its own,
// and every enrolled repository gets its own engine.
func TestWatchBuildsOneEngineForEachEnrolledRepository(t *testing.T) {
	_, configPath := watchWorkspace(t, "zenchron/seeded", "zenchron/other")
	built, err := newComposition(autonomyFlags{Config: configPath}, offlineOverrides())
	if err != nil {
		t.Fatal(err)
	}
	defer built.release()
	settings, err := built.watchSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Repositories) != 2 {
		t.Fatalf("enrolment did not survive configuration: %+v", settings.Repositories)
	}
	seen := map[*runtime.EngineeringRuntime]bool{}
	for _, repo := range settings.Repositories {
		engine, err := built.engine(runtime.RepositoryTarget{
			Identity:      repo.String(),
			Remote:        repo.CloneURL(),
			DefaultBranch: watchedDefaultBranch,
		})
		if err != nil {
			t.Fatalf("%s: %v", repo, err)
		}
		if seen[engine] {
			t.Fatalf("%s reused another repository's engine", repo)
		}
		seen[engine] = true
	}
}

// ---------------------------------------------------------------------------
// SIGTERM stops the watcher; only `stop` cancels a run
// ---------------------------------------------------------------------------

// lockedBuffer collects a child process's output. os/exec writes it from its
// own goroutine while the test reads it, so the buffer is guarded.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// TestSIGTERMStopsWatchingWithoutCancellingRuns is the §12 distinction, proven
// against a REAL child process, a REAL signal, and the REAL persisted journal.
// Stopping the daemon must leave every run exactly as resumable as it was; only
// `autonomy stop <run>` may append run.cancelled.
func TestSIGTERMStopsWatchingWithoutCancellingRuns(t *testing.T) {
	dir, configPath := watchWorkspace(t)
	runID := "run-watched"
	stateDir := activeRun(t, configPath, dir, runID)
	before := journalOf(t, stateDir, runID)

	command := exec.Command(os.Args[0], "-test.run=ZenchronCLIHelperNoTest")
	command.Dir = dir
	command.Env = append(os.Environ(),
		"ZENCHRON_CLI_HELPER=1", "ZENCHRON_CLI_OFFLINE=1", "ZENCHRON_CLI_WATCHED_RUN="+runID,
		"ZENCHRON_CLI_ARGS="+strings.Join([]string{"autonomy", "watch", "--config", configPath}, "\x1f"))
	var output lockedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for the first tick report, which is the proof the watcher reached
	// its loop and is now waiting for the next eligible poll.
	deadline := time.Now().Add(30 * time.Second)
	for output.len() == 0 {
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatalf("the watcher never produced a tick report: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("SIGTERM must stop the watcher cleanly: %v\n%s", err, output.String())
	}

	// 1. The journal is byte-for-byte what it was: the daemon stopping is not
	//    an event in any run's life.
	after := journalOf(t, stateDir, runID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("shutdown wrote to the journal:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, event := range after {
		if event.Type == runtime.EventRunCancelled {
			t.Fatalf("shutdown appended %s; stopping the daemon is not cancelling a run", event.Type)
		}
	}
	// 2. The run is still resumable: the durable document has not been settled.
	if run := runDocument(t, stateDir, runID); run.Disposition != runtime.Active {
		t.Fatalf("the run was settled as %q by a shutdown", run.Disposition)
	}
	// 3. status agrees, through the same mapping every other command uses.
	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitWaiting {
		t.Fatalf("status exit code %d, want %d: the run is no longer resumable", code, runtime.ExitWaiting)
	}
	// 4. Watcher ownership was handed back, so the next watcher may start.
	locks, err := os.ReadDir(filepath.Join(stateDir, "locks", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Fatalf("the watcher kept ownership after shutdown: %v", locks)
	}
}

// TestStopCancelsTheRun is the other half of the distinction: an explicit
// operator stop DOES cancel, durably, in the run's own journal.
func TestStopCancelsTheRun(t *testing.T) {
	dir, configPath := watchWorkspace(t)
	runID := "run-stoppable"
	stateDir := activeRun(t, configPath, dir, runID)

	var out bytes.Buffer
	code, err := autonomy([]string{"stop", runID, "--config", configPath}, offlineOverrides(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCancelled {
		t.Fatalf("stop exit code %d, want %d", code, runtime.ExitCancelled)
	}
	events := journalOf(t, stateDir, runID)
	last := events[len(events)-1]
	if last.Type != runtime.EventRunCancelled {
		t.Fatalf("stop did not record the cancellation: %+v", events)
	}
	if !strings.Contains(string(last.Payload), stopReason) {
		t.Fatalf("the cancellation does not name the operator: %s", last.Payload)
	}
	if last.EventHash == "" || last.PreviousEventID != events[len(events)-2].ID {
		t.Fatalf("the cancellation is not chained into the journal: %+v", last)
	}
	if run := runDocument(t, stateDir, runID); run.Disposition != runtime.Cancelled {
		t.Fatalf("the run document still reads %q", run.Disposition)
	}
	// status reports the cancellation through the same mapping as every other
	// disposition, and a second stop is a no-op rather than a second event.
	code, err = autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err != nil || code != runtime.ExitCancelled {
		t.Fatalf("status code=%d err=%v, want %d", code, err, runtime.ExitCancelled)
	}
	if _, err := autonomy([]string{"stop", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if again := journalOf(t, stateDir, runID); len(again) != len(events) {
		t.Fatalf("a repeated stop grew the journal: %d -> %d", len(events), len(again))
	}
	// Resuming does not withdraw the operator's cancellation.
	code, err = autonomy([]string{"resume", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != runtime.ExitCancelled {
		t.Fatalf("resume un-cancelled a stopped run: code=%d err=%v", code, err)
	}
	if again := journalOf(t, stateDir, runID); len(again) != len(events) {
		t.Fatalf("resume wrote to a cancelled run's journal: %d -> %d", len(events), len(again))
	}
}

// TestStopRefusesAnUnknownRun also fixes the boundary between "the command was
// malformed" and "the command was fine, the subject does not exist": the second
// is its own exit status, so an operator scripting against this CLI can tell a
// typo in a run id from a typo in a flag.
func TestStopRefusesAnUnknownRun(t *testing.T) {
	_, configPath := watchWorkspace(t)
	code, err := autonomy([]string{"stop", "run-absent", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != exitRunNotFound {
		t.Fatalf("code=%d err=%v, want %d", code, err, exitRunNotFound)
	}
}

// TestWatchRendersRealPersistedObservationState is §15 end to end against the
// REAL runtime.WatchController: three enrolled repositories, one of them
// answering with a credential failure. The failure is confined to its own
// repository, the others are still polled in the same tick, and what the CLI
// renders is exactly what the controller durably persisted - no second log.
func TestWatchRendersRealPersistedObservationState(t *testing.T) {
	dir, configPath := watchWorkspace(t, "zenchron/alpha", "zenchron/broken", "zenchron/gamma")
	forge := runtime.NewFakeGitHubAdapter()
	forge.Fail = func(call runtime.GitHubCall) error {
		if call.Repo.Name == "broken" {
			return &runtime.GitHubAuthError{Detail: "no credential is authorized for this repository"}
		}
		return nil
	}
	overrides := offlineOverrides()
	overrides.GitHub = forge
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &recordingWaiter{stopAfter: 1, cancel: cancel}
	overrides.WatchWait = waiter.Wait

	var out bytes.Buffer
	code, err := autonomyWatch(ctx, autonomyFlags{Config: configPath}, overrides, &out)
	if err != nil {
		t.Fatalf("one repository's auth failure stopped the watcher: %v\n%s", err, out.String())
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	var report runtime.TickReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("watch did not print a TickReport: %v\n%s", err, out.String())
	}
	if len(report.Repositories) != 3 {
		t.Fatalf("a failing repository removed the others from the tick: %+v", report.Repositories)
	}
	rendered := map[string]runtime.RepositoryWatchReport{}
	for _, repository := range report.Repositories {
		rendered[repository.Repository.String()] = repository
	}
	if broken := rendered["zenchron/broken"]; broken.ErrorClass != runtime.WatchErrorAuth {
		t.Fatalf("the credential failure was not classified: %+v", broken)
	}
	for _, healthy := range []string{"zenchron/alpha", "zenchron/gamma"} {
		if got := rendered[healthy]; got.ErrorClass != runtime.WatchErrorNone || got.LastPollAt.IsZero() {
			t.Fatalf("%s was not polled in the same tick as the failing repository: %+v", healthy, got)
		}
	}

	// What was rendered is what was durably observed: the same last poll, the
	// same next eligible poll, and the same error class the store now holds.
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for identity, shown := range rendered {
		state, _, found, err := store.WatchStateFor(identity)
		if err != nil || !found {
			t.Fatalf("%s: no durable watch state: found=%v err=%v", identity, found, err)
		}
		if state.LastErrorClass != shown.ErrorClass {
			t.Fatalf("%s: rendered %q, persisted %q", identity, shown.ErrorClass, state.LastErrorClass)
		}
		if !state.NotBefore.Equal(shown.NextEligibleAt) {
			t.Fatalf("%s: rendered next eligible %s, persisted backoff %s", identity, shown.NextEligibleAt, state.NotBefore)
		}
		if shown.ErrorClass == runtime.WatchErrorNone && !state.LastSuccessAt.Equal(shown.LastPollAt) {
			t.Fatalf("%s: rendered last poll %s, persisted %s", identity, shown.LastPollAt, state.LastSuccessAt)
		}
	}
	// The failing repository reports the actionable diagnostic and no
	// successful poll, and the loop waited on the schedule it was handed.
	if broken := rendered["zenchron/broken"]; !broken.LastPollAt.IsZero() || !strings.Contains(broken.Detail, "github_auth_required") {
		t.Fatalf("the credential failure was not reported actionably: %+v", broken)
	}
	if len(waiter.waited) != 1 || !waiter.waited[0].Equal(report.NextEligibleAt) {
		t.Fatalf("the loop did not wait on the reported schedule: %v vs %s", waiter.waited, report.NextEligibleAt)
	}
}

// ---------------------------------------------------------------------------
// Phase 10 operator surface
// ---------------------------------------------------------------------------

func hasEventType(events []runtime.EngineeringEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

// pendingRequest is the exact shape the runtime projects when a run is waiting
// on human authority. It carries the bindings an operator must never have to
// type: the candidate revision and tree, the contract revision, the controller.
func pendingRequest(runID string) *runtime.AuthorityRequest {
	return &runtime.AuthorityRequest{
		SchemaVersion: runtime.SchemaVersion,
		ID:            "authreq-0123456789abcdef0123456789abcdef",
		Digest:        strings.Repeat("d", 64),
		RunID:         runID,
		Repository:    "zenchron/seeded",
		Action:        domain.Action{Type: "git.pull_request.create", Target: "main"},
		Controller:    runtime.Ref{ID: "zenchron-engineering/dev", Revision: strings.Repeat("c", 64)},
		Candidate:     runtime.Candidate{Branch: "zenchron/" + runID, Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)},
		Contract:      runtime.Ref{ID: "contract-" + runID, Revision: "1-" + strings.Repeat("a", 40)},
		Status:        domain.AuthorityAwaitingAuthority,
		Requires:      []string{"human.publication_review"},
		Missing:       []string{"human.publication_review"},
	}
}

// TestStatusShowsTheExactPendingAuthorityRequest is §11's sharpest case: a run
// waiting on human authority must show WHICH request, by id and digest, and
// must tell the operator exactly what to type next. Anything less makes the
// operator guess at a subject binding.
func TestStatusShowsTheExactPendingAuthorityRequest(t *testing.T) {
	request := pendingRequest("run-1")
	engine := &scriptedRuntime{
		runID:   "run-1",
		request: request,
		report: runtime.StatusReport{
			SchemaVersion: runtime.SchemaVersion,
			RunID:         "run-1",
			Repository:    "zenchron/seeded",
			Phase:         runtime.Authorize,
			Disposition:   runtime.Waiting,
			Reason:        "awaiting_authority",
			Candidate:     request.Candidate,
			Contract:      request.Contract,
		},
	}
	var out bytes.Buffer
	code, err := autonomy([]string{"status", "run-1"}, autonomyOverrides{Runtime: engine}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitWaiting {
		t.Fatalf("status exit code %d, want %d", code, runtime.ExitWaiting)
	}
	var view struct {
		View             string `json:"view"`
		Reason           string `json:"reason"`
		NextAction       string `json:"next_action"`
		AuthorityRequest *struct {
			ID       string   `json:"id"`
			Digest   string   `json:"digest"`
			Requires []string `json:"requires"`
		} `json:"authority_request"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("status printed no view: %v\n%s", err, out.String())
	}
	if view.View != "autonomy.status/1" {
		t.Fatalf("the operator view is not versioned: %q", view.View)
	}
	if view.AuthorityRequest == nil {
		t.Fatalf("a run waiting on authority did not show its request: %s", out.String())
	}
	if view.AuthorityRequest.ID != request.ID || view.AuthorityRequest.Digest != request.Digest {
		t.Fatalf("status showed request %+v, want %s/%s", view.AuthorityRequest, request.ID, request.Digest)
	}
	if len(view.AuthorityRequest.Requires) != 1 || view.AuthorityRequest.Requires[0] != "human.publication_review" {
		t.Fatalf("the request does not name the outstanding human claim: %+v", view.AuthorityRequest)
	}
	if !strings.Contains(view.NextAction, "authorize run-1 "+request.ID) {
		t.Fatalf("status did not name the next operator action: %q", view.NextAction)
	}
	// The text rendering is a projection over the same structure.
	var text bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), request.ID) || !strings.Contains(text.String(), "next action") {
		t.Fatalf("the text projection lost the request: %s", text.String())
	}
}

// TestAuthorizeRecordsEvidenceAndReportsTheReEvaluation is §2. Three things are
// asserted, and each is a rule rather than a formatting detail:
//
//  1. the operator typed only a run id and a request id - the candidate SHA,
//     the tree, the contract revision and the action came from the runtime's
//     own projection;
//  2. a resolved operator identity was attached, with its provenance;
//  3. what is reported is the RE-EVALUATED status, not a state assignment.
func TestAuthorizeRecordsEvidenceAndReportsTheReEvaluation(t *testing.T) {
	request := pendingRequest("run-1")
	engine := &scriptedRuntime{
		runID:   "run-1",
		request: request,
		authorizeResult: runtime.AuthorizeResult{
			EvidenceID:  "authev-" + strings.Repeat("e", 64),
			Recorded:    true,
			Request:     runtime.Ref{ID: request.ID, Revision: request.Digest},
			Status:      domain.AuthorityAuthorized,
			Disposition: runtime.Active,
		},
	}
	var out bytes.Buffer
	code, err := autonomy([]string{"authorize", "run-1", request.ID, "--approve", "--note", "reviewed the diff"},
		autonomyOverrides{Runtime: engine}, &out)
	if err != nil {
		t.Fatalf("authorize failed: %v", err)
	}
	if code != runtime.ExitWaiting {
		t.Fatalf("exit code %d, want the run's own disposition mapping %d", code, runtime.ExitWaiting)
	}
	if len(engine.authorized) != 1 {
		t.Fatalf("authorize recorded %d decisions, want 1", len(engine.authorized))
	}
	recorded := engine.authorized[0]
	if recorded.Decision != "approve" || recorded.Note != "reviewed the diff" {
		t.Fatalf("the operator's answer was not carried: %+v", recorded)
	}
	if recorded.Digest != request.Digest || recorded.Action != request.Action {
		t.Fatalf("the exact binding was not pinned from the runtime's projection: %+v", recorded)
	}
	if recorded.Operator.ID == "" || recorded.Operator.Provenance != runtime.ProvenanceLocalUnverified {
		t.Fatalf("no resolved operator provenance was attached: %+v", recorded.Operator)
	}
	var result runtime.AuthorizeResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("authorize printed no result: %v\n%s", err, out.String())
	}
	if result.Status != domain.AuthorityAuthorized || !result.Recorded {
		t.Fatalf("the re-evaluated status was not reported: %+v", result)
	}
	if _, err := autonomy([]string{"authorize", "run-1", request.ID}, autonomyOverrides{Runtime: engine}, &bytes.Buffer{}); err == nil {
		t.Fatal("authorize without a decision must be refused")
	}
	if _, err := autonomy([]string{"authorize", "run-1", request.ID, "--approve", "--reject"}, autonomyOverrides{Runtime: engine}, &bytes.Buffer{}); err == nil {
		t.Fatal("two decisions must be refused")
	}
}

// TestStaleAuthorizeIsRefusedWithItsOwnStatus is §5 at the CLI boundary. Two
// things must hold: the refusal is its own exit status, and a request id that
// is NOT the current one must never be silently retargeted at the current
// binding by the CLI pinning a digest the operator did not name.
func TestStaleAuthorizeIsRefusedWithItsOwnStatus(t *testing.T) {
	request := pendingRequest("run-1")
	engine := &scriptedRuntime{
		runID:   "run-1",
		request: request,
		authorizeErr: &runtime.AuthorityRefusedError{
			Code:   runtime.RefusedStaleRequest,
			Detail: "request authreq-old is not the run's current request " + request.ID,
		},
	}
	code, err := autonomy([]string{"authorize", "run-1", "authreq-old", "--approve"},
		autonomyOverrides{Runtime: engine}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a stale authorization must be refused")
	}
	if code != exitAuthorityRefused {
		t.Fatalf("exit code %d, want %d for a refused authorization", code, exitAuthorityRefused)
	}
	if len(engine.authorized) != 1 {
		t.Fatalf("authorize recorded %d attempts, want 1", len(engine.authorized))
	}
	if engine.authorized[0].Digest != "" || engine.authorized[0].Action != (domain.Action{}) {
		t.Fatalf("the CLI retargeted a stale request id at the current binding: %+v", engine.authorized[0])
	}
	var refused *runtime.AuthorityRefusedError
	if !errors.As(err, &refused) || refused.Code != runtime.RefusedStaleRequest {
		t.Fatalf("the refusal lost its type: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Real runs: resume, refresh
// ---------------------------------------------------------------------------

// forgeWithIssue is an offline forge holding one opted-in source issue and a
// default branch, which is the minimum a source observation needs.
func forgeWithIssue(issue int) *runtime.FakeGitHubAdapter {
	forge := runtime.NewFakeGitHubAdapter()
	forge.Issues[issue] = runtime.GitHubIssue{
		Number:    issue,
		URL:       fmt.Sprintf("https://github.com/zenchron/seeded/issues/%d", issue),
		Title:     "seeded objective",
		Body:      "seeded body",
		Labels:    []runtime.UntrustedText{runtime.DefaultDiscoveryLabel},
		State:     runtime.GitHubOpen,
		UpdatedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Author:    runtime.GitHubActor{Login: "operator"},
	}
	forge.Refs["main"] = strings.Repeat("f", 40)
	return forge
}

// seedIssueRun creates a REAL run through the composition root, so it carries
// the controller digest, the goal and the genesis event the runtime derives,
// then settles it into the durable wait a test resumes from.
func seedIssueRun(t *testing.T, configPath string, issue int, overrides autonomyOverrides,
	disposition runtime.Disposition, reason string, extra ...runtime.EngineeringEvent) (stateDir, runID string) {
	t.Helper()
	built, engine, err := buildEngine(autonomyFlags{Config: configPath}, overrides)
	if err != nil {
		t.Fatal(err)
	}
	stateDir = built.config.StateDir
	runID, err = engine.StartOrResumeIssueRun(context.Background(), issue)
	built.release()
	if err != nil {
		t.Fatal(err)
	}

	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, event := range extra {
		event.SchemaVersion, event.RunID = runtime.SchemaVersion, runID
		event.ID = fmt.Sprintf("%s-seed-%d", runID, i)
		if _, err := store.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	run, found, err := store.Run(runID)
	if err != nil || !found {
		t.Fatalf("seeded run is not persisted: found=%v err=%v", found, err)
	}
	run.Disposition, run.Reason = disposition, reason
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	return stateDir, runID
}

func intentChangedEvent() runtime.EngineeringEvent {
	return runtime.EngineeringEvent{
		Type:    runtime.EventSourceIntentChanged,
		Payload: json.RawMessage(`{"previous_digest":"before","current_digest":"after","reason":"issue edited"}`),
	}
}

// TestResumeOfAnAuthRestoredRunProceeds is §8's "may proceed" case, against the
// REAL runtime: a run that was waiting because the forge credential was gone is
// not stuck once it is back. Resume asks the runtime to reconcile, and the
// runtime re-derives the wait from durable state and finds it cleared.
func TestResumeOfAnAuthRestoredRunProceeds(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	overrides := offlineOverrides()
	overrides.GitHub = forgeWithIssue(7)
	stateDir, runID := seedIssueRun(t, configPath, 7, overrides, runtime.Waiting, runtime.WatchWaitingGitHubAuth,
		runtime.EngineeringEvent{Type: runtime.EventRunWaiting, Payload: json.RawMessage(`{"reason":"github_auth_required"}`)})

	if _, err := autonomy([]string{"resume", runID, "--config", configPath}, overrides, &bytes.Buffer{}); err != nil {
		t.Fatalf("resume refused a run whose credential was restored: %v", err)
	}
	events := journalOf(t, stateDir, runID)
	if !hasEventType(events, runtime.EventContractCompiled) {
		t.Fatalf("resume did not proceed past the credential wait; journal: %+v", eventTypesOf(events))
	}
}

// TestResumeDoesNotAbsorbChangedSourceIntent is the §8 invariant with the
// sharpest teeth. A plain resume of a run whose pinned source moved must leave
// it waiting and must NOT compile the new intent. `refresh` is the only thing
// that re-reads a moved source, and it is explicit.
func TestResumeDoesNotAbsorbChangedSourceIntent(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	overrides := offlineOverrides()
	overrides.GitHub = forgeWithIssue(7)
	stateDir, runID := seedIssueRun(t, configPath, 7, overrides, runtime.Waiting, "source_intent_changed", intentChangedEvent())

	var out bytes.Buffer
	code, err := autonomy([]string{"resume", runID, "--config", configPath}, overrides, &out)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if code != runtime.ExitWaiting {
		t.Fatalf("resume exit code %d, want %d", code, runtime.ExitWaiting)
	}
	var outcome runtime.Outcome
	if err := json.Unmarshal(out.Bytes(), &outcome); err != nil {
		t.Fatalf("resume printed no outcome: %v\n%s", err, out.String())
	}
	if outcome.Disposition != runtime.Waiting || outcome.Reason != "source_intent_changed" {
		t.Fatalf("a plain resume changed the wait: %+v", outcome)
	}
	events := journalOf(t, stateDir, runID)
	if hasEventType(events, runtime.EventContractCompiled) {
		t.Fatalf("a plain resume absorbed changed source intent: %+v", eventTypesOf(events))
	}
}

// TestRefreshRecompilesFromTheCurrentSource is §9. The refresh is explicit, it
// is recorded in the run's own journal, the journal it refreshes is preserved
// byte for byte, and the work that answers the same source is recompiled from
// the source as it is NOW - through ordinary reconciliation, granting nothing.
func TestRefreshRecompilesFromTheCurrentSource(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	overrides := offlineOverrides()
	overrides.GitHub = forgeWithIssue(7)
	stateDir, runID := seedIssueRun(t, configPath, 7, overrides, runtime.Waiting, "source_intent_changed", intentChangedEvent())
	before := journalOf(t, stateDir, runID)

	var out bytes.Buffer
	if _, err := autonomy([]string{"refresh", runID, "--config", configPath}, overrides, &out); err != nil {
		t.Fatalf("refresh failed: %v\n%s", err, out.String())
	}
	var view struct {
		View         string `json:"view"`
		Run          string `json:"run"`
		Issue        int    `json:"issue"`
		Successor    string `json:"successor_run"`
		SourceAfter  string `json:"source_digest_after"`
		SourceBefore string `json:"source_digest_before"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("refresh printed no view: %v\n%s", err, out.String())
	}
	if view.View != "autonomy.refresh/1" || view.Issue != 7 {
		t.Fatalf("refresh did not report its own subject: %+v", view)
	}
	if view.Successor == "" || view.Successor == runID {
		t.Fatalf("refresh produced no work that answers the current source: %+v", view)
	}
	if view.SourceAfter == "" {
		t.Fatalf("refresh did not derive the new snapshot identity: %+v", view)
	}

	// The old journal is preserved exactly, and now records the operator's
	// refresh intent as its own durable, distinguishable reason.
	after := journalOf(t, stateDir, runID)
	if len(after) != len(before)+1 {
		t.Fatalf("refresh rewrote history: %d events before, %d after", len(before), len(after))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].EventHash != before[i].EventHash {
			t.Fatalf("refresh rewrote event %d", i)
		}
	}
	last := after[len(after)-1]
	if last.Type != runtime.EventRunCancelled || !strings.Contains(string(last.Payload), refreshReason) {
		t.Fatalf("refresh did not record operator refresh intent: %+v", last)
	}
	if last.Type == runtime.EventRunCancelled && strings.Contains(string(last.Payload), stopReason) {
		t.Fatal("a refresh must stay distinguishable from an operator stop")
	}

	// The work that now answers the source recompiled it: source -> facts ->
	// policy -> WorkContract ran again, through ordinary reconciliation.
	successor := journalOf(t, stateDir, view.Successor)
	if !hasEventType(successor, runtime.EventContractCompiled) {
		t.Fatalf("refresh did not recompile: %+v", eventTypesOf(successor))
	}
	// Refreshing grants nothing: no human authority was recorded by it.
	if hasEventType(successor, runtime.EventHumanAuthorityRecorded) {
		t.Fatal("refresh recorded authority")
	}
}

func eventTypesOf(events []runtime.EngineeringEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

// ---------------------------------------------------------------------------
// events and --follow
// ---------------------------------------------------------------------------

// TestEventsRendersThePersistedOrder is §12: the journal is rendered in the
// order it was persisted, with sequence, chain, state digests and bounded
// payloads, and artifact references only.
func TestEventsRendersThePersistedOrder(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	var out bytes.Buffer
	code, err := autonomy([]string{"events", runID, "--config", configPath}, offlineOverrides(), &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var rendered []struct {
		View        string `json:"view"`
		Sequence    int64  `json:"sequence"`
		Type        string `json:"type"`
		Actor       string `json:"actor"`
		StateAfter  string `json:"state_after"`
		EventHash   string `json:"event_hash"`
		PreviousID  string `json:"previous_event_id"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatalf("events printed no journal: %v\n%s", err, out.String())
	}
	if len(rendered) != 2 {
		t.Fatalf("events rendered %d rows, want the 2 persisted ones", len(rendered))
	}
	if rendered[0].Sequence != 1 || rendered[1].Sequence != 2 {
		t.Fatalf("events did not preserve persisted order: %+v", rendered)
	}
	if rendered[0].Type != runtime.EventRunCreated || rendered[1].Type != runtime.EventRunFailed {
		t.Fatalf("events reordered the journal: %+v", rendered)
	}
	if rendered[1].EventHash == "" || rendered[1].PreviousID != "seeded-1" || rendered[1].StateAfter == "" {
		t.Fatalf("events dropped the diagnostics an audit needs: %+v", rendered[1])
	}
	if rendered[0].View != "autonomy.events/1" || rendered[1].Actor == "" {
		t.Fatalf("events is not a versioned, attributed view: %+v", rendered)
	}
	if _, err := autonomy([]string{"events", "run-absent", "--config", configPath}, offlineOverrides(), &bytes.Buffer{}); err == nil {
		t.Fatal("events must refuse a run the store has never held")
	}
}

// TestEventsFollowObservesAConcurrentAppendWithoutTakingOwnership is the §12
// invariant that makes `--follow` usable at all: it must be able to tail a run
// something else is driving. That means it may take NEITHER the state
// directory's exclusive ownership NOR any run-driving lease - so this test
// holds the ownership lock for the whole run of it, appends through a second,
// independent store handle, and requires the follower to observe the append.
func TestEventsFollowObservesAConcurrentAppendWithoutTakingOwnership(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Somebody else already owns this state directory and is driving the run.
	lock, err := runtime.AcquireOwnershipLock(config.StateDir, runtime.NewRuntimeOwner())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	locksBefore, err := os.ReadDir(filepath.Join(config.StateDir, "locks", "runtime"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out lockedBuffer
	done := make(chan int, 1)
	go func() {
		code, err := autonomyEvents(ctx, autonomyFlags{Config: configPath, Follow: true}, runID, &out)
		if err != nil {
			t.Errorf("follow failed: %v", err)
		}
		done <- code
	}()

	waitFor := func(what string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("timed out waiting for %s; output so far:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("the already persisted events", func() bool { return strings.Contains(out.String(), "seeded-2") })

	// A second, INDEPENDENT handle appends while the follower is running.
	appended := "concurrent-append"
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(runtime.EngineeringEvent{
		SchemaVersion: runtime.SchemaVersion, ID: appended, RunID: runID,
		Type: runtime.EventRunWaiting, Payload: json.RawMessage(`{"reason":"still_here"}`),
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	waitFor("the concurrent append", func() bool { return strings.Contains(out.String(), appended) })

	// Following took no ownership of its own: the only lock is the one this
	// test is holding on behalf of the other controller.
	locksAfter, err := os.ReadDir(filepath.Join(config.StateDir, "locks", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if len(locksAfter) != len(locksBefore) {
		t.Fatalf("follow acquired ownership: %d locks before, %d after", len(locksBefore), len(locksAfter))
	}

	cancel()
	if code := <-done; code != runtime.ExitCompleted {
		t.Fatalf("follow exited %d on shutdown, want %d", code, runtime.ExitCompleted)
	}
	// Following mutated nothing: the journal is exactly what the two writers
	// left, with no operation planned or leased by the reader.
	events := journalOf(t, config.StateDir, runID)
	if len(events) != 3 || events[2].ID != appended {
		t.Fatalf("follow changed the journal: %+v", eventTypesOf(events))
	}
	operations, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	if held, err := operations.Operations(runID); err != nil || len(held) != 0 {
		t.Fatalf("follow acquired a run-driving lease: %+v err=%v", held, err)
	}
}

// ---------------------------------------------------------------------------
// gc
// ---------------------------------------------------------------------------

// TestGCDryRunEqualsTheRealPlanAndPreservesActiveRuns is §14. There is exactly
// one planner, so what --dry-run prints is what a real run executes; and a run
// that is not terminal keeps everything that explains it.
func TestGCDryRunEqualsTheRealPlanAndPreservesActiveRuns(t *testing.T) {
	dir, configPath, terminalRun := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		func(config map[string]any) { config["gc"] = map[string]any{"retention_hours": 1} })
	t.Chdir(dir)
	stateDir := activeRun(t, configPath, dir, "run-still-going")

	workspaces := map[string]string{}
	for _, id := range []string{terminalRun, "run-still-going"} {
		path := filepath.Join(stateDir, "runs", id, "candidate")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "file.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		workspaces[id] = path
	}

	var planned bytes.Buffer
	code, err := autonomy([]string{"gc", "--dry-run", "--config", configPath}, offlineOverrides(), &planned)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("gc --dry-run code=%d err=%v", code, err)
	}
	var plan runtime.GCPlan
	if err := json.Unmarshal(planned.Bytes(), &plan); err != nil {
		t.Fatalf("gc --dry-run printed no plan: %v\n%s", err, planned.String())
	}
	if !containsWorkspace(plan.Eligible, workspaces[terminalRun]) {
		t.Fatalf("the terminal run's workspace is not eligible: %+v", plan.Eligible)
	}
	if !containsWorkspace(plan.Retained, workspaces["run-still-going"]) {
		t.Fatalf("an active run's workspace was judged eligible: %+v", plan)
	}

	var executed bytes.Buffer
	code, err = autonomy([]string{"gc", "--config", configPath}, offlineOverrides(), &executed)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("gc code=%d err=%v", code, err)
	}
	var result runtime.GCResult
	if err := json.Unmarshal(executed.Bytes(), &result); err != nil {
		t.Fatalf("gc printed no result: %v\n%s", err, executed.String())
	}
	if !containsWorkspace(result.Deleted, workspaces[terminalRun]) {
		t.Fatalf("the real run did not execute the plan it printed: %+v", result)
	}
	if !containsWorkspace(result.Plan.Retained, workspaces["run-still-going"]) {
		t.Fatalf("the real run's plan disagreed with the dry run: %+v", result.Plan)
	}
	if _, err := os.Stat(workspaces[terminalRun]); !os.IsNotExist(err) {
		t.Fatalf("the eligible workspace survived: %v", err)
	}
	if _, err := os.Stat(workspaces["run-still-going"]); err != nil {
		t.Fatalf("gc reclaimed an active run's workspace: %v", err)
	}
}

func containsWorkspace(targets []runtime.GCTarget, path string) bool {
	for _, target := range targets {
		if target.Kind == runtime.GCCandidateWorkspace && target.Path == path {
			return true
		}
	}
	return false
}

// TestGCRetentionIsOperatorAuthority proves the new configuration member is on
// the authorizing layer only: an in-repo file naming it is refused before it is
// decoded, exactly like every other operator-only member.
func TestGCRetentionIsOperatorAuthority(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		func(config map[string]any) { config["gc"] = map[string]any{"retention_hours": 48} })
	t.Chdir(dir)
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if config.GCRetention() != 48*time.Hour {
		t.Fatalf("effective retention %s, want 48h", config.GCRetention())
	}
	if err := os.WriteFile(filepath.Join(dir, runtime.RepositoryConfigFile), []byte(`{"gc": {"retention_hours": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || code != runtime.ExitInvalid {
		t.Fatalf("a repository naming gc must be refused: code=%d err=%v", code, err)
	}
}

// ---------------------------------------------------------------------------
// Real process exit status for every operator outcome (§19)
// ---------------------------------------------------------------------------

func TestOperatorExitStatusIsTheRealProcessStatus(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")

	t.Run("failed run", func(t *testing.T) {
		if code, out := runCLI(t, dir, "autonomy", "status", runID, "--config", configPath); code != runtime.ExitFailed {
			t.Fatalf("process exited %d, want %d\n%s", code, runtime.ExitFailed, out)
		}
	})
	t.Run("run not found", func(t *testing.T) {
		if code, out := runCLI(t, dir, "autonomy", "status", "run-absent", "--config", configPath); code != exitRunNotFound {
			t.Fatalf("process exited %d, want %d\n%s", code, exitRunNotFound, out)
		}
	})
	t.Run("usage", func(t *testing.T) {
		if code, out := runCLI(t, dir, "autonomy", "status"); code != runtime.ExitInvalid {
			t.Fatalf("process exited %d, want %d\n%s", code, runtime.ExitInvalid, out)
		}
	})
	t.Run("refused authorization", func(t *testing.T) {
		env := []string{"ZENCHRON_CLI_OFFLINE=1", "ZENCHRON_CLI_STALE_AUTHORIZE=1"}
		code, out := runCLIEnv(t, dir, env, "autonomy", "authorize", "run-scripted", "authreq-old", "--approve", "--config", configPath)
		if code != exitAuthorityRefused {
			t.Fatalf("process exited %d, want %d\n%s", code, exitAuthorityRefused, out)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		stopDir, stopConfig, stopRun := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
		if code, out := runCLI(t, stopDir, "autonomy", "stop", stopRun, "--config", stopConfig); code != runtime.ExitCancelled {
			t.Fatalf("process exited %d, want %d\n%s", code, runtime.ExitCancelled, out)
		}
		// Still cancelled after the process that recorded it is gone.
		if code, out := runCLI(t, stopDir, "autonomy", "status", stopRun, "--config", stopConfig); code != runtime.ExitCancelled {
			t.Fatalf("the cancellation did not survive the process: %d\n%s", code, out)
		}
	})
	t.Run("waiting", func(t *testing.T) {
		waitDir, waitConfig, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
		stateDir := activeRun(t, waitConfig, waitDir, "run-open")
		_ = stateDir
		if code, out := runCLI(t, waitDir, "autonomy", "status", "run-open", "--config", waitConfig); code != runtime.ExitWaiting {
			t.Fatalf("process exited %d, want %d\n%s", code, runtime.ExitWaiting, out)
		}
	})
}
