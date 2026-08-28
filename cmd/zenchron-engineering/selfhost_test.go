package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSelfhostIssuePublishesVerifiedHandoff(t *testing.T) {
	commands := newFakeCommands(t)
	var output bytes.Buffer
	if err := selfhostIssue("4", commands, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commands.prompt, "issue #4") || strings.Contains(commands.prompt, commands.target.Body) {
		t.Fatalf("prompt must point to the issue without copying its body:\n%s", commands.prompt)
	}
	for _, want := range []string{
		"Target issue: #4",
		"Branch: `issue-4`",
		"Exact head: `head456`",
		"Harness Go runtime: `local Go 1.25.1 (host-go:1.25.1)`",
		`"command": "go test ./..."`,
		`"result": "pass"`,
		"Executor-reported observations",
		"Harness-verified deterministic checks",
		"`format` — pass (`gofmt -l <tracked and untracked non-ignored Go files>`)",
		"Stopped before merge: yes",
		"Authority: external review required",
	} {
		if !strings.Contains(commands.comment, want) {
			t.Errorf("handoff missing %q:\n%s", want, commands.comment)
		}
	}
	if strings.Contains(strings.Join(commands.calls, "\n"), " merge ") {
		t.Fatal("self-host flow must not merge")
	}
	if !strings.Contains(strings.Join(commands.calls, "\n"), "--ask-for-approval never --sandbox workspace-write") {
		t.Fatal("Codex was not run with the bounded non-interactive policy")
	}
	if !strings.Contains(commands.prBody, "Closes #4") || !strings.Contains(commands.prBody, "do not authorize merge") {
		t.Fatalf("PR body did not preserve the trust boundary:\n%s", commands.prBody)
	}
	if !strings.Contains(output.String(), "Stopped before merge") {
		t.Fatalf("output did not confirm stop boundary: %s", output.String())
	}
}

func TestSelfhostIssueRefusesUnsafeState(t *testing.T) {
	tests := []struct {
		name string
		want string
		edit func(*fakeCommands)
	}{
		{"missing gh", `required executable "gh"`, func(f *fakeCommands) { f.missing = "gh" }},
		{"missing codex", `required executable "codex"`, func(f *fakeCommands) { f.missing = "codex" }},
		{"GitHub auth", "GitHub CLI authentication unavailable", func(f *fakeCommands) { f.fail = "gh auth status" }},
		{"Codex auth", "Codex CLI authentication unavailable", func(f *fakeCommands) { f.fail = "codex login status" }},
		{"wrong origin", "wrong origin remote", func(f *fakeCommands) { f.origin = "https://github.com/someone/else.git" }},
		{"wrong push origin", "wrong origin push remote", func(f *fakeCommands) { f.pushOrigin = "git@github.com:someone/else.git" }},
		{"wrong repository", "wrong repository", func(f *fakeCommands) { f.identity = "someone/else" }},
		{"unexpected branch", "unexpected branch", func(f *fakeCommands) { f.branch = "feature" }},
		{"dirty tree", "working tree is not clean", func(f *fakeCommands) { f.status = "?? notes.txt" }},
		{"ignored files", "ignored files are present", func(f *fakeCommands) { f.ignored = ".env" }},
		{"stale main", "not synchronized", func(f *fakeCommands) { f.remoteMain = "newer" }},
		{"closed issue", "not open", func(f *fakeCommands) { f.target.State = "CLOSED" }},
		{"local issue branch", "already exists locally", func(f *fakeCommands) { f.localBranch = "issue-4" }},
		{"remote issue branch", "already exists on origin", func(f *fakeCommands) { f.remoteBranch = "head refs/heads/issue-4" }},
		{"existing PR", "pull request for", func(f *fakeCommands) { f.existingPR = true }},
		{"Codex failure", "work remains on", func(f *fakeCommands) { f.codexErr = errors.New("agent failed") }},
		{"no changes", "no working-tree change", func(f *fakeCommands) { f.finalStatus = "" }},
		{"new ignored files", "produced ignored files", func(f *fakeCommands) { f.finalIgnored = "build/output" }},
		{"failing harness check", "harness verification", func(f *fakeCommands) { f.formatOutput = "changed.go" }},
		{"wrong PR head", "does not match", func(f *fakeCommands) { f.pr.HeadRefOID = "other" }},
		{"missing issue reference", `must contain "Closes #4"`, func(f *fakeCommands) { f.pr.Body = "No link" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commands := newFakeCommands(t)
			test.edit(commands)
			err := selfhostIssue("4", commands, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSelfhostIssueRefusesUnformattedUntrackedGoFile(t *testing.T) {
	commands := newFakeCommands(t)
	if err := os.WriteFile(filepath.Join(commands.root, "new.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands.goFiles = "changed.go\x00new.go\x00"
	commands.formatOutput = "new.go"

	err := selfhostIssue("4", commands, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "harness verification \"format\" failed") {
		t.Fatalf("error = %v, want format verification failure", err)
	}

	calls := strings.Join(commands.calls, "\n")
	if !strings.Contains(calls, "git ls-files -z --cached --others --exclude-standard -- *.go") {
		t.Fatalf("format check did not enumerate tracked and untracked non-ignored Go files:\n%s", calls)
	}
	if !strings.Contains(calls, "gofmt -l changed.go new.go") {
		t.Fatalf("format check did not include untracked Go file:\n%s", calls)
	}
	if strings.Contains(calls, "git add --all") || commands.committed {
		t.Fatalf("unformatted untracked Go file must prevent publication:\n%s", calls)
	}
}

func TestSelfhostIssueAcceptsDockerExecutorObservationWhenHarnessVerifiesChecks(t *testing.T) {
	commands := newFakeCommands(t)
	commands.report.Validation = []validationResult{
		{ID: "format", Command: "docker run golang:1.25 gofmt -l .", Result: "pass"},
		{ID: "vet", Command: "docker run golang:1.25 go vet ./...", Result: "pass"},
		{ID: "test", Command: "docker run golang:1.25 go test ./...", Result: "pass"},
	}
	if err := selfhostIssue("4", commands, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commands.comment, `"command": "docker run golang:1.25 go test ./..."`) {
		t.Fatalf("executor observation was not retained:\n%s", commands.comment)
	}
}

func TestSelfhostResumePublishesInterruptedCandidate(t *testing.T) {
	commands := newFakeCommands(t)
	commands.branch = "issue-4"
	commands.status = " M changed.go"
	var output bytes.Buffer
	if err := selfhostResume("4", commands, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(commands.calls, "\n"), "codex ") {
		t.Fatal("resume must not invoke Codex")
	}
	if !strings.Contains(commands.comment, "Harness-verified deterministic checks") {
		t.Fatalf("resume handoff missing harness checks:\n%s", commands.comment)
	}
}

func TestSelfhostResumeRefusesUnsafeInterruptedState(t *testing.T) {
	tests := []struct {
		name string
		want string
		edit func(*fakeCommands)
	}{
		{"wrong branch", "resume requires", func(f *fakeCommands) {}},
		{"clean tree", "no interrupted candidate", func(f *fakeCommands) { f.branch = "issue-4" }},
		{"history", "refusing unrelated history", func(f *fakeCommands) { f.branch, f.status, f.base = "issue-4", " M changed.go", "candidate" }},
		{"remote branch", "already exists on origin", func(f *fakeCommands) {
			f.branch, f.status, f.remoteBranch = "issue-4", " M changed.go", "head refs/heads/issue-4"
		}},
		{"existing PR", "pull request for", func(f *fakeCommands) { f.branch, f.status, f.existingPR = "issue-4", " M changed.go", true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commands := newFakeCommands(t)
			test.edit(commands)
			err := selfhostResume("4", commands, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

type fakeCommands struct {
	t                 *testing.T
	root              string
	missing           string
	goUnavailable     bool
	dockerUnavailable bool
	fail              string
	identity          string
	origin            string
	pushOrigin        string
	branch            string
	status            string
	finalStatus       string
	ignored           string
	finalIgnored      string
	goFiles           string
	formatOutput      string
	base              string
	remoteMain        string
	head              string
	target            issue
	tracker           issue
	localBranch       string
	remoteBranch      string
	existingPR        bool
	codexErr          error
	pr                pullRequest
	report            executorReport
	switched          bool
	committed         bool
	prompt            string
	comment           string
	prBody            string
	calls             []string
}

func newFakeCommands(t *testing.T) *fakeCommands {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/selfhost\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "changed.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeCommands{
		t:           t,
		root:        root,
		identity:    repository,
		origin:      "https://github.com/bogdaniel/zenchron-engineering.git",
		pushOrigin:  "https://github.com/bogdaniel/zenchron-engineering.git",
		branch:      "main",
		base:        "base123",
		remoteMain:  "base123",
		head:        "head456",
		finalStatus: " M changed.go",
		goFiles:     "changed.go",
		target: issue{
			Number: 4,
			Title:  "ProjectModel snapshot",
			Body:   "ISSUE_BODY_MUST_NOT_BE_COPIED",
			State:  "OPEN",
			URL:    "https://github.com/bogdaniel/zenchron-engineering/issues/4",
		},
		tracker: issue{Number: 12, Title: "M0 tracker", Body: "M0", State: "OPEN"},
		pr: pullRequest{
			Number:      44,
			URL:         "https://github.com/bogdaniel/zenchron-engineering/pull/44",
			State:       "OPEN",
			HeadRefName: "issue-4",
			HeadRefOID:  "head456",
			BaseRefName: "main",
			Body:        "Closes #4",
		},
		report: executorReport{
			Validation: []validationResult{
				{ID: "format", Command: "gofmt -l .", Result: "pass"},
				{ID: "vet", Command: "go vet ./...", Result: "pass"},
				{ID: "test", Command: "go test ./...", Result: "pass"},
			},
			ArchitecturalDeviations: []string{},
			UnresolvedQuestions:     []string{},
		},
	}
}

func (f *fakeCommands) LookPath(name string) error {
	f.calls = append(f.calls, "lookpath "+name)
	if name == f.missing || name == "go" && f.goUnavailable || name == "docker" && f.dockerUnavailable {
		return errors.New("missing")
	}
	return nil
}

func (f *fakeCommands) Output(_ string, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if call == f.fail {
		return "", errors.New("failed")
	}
	switch call {
	case "go version":
		return "go version go1.25.1 test/arch", nil
	case "git ls-files -z --cached --others --exclude-standard -- *.go":
		return f.goFiles, nil
	case "gofmt -l changed.go":
		return f.formatOutput, nil
	case "gofmt -l changed.go new.go":
		return f.formatOutput, nil
	case "git rev-parse --show-toplevel":
		return f.root, nil
	case "git remote get-url origin":
		return f.origin, nil
	case "git remote get-url --push origin":
		return f.pushOrigin, nil
	case "gh auth status", "codex login status", "git fetch --quiet origin main":
		return "", nil
	case "gh repo view --json nameWithOwner --jq .nameWithOwner":
		return f.identity, nil
	case "git branch --show-current":
		if f.switched {
			return "issue-4", nil
		}
		return f.branch, nil
	case "git status --porcelain --untracked-files=all":
		if f.switched {
			return f.finalStatus, nil
		}
		return f.status, nil
	case "git ls-files --others --ignored --exclude-standard":
		if f.switched {
			return f.finalIgnored, nil
		}
		return f.ignored, nil
	case "git rev-parse HEAD":
		if f.committed {
			return f.head, nil
		}
		return f.base, nil
	case "git rev-parse origin/main":
		return f.remoteMain, nil
	case "gh issue view 4 --repo " + repository + " --json number,title,body,state,url":
		return mustJSON(f.t, f.target), nil
	case "gh issue view 12 --repo " + repository + " --json number,title,body,state,url":
		return mustJSON(f.t, f.tracker), nil
	case "git branch --list --format=%(refname:short) issue-4":
		return f.localBranch, nil
	case "git ls-remote --heads origin refs/heads/issue-4":
		return f.remoteBranch, nil
	case "gh pr list --repo " + repository + " --state all --head issue-4 --json number,url":
		if f.existingPR {
			return `[ {"number":44,"url":"https://example.test/pr/44"} ]`, nil
		}
		return "[]", nil
	case "gh pr view issue-4 --repo " + repository + " --json number,url,state,headRefName,headRefOid,baseRefName,body":
		return mustJSON(f.t, f.pr), nil
	default:
		return "", fmt.Errorf("unexpected Output call: %s", call)
	}
}

func (f *fakeCommands) OutputEnv(dir string, _ []string, name string, args ...string) (string, error) {
	return f.Output(dir, name, args...)
}

func (f *fakeCommands) Run(_ string, name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case name == "git" && slices.Equal(args, []string{"switch", "-c", "issue-4"}):
		f.switched = true
		return nil
	case name == "codex":
		if f.codexErr != nil {
			return f.codexErr
		}
		f.prompt = args[len(args)-1]
		path := argumentAfter(f.t, args, "--output-last-message")
		return os.WriteFile(path, []byte(mustJSON(f.t, f.report)), 0o600)
	case name == "git" && slices.Equal(args, []string{"add", "--all"}):
		return nil
	case name == "go" && (slices.Equal(args, []string{"vet", "./..."}) || slices.Equal(args, []string{"test", "./..."})):
		return nil
	case name == "git" && len(args) == 3 && args[0] == "commit" && args[1] == "-m":
		f.committed = true
		return nil
	case name == "git" && slices.Equal(args, []string{"push", "--set-upstream", "origin", "issue-4"}):
		return nil
	case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "create":
		path := argumentAfter(f.t, args, "--body-file")
		body, err := os.ReadFile(path)
		f.prBody = string(body)
		return err
	case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "comment":
		path := argumentAfter(f.t, args, "--body-file")
		body, err := os.ReadFile(path)
		f.comment = string(body)
		return err
	default:
		return fmt.Errorf("unexpected Run call: %s", call)
	}
}

func (f *fakeCommands) RunEnv(dir string, _ []string, name string, args ...string) error {
	return f.Run(dir, name, args...)
}

func argumentAfter(t *testing.T, args []string, name string) string {
	t.Helper()
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("argument %s missing from %v", name, args)
	return ""
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
