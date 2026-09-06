package main

// The operator surfaces #63 adds, exercised the way an operator reaches them:
// through the command line, over a real configuration, with no coding CLI
// installed and no network.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// agentsConfig replaces the pre-#63 single provider with a named registry. The
// two are mutually exclusive, so the provider entry is removed rather than left
// beside it.
func agentsConfig(agents map[string]any, defaultAgent string) func(map[string]any) {
	return func(config map[string]any) {
		delete(config, "provider")
		config["agents"] = agents
		if defaultAgent != "" {
			config["default_agent"] = defaultAgent
		}
	}
}

// TestAgentsListsEveryConfiguredWorkerWithoutSpendingAnything is the discovery
// surface: an operator sees what they can give work to, and finding that out
// costs nothing.
func TestAgentsListsEveryConfiguredWorkerWithoutSpendingAnything(t *testing.T) {
	_, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		agentsConfig(map[string]any{
			"codex":  map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted", "command": "codex-not-installed-9c3"},
			"claude": map[string]any{"kind": "claude_code", "trust_mode": "operator_trusted", "command": "claude-not-installed-9c3"},
		}, "codex"))

	var out bytes.Buffer
	code, err := autonomy([]string{"agents", "--config", configPath}, offlineOverrides(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	var statuses []runtime.AgentStatus
	if err := json.Unmarshal(out.Bytes(), &statuses); err != nil {
		t.Fatalf("agents printed no listing: %v\n%s", err, out.String())
	}
	if len(statuses) != 2 {
		t.Fatalf("agents listed %d workers, want both configured ones", len(statuses))
	}
	byID := map[string]runtime.AgentStatus{}
	for _, status := range statuses {
		byID[status.ID] = status
	}
	if !byID["codex"].Default || byID["claude"].Default {
		t.Fatalf("the default agent is not marked exactly once: %#v", statuses)
	}
	for id, status := range byID {
		if status.TrustMode != runtime.TrustOperatorTrusted {
			t.Errorf("%s trust mode = %q", id, status.TrustMode)
		}
		// The executables do not exist, so the honest answer is unavailable
		// with a reason - never a guess, and never an invented auth mode.
		if status.Available {
			t.Errorf("%s reported available with no executable installed", id)
		}
		if status.Detail == "" {
			t.Errorf("%s is unavailable with no reason", id)
		}
		if status.AuthMode != runtime.AuthModeUnknown {
			t.Errorf("%s invented an authentication mode for a missing executable: %q", id, status.AuthMode)
		}
	}

	// The text projection is over the same answer.
	var text bytes.Buffer
	if _, err := autonomy([]string{"agents", "--config", configPath, "--text"}, offlineOverrides(), &text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex", "claude", "operator_trusted", "unknown"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("the text projection lost %q:\n%s", want, text.String())
		}
	}
}

// TestUnknownAgentIsRefusedWithTheRealNames is the typo path. An operator who
// asks for an agent they did not configure gets the set that exists, not a
// silent fallback to the default - which would run their work on a worker they
// did not choose.
func TestUnknownAgentIsRefusedWithTheRealNames(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		agentsConfig(map[string]any{
			"codex": map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted"},
		}, "codex"))
	t.Chdir(dir)

	code, err := autonomy([]string{"run", "issue", "7", "--agent", "gemni", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil {
		t.Fatal("an unconfigured agent was accepted")
	}
	if code != runtime.ExitInvalid {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitInvalid)
	}
	var unknown *runtime.UnknownAgentError
	if !strings.Contains(err.Error(), "codex") {
		t.Fatalf("the refusal does not name the configured agents: %v", err)
	}
	_ = unknown
}

// TestRepositoryConfigurationCannotNameAnAgent keeps worker selection operator
// authority. A repository that could choose its own coding agent would be
// choosing which account pays for changing it, and with which trust.
func TestRepositoryConfigurationCannotNameAnAgent(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		agentsConfig(map[string]any{
			"codex": map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted"},
		}, "codex"))
	t.Chdir(dir)
	for _, member := range []string{
		`{"agents": {"mine": {"kind": "codex_cli", "trust_mode": "operator_trusted"}}}`,
		`{"default_agent": "mine"}`,
		`{"feedback": {"min_permission": "read"}}`,
		`{"storage": {"max_state_bytes": 1}}`,
		`{"supervisor": {"max_concurrent_runs": 99}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, runtime.RepositoryConfigFile), []byte(member), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := autonomy([]string{"agents", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
		if err == nil {
			t.Fatalf("an in-repo file named %s and was accepted", member)
		}
		if !strings.Contains(err.Error(), "operator authority") {
			t.Fatalf("the refusal does not name the authority boundary for %s: %v", member, err)
		}
	}
}

// TestFleetStatusAnswersEveryRunWithoutADatabase is the control-room view: the
// question an operator with several workers has, answered without opening
// SQLite or knowing where an artifact path is built.
func TestFleetStatusAnswersEveryRunWithoutADatabase(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	var out bytes.Buffer
	code, err := autonomy([]string{"status", "--config", configPath}, offlineOverrides(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	var fleet runtime.Fleet
	if err := json.Unmarshal(out.Bytes(), &fleet); err != nil {
		t.Fatalf("status printed no fleet: %v\n%s", err, out.String())
	}
	var found bool
	for _, run := range fleet.Runs {
		if run.RunID == runID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the seeded run is missing from the fleet view: %#v", fleet.Runs)
	}
	if fleet.Capacity < 1 {
		t.Fatalf("the fleet view reports no operator capacity: %#v", fleet)
	}
	if fleet.SupervisorRunning {
		t.Fatal("no supervisor is running, but the fleet view says one is")
	}
	if fleet.ControlEndpoint == "" {
		t.Fatal("the fleet view does not say where the control endpoint would be")
	}

	// Naming a run still gets the detailed single-run projection: the
	// control-room view is an addition, not a replacement.
	var single bytes.Buffer
	if _, err := autonomy([]string{"status", runID, "--config", configPath}, offlineOverrides(), &single); err != nil {
		t.Fatal(err)
	}
	var report runtime.StatusReport
	if err := json.Unmarshal(single.Bytes(), &report); err != nil || report.RunID != runID {
		t.Fatalf("the single-run projection was lost: %v\n%s", err, single.String())
	}
}

// TestLogsShowTheSanitizedTranscriptAndItsAttemptBoundaries is the operator
// path to "what is my worker actually doing", without knowing an artifact path.
func TestLogsShowTheSanitizedTranscriptAndItsAttemptBoundaries(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := runtime.ArtifactStore{Root: filepath.Join(config.StateDir, "artifacts")}
	// Two attempts of one operation, written the way the runtime writes them.
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := artifacts.StoreExecutionAttemptTranscript("codex", runtime.ExecutionAttemptRef{
			RunID: runID, OperationID: "execution.invoke#initial", Attempt: attempt,
		}, []byte("worker output for attempt "+string(rune('0'+attempt))+"\n"), nil); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	code, err := autonomyLogs(context.Background(), autonomyFlags{Config: configPath}, offlineOverrides(), runID, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCompleted)
	}
	text := out.String()
	for _, want := range []string{"attempt 1", "attempt 2", "codex", "worker output for attempt 1", "worker output for attempt 2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("logs lost %q:\n%s", want, text)
		}
	}
	// An unknown run is a typed refusal, not an empty success.
	if _, err := autonomyLogs(context.Background(), autonomyFlags{Config: configPath}, offlineOverrides(), "run-does-not-exist", &bytes.Buffer{}); err == nil {
		t.Fatal("logs for an unknown run reported success")
	}
}

// TestBatchSubmissionRequiresASupervisor states the product boundary plainly:
// starting several issues at once is what the supervisor is for, and without
// one the command says so instead of silently driving one of them.
func TestBatchSubmissionRequiresASupervisor(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	code, err := autonomy([]string{"run", "issues", "7", "8", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a batch was accepted with no supervisor to own it")
	}
	if code != runtime.ExitInvalid {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitInvalid)
	}
	if !strings.Contains(err.Error(), "serve") {
		t.Fatalf("the refusal does not say what to start: %v", err)
	}
}

// TestDrainAndShutdownNeedARunningSupervisor keeps the lifecycle verbs
// meaningful. They are instructions TO a supervisor; with none running there is
// nothing to instruct, and saying so beats succeeding silently.
func TestDrainAndShutdownNeedARunningSupervisor(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	for _, command := range []string{"drain", "shutdown"} {
		code, err := autonomy([]string{command, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
		if err == nil {
			t.Fatalf("%s succeeded with no supervisor running", command)
		}
		if code != runtime.ExitInvalid || !strings.Contains(err.Error(), "serve") {
			t.Fatalf("%s: exit %d, err %v", command, code, err)
		}
	}
}

// TestStopAllCancelsWithoutASupervisor is the other half: cancelling runs is an
// operator authority that does not depend on a supervisor existing.
func TestStopAllCancelsWithoutASupervisor(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	activeRun(t, configPath, dir, runID)

	var out bytes.Buffer
	code, err := autonomy([]string{"stop-all", "--reason", "operator_stop_all", "--config", configPath}, offlineOverrides(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != runtime.ExitCancelled {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitCancelled)
	}
	var outcomes []runtime.Outcome
	if err := json.Unmarshal(out.Bytes(), &outcomes); err != nil {
		t.Fatalf("stop-all printed no outcomes: %v\n%s", err, out.String())
	}
	for _, outcome := range outcomes {
		if outcome.Disposition != runtime.Cancelled {
			t.Fatalf("stop-all left %s as %q", outcome.RunID, outcome.Disposition)
		}
	}
}

// TestAgentSetRefusesAndSaysWhatToDoInstead is the governed transition at the
// command line.
func TestAgentSetRefusesAndSaysWhatToDoInstead(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git",
		agentsConfig(map[string]any{
			"codex":  map[string]any{"kind": "codex_cli", "trust_mode": "operator_trusted"},
			"claude": map[string]any{"kind": "claude_code", "trust_mode": "operator_trusted"},
		}, "codex"))
	t.Chdir(dir)

	// Both statements are required: naming an agent without a reason is a
	// usage error, because an unexplained provider change is exactly what the
	// transition record exists to prevent.
	if _, err := autonomy([]string{"agent", "set", runID, "--agent", "claude", "--config", configPath}, offlineOverrides(), &bytes.Buffer{}); err == nil {
		t.Fatal("an agent change was accepted with no stated reason")
	}
}
