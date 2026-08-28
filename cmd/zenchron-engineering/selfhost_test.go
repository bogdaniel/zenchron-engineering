package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
		`"command": "go test ./..."`,
		`"result": "pass"`,
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
		{"missing validation", "required validation", func(f *fakeCommands) {
			f.report.Validation = f.report.Validation[:1]
		}},
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

type fakeCommands struct {
	t            *testing.T
	root         string
	missing      string
	fail         string
	identity     string
	origin       string
	pushOrigin   string
	branch       string
	status       string
	finalStatus  string
	ignored      string
	finalIgnored string
	base         string
	remoteMain   string
	head         string
	target       issue
	tracker      issue
	localBranch  string
	remoteBranch string
	existingPR   bool
	codexErr     error
	pr           pullRequest
	report       executorReport
	switched     bool
	committed    bool
	prompt       string
	comment      string
	prBody       string
	calls        []string
}

func newFakeCommands(t *testing.T) *fakeCommands {
	t.Helper()
	return &fakeCommands{
		t:           t,
		root:        t.TempDir(),
		identity:    repository,
		origin:      "https://github.com/bogdaniel/zenchron-engineering.git",
		pushOrigin:  "https://github.com/bogdaniel/zenchron-engineering.git",
		branch:      "main",
		base:        "base123",
		remoteMain:  "base123",
		head:        "head456",
		finalStatus: " M changed.go",
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
				{Command: "gofmt -l .", Result: "pass"},
				{Command: "go vet ./...", Result: "pass"},
				{Command: "go test ./...", Result: "pass"},
			},
			ArchitecturalDeviations: []string{},
			UnresolvedQuestions:     []string{},
		},
	}
}

func (f *fakeCommands) LookPath(name string) error {
	f.calls = append(f.calls, "lookpath "+name)
	if name == f.missing {
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
