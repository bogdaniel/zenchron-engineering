package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestAutonomyReviewRefusesAMalformedRequestBeforeDoingAnything(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand":      {"review"},
		"pr, no number":      {"review", "pr"},
		"pr, bad number":     {"review", "pr", "zero", "--agent", "claude"},
		"pr, no agent":       {"review", "pr", "7"},
		"status, no number":  {"review", "status"},
		"status, bad number": {"review", "status", "-1"},
		"unknown subcommand": {"review", "nonsense", "7"},
	} {
		code, err := autonomy(args, offlineOverrides(), &bytes.Buffer{})
		if err == nil || code != runtime.ExitInvalid {
			t.Errorf("%s: code=%d err=%v, want an invalid-request refusal", name, code, err)
		}
	}
}

func TestAutonomyReviewStatusReportsNotFoundForAnUnreviewedPR(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	var out bytes.Buffer
	code, err := autonomy([]string{"review", "status", "7", "--config", configPath}, offlineOverrides(), &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("review status: code=%d err=%v\n%s", code, err, out.String())
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("review status output is not JSON: %v\n%s", err, out.String())
	}
	if found, _ := report["found"].(bool); found {
		t.Fatalf("expected found=false for a PR with no independent review yet, got %+v", report)
	}
}

func TestAutonomyReviewStatusRendersAnExistingDecision(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)

	stateDirConfig := loadReviewTestStateDir(t, configPath)
	store, err := runtime.OpenSQLiteOperationStore(stateDirConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	subject := review.Subject{Repository: "zenchron/seeded", PRNumber: 7, HeadSHA: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: "run-seeded",
		ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{{Severity: review.SeverityBlocking, Signature: "real defect"}},
		Reason:   "found a real defect", CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}

	overrides := offlineOverrides()
	fake := overrides.GitHub.(*runtime.FakeGitHubAdapter)
	fake.PullRequests[7] = runtime.GitHubPullRequest{Number: 7, HeadSHA: subject.HeadSHA, State: runtime.GitHubOpen}

	var out bytes.Buffer
	code, err := autonomy([]string{"review", "status", "7", "--config", configPath, "--text"}, overrides, &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("review status: code=%d err=%v\n%s", code, err, out.String())
	}
	rendered := out.String()
	for _, want := range []string{"request_changes", "blocking=1", "stale: false"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered status missing %q:\n%s", want, rendered)
		}
	}
}

// loadReviewTestStateDir reads the state_dir this test's own seededWorkspace
// config was written with, so the CLI under test and this test's direct store
// access open the exact same SQLite database.
func loadReviewTestStateDir(t *testing.T, configPath string) string {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	stateDir, _ := body["state_dir"].(string)
	if stateDir == "" {
		t.Fatalf("config at %s names no state_dir", configPath)
	}
	return stateDir
}
