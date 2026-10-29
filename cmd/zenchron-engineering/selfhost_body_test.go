package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The expected bodies were produced by the implementation before the write and
// close errors were checked, so they pin that the cleanup changed no output.
func selfhostBodyInputs() (issue, pullRequest, goRuntime, *executorReport, *codexExecution, []harnessCheck) {
	report := &executorReport{Validation: []validationResult{{ID: "test", Command: "go test ./...", Result: "pass"}}, ArchitecturalDeviations: []string{}, UnresolvedQuestions: []string{"none"}}
	execution := &codexExecution{Provider: "codex", Model: "m", AuthMode: "chatgpt", Attempt: 1, MaxAttempts: 3}
	checks := []harnessCheck{{ID: "format", Command: "gofmt -l ."}, {ID: "test", Command: "go test ./..."}}
	return issue{Number: 4, Title: "Fix the thing"}, pullRequest{Number: 9, URL: "https://github.com/acme/repo/pull/9"},
		goRuntime{kind: localGoRuntime, goVersion: "1.25.2", environmentIdentifier: "host"}, report, execution, checks
}

func TestSelfhostBodiesAreUnchangedAndTheTemporaryFileIsTheOnlyOutput(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	target, pr, rt, report, execution, checks := selfhostBodyInputs()
	for _, test := range []struct {
		name  string
		write func() (string, error)
		want  string
	}{
		{"handoff with provenance", func() (string, error) {
			return writeComment(target, "issue-4", pr, "abc123", rt, report, execution, checks)
		}, "## Zenchron self-host bootstrap handoff\n\n- Target issue: #4 — Fix the thing\n- Branch: `issue-4`\n- PR: #9 — https://github.com/acme/repo/pull/9\n- Exact head: `abc123`\n- Harness Go runtime: `local Go 1.25.2 (host)`\n- Stopped before merge: yes\n- Authority: external review required; execution and validation do not authorize merge\n\n### Executor-reported observations\n\n- Execution provider: `codex`\n- Codex model: `m`\n- Codex authentication mode: `chatgpt`\n- Successful attempt: 1/3\n```json\n{\n  \"validation\": [\n    {\n      \"id\": \"test\",\n      \"command\": \"go test ./...\",\n      \"result\": \"pass\"\n    }\n  ],\n  \"architectural_deviations\": [],\n  \"unresolved_questions\": [\n    \"none\"\n  ]\n}\n```\n\n### Harness-verified deterministic checks\n\n- `format` — pass (`gofmt -l .`)\n- `test` — pass (`go test ./...`)\n"},
		{"handoff of an interrupted resume", func() (string, error) { return writeComment(target, "issue-4", pr, "abc123", rt, nil, nil, nil) }, "## Zenchron self-host bootstrap handoff\n\n- Target issue: #4 — Fix the thing\n- Branch: `issue-4`\n- PR: #9 — https://github.com/acme/repo/pull/9\n- Exact head: `abc123`\n- Harness Go runtime: `local Go 1.25.2 (host)`\n- Stopped before merge: yes\n- Authority: external review required; execution and validation do not authorize merge\n\n### Executor-reported observations\n\n- Codex execution provenance: unavailable (interrupted-run resume)\nUnavailable: the interrupted run did not preserve a durable executor report.\n\n### Harness-verified deterministic checks\n\n"},
		{"pull request body", func() (string, error) { return writePRBody(target) }, "Closes #4\n\nCreated by `zenchron-engineering selfhost issue 4`.\n\nThis is an untrusted candidate change requiring external review. Codex completion, validation, and PR creation do not authorize merge. Automatic merge was not performed.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := test.write()
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Fatalf("body changed:\n%q\nwant:\n%q", got, test.want)
			}
		})
	}
}

// The write and close failure branches need an injectable file and are covered
// by review only; this pins the two outcomes a test can reach.
func TestWriteTemporaryBodyCreateFailureReturnsNoPathAndSuccessKeepsTheFile(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
	if path, err := writeTemporaryBody("x-*.md", "pull request body", "body"); err == nil || path != "" || !strings.Contains(err.Error(), "create pull request body") {
		t.Fatalf("path = %q, err = %v", path, err)
	}

	t.Setenv("TMPDIR", dir)
	path, err := writeTemporaryBody("x-*.md", "pull request body", "body")
	if err != nil || path == "" {
		t.Fatalf("path = %q, err = %v", path, err)
	}
	if content, readErr := os.ReadFile(path); readErr != nil || string(content) != "body" {
		t.Fatalf("content = %q, err = %v", content, readErr)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("the caller removes the published file: %v", err)
	}
}
