package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const repository = "bogdaniel/zenchron-engineering"

type commandRunner interface {
	LookPath(string) error
	Output(string, string, ...string) (string, error)
	OutputEnv(string, []string, string, ...string) (string, error)
	Run(string, string, ...string) error
	RunEnv(string, []string, string, ...string) error
}

type osCommands struct{}

func (osCommands) LookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}

func (osCommands) Output(dir, name string, args ...string) (string, error) {
	return osCommands{}.OutputEnv(dir, nil, name, args...)
}

func (osCommands) OutputEnv(dir string, env []string, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", commandError(name, args, output, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (osCommands) Run(dir, name string, args ...string) error {
	return osCommands{}.RunEnv(dir, nil, name, args...)
}

func (osCommands) RunEnv(dir string, env []string, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func commandError(name string, args []string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
}

type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	URL    string `json:"url"`
}

type pullRequest struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	HeadRefOID  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Body        string `json:"body"`
}

type executorReport struct {
	Validation              []validationResult `json:"validation"`
	ArchitecturalDeviations []string           `json:"architectural_deviations"`
	UnresolvedQuestions     []string           `json:"unresolved_questions"`
}

type validationResult struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Result  string `json:"result"`
}

type harnessCheck struct {
	ID      string
	Command string
}

var bootstrapChecks = []harnessCheck{
	{ID: "format", Command: "gofmt -l <tracked Go files>"},
	{ID: "vet", Command: "go vet ./..."},
	{ID: "test", Command: "go test ./..."},
}

func selfhostIssue(rawNumber string, commands commandRunner, stdout io.Writer) error {
	number, err := strconv.Atoi(rawNumber)
	if err != nil || number < 1 {
		return fmt.Errorf("issue number must be a positive integer")
	}
	for _, tool := range []string{"git", "gh", "codex"} {
		if err := commands.LookPath(tool); err != nil {
			return fmt.Errorf("required executable %q not found in PATH", tool)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	root, err := commands.Output(cwd, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not a Git repository: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	runtime, err := resolveGoRuntime(root, commands)
	if err != nil {
		return fmt.Errorf("resolve Go runtime before repository mutation: %w", err)
	}
	fmt.Fprintf(stdout, "Go runtime: %s\n", runtime)
	origin, err := commands.Output(root, "git", "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read origin remote: %w", err)
	}
	if !expectedOrigin(origin) {
		return fmt.Errorf("wrong origin remote: got %q, want %q", origin, "github.com/"+repository)
	}
	pushOrigin, err := commands.Output(root, "git", "remote", "get-url", "--push", "origin")
	if err != nil {
		return fmt.Errorf("read origin push remote: %w", err)
	}
	if !expectedOrigin(pushOrigin) {
		return fmt.Errorf("wrong origin push remote: got %q, want %q", pushOrigin, "github.com/"+repository)
	}

	if _, err := commands.Output(root, "gh", "auth", "status"); err != nil {
		return fmt.Errorf("GitHub CLI authentication unavailable: %w", err)
	}
	if _, err := commands.Output(root, "codex", "login", "status"); err != nil {
		return fmt.Errorf("Codex CLI authentication unavailable: %w", err)
	}
	identity, err := commands.Output(root, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return fmt.Errorf("identify GitHub repository: %w", err)
	}
	if identity != repository {
		return fmt.Errorf("wrong repository: got %q, want %q", identity, repository)
	}
	branch, err := commands.Output(root, "git", "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("read current branch: %w", err)
	}
	if branch != "main" {
		return fmt.Errorf("unexpected branch %q: start from main", branch)
	}
	status, err := commands.Output(root, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect working tree: %w", err)
	}
	if status != "" {
		return errors.New("working tree is not clean; tracked and untracked changes are refused")
	}
	ignored, err := commands.Output(root, "git", "ls-files", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("inspect ignored files: %w", err)
	}
	if ignored != "" {
		return errors.New("ignored files are present; self-hosting refuses local data it cannot safely preserve")
	}
	if _, err := commands.Output(root, "git", "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("refresh origin/main: %w", err)
	}
	base, err := commands.Output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read local main revision: %w", err)
	}
	remoteMain, err := commands.Output(root, "git", "rev-parse", "origin/main")
	if err != nil {
		return fmt.Errorf("read origin/main revision: %w", err)
	}
	if base != remoteMain {
		return fmt.Errorf("local main is not synchronized with origin/main: local %s, remote %s", base, remoteMain)
	}

	issueData, err := commands.Output(root, "gh", "issue", "view", rawNumber, "--repo", repository, "--json", "number,title,body,state,url")
	if err != nil {
		return fmt.Errorf("retrieve issue #%d: %w", number, err)
	}
	var target issue
	if err := json.Unmarshal([]byte(issueData), &target); err != nil {
		return fmt.Errorf("decode issue #%d: %w", number, err)
	}
	if target.Number != number {
		return fmt.Errorf("GitHub returned issue #%d for requested issue #%d", target.Number, number)
	}
	if target.State != "OPEN" {
		return fmt.Errorf("issue #%d is %s, not open", number, strings.ToLower(target.State))
	}
	trackerData, err := commands.Output(root, "gh", "issue", "view", "12", "--repo", repository, "--json", "number,title,body,state,url")
	if err != nil {
		return fmt.Errorf("retrieve M0 tracker #12: %w", err)
	}
	var tracker issue
	if err := json.Unmarshal([]byte(trackerData), &tracker); err != nil {
		return fmt.Errorf("decode M0 tracker #12: %w", err)
	}
	if tracker.Number != 12 {
		return fmt.Errorf("GitHub returned issue #%d for M0 tracker #12", tracker.Number)
	}

	issueBranch := fmt.Sprintf("issue-%d", number)
	localBranch, err := commands.Output(root, "git", "branch", "--list", "--format=%(refname:short)", issueBranch)
	if err != nil {
		return fmt.Errorf("check local issue branch: %w", err)
	}
	if localBranch != "" {
		return fmt.Errorf("issue branch %q already exists locally", issueBranch)
	}
	remoteBranch, err := commands.Output(root, "git", "ls-remote", "--heads", "origin", "refs/heads/"+issueBranch)
	if err != nil {
		return fmt.Errorf("check remote issue branch: %w", err)
	}
	if remoteBranch != "" {
		return fmt.Errorf("issue branch %q already exists on origin", issueBranch)
	}
	existingPRs, err := commands.Output(root, "gh", "pr", "list", "--repo", repository, "--state", "all", "--head", issueBranch, "--json", "number,url")
	if err != nil {
		return fmt.Errorf("check existing pull requests: %w", err)
	}
	var existing []pullRequest
	if err := json.Unmarshal([]byte(existingPRs), &existing); err != nil {
		return fmt.Errorf("decode existing pull requests: %w", err)
	}
	if len(existing) != 0 {
		return fmt.Errorf("pull request for %q already exists: %s", issueBranch, existing[0].URL)
	}

	if err := commands.Run(root, "git", "switch", "-c", issueBranch); err != nil {
		return fmt.Errorf("create issue branch %q: %w", issueBranch, err)
	}
	reportFile, schemaFile, cleanup, err := temporaryReportFiles()
	if err != nil {
		return err
	}
	defer cleanup()
	contextFile, contextDir, removeContext, err := writeIssueContext(target, tracker)
	if err != nil {
		return err
	}
	defer removeContext()
	prompt := selfhostPrompt(number, issueBranch, contextFile)
	if err := commands.Run(root, "codex", "--ask-for-approval", "never", "--sandbox", "workspace-write", "exec", "--ignore-user-config", "--cd", root, "--add-dir", contextDir, "--output-schema", schemaFile, "--output-last-message", reportFile, prompt); err != nil {
		return fmt.Errorf("Codex execution failed; work remains on %q for inspection: %w", issueBranch, err)
	}

	branch, err = commands.Output(root, "git", "branch", "--show-current")
	if err != nil || branch != issueBranch {
		return fmt.Errorf("Codex left expected branch %q (current %q): %w", issueBranch, branch, err)
	}
	status, err = commands.Output(root, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect final working tree: %w", err)
	}
	if status == "" {
		return errors.New("Codex produced no working-tree change")
	}
	ignored, err = commands.Output(root, "git", "ls-files", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("inspect final ignored files: %w", err)
	}
	if ignored != "" {
		return errors.New("Codex produced ignored files; refusing incomplete handoff")
	}
	currentHead, err := commands.Output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read post-execution revision: %w", err)
	}
	if currentHead != base {
		return errors.New("Codex changed Git history; refusing executor-owned publication")
	}
	reportData, err := os.ReadFile(reportFile)
	if err != nil {
		return fmt.Errorf("read Codex report: %w", err)
	}
	var report executorReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		return fmt.Errorf("decode Codex report: %w", err)
	}
	return publishCandidate(root, number, issueBranch, target, runtime, &report, commands, stdout)
}

// selfhostResume publishes the uncommitted candidate left by an interrupted
// selfhost issue run. It is intentionally narrow: it accepts only the exact
// issue branch at origin/main with no remote branch or PR to avoid adopting
// unrelated local work or history.
func selfhostResume(rawNumber string, commands commandRunner, stdout io.Writer) error {
	number, err := strconv.Atoi(rawNumber)
	if err != nil || number < 1 {
		return errors.New("issue number must be a positive integer")
	}
	for _, tool := range []string{"git", "gh"} {
		if err := commands.LookPath(tool); err != nil {
			return fmt.Errorf("required executable %q not found in PATH", tool)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	root, err := commands.Output(cwd, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not a Git repository: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	runtime, err := resolveGoRuntime(root, commands)
	if err != nil {
		return fmt.Errorf("resolve Go runtime before repository mutation: %w", err)
	}
	fmt.Fprintf(stdout, "Go runtime: %s\n", runtime)
	for _, remote := range []struct {
		args []string
		name string
	}{{[]string{"remote", "get-url", "origin"}, "origin"}, {[]string{"remote", "get-url", "--push", "origin"}, "origin push"}} {
		value, err := commands.Output(root, "git", remote.args...)
		if err != nil {
			return fmt.Errorf("read %s remote: %w", remote.name, err)
		}
		if !expectedOrigin(value) {
			return fmt.Errorf("wrong %s remote: got %q, want %q", remote.name, value, "github.com/"+repository)
		}
	}
	if _, err := commands.Output(root, "gh", "auth", "status"); err != nil {
		return fmt.Errorf("GitHub CLI authentication unavailable: %w", err)
	}
	identity, err := commands.Output(root, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return fmt.Errorf("identify GitHub repository: %w", err)
	}
	if identity != repository {
		return fmt.Errorf("wrong repository: got %q, want %q", identity, repository)
	}
	issueBranch := fmt.Sprintf("issue-%d", number)
	branch, err := commands.Output(root, "git", "branch", "--show-current")
	if err != nil || branch != issueBranch {
		return fmt.Errorf("unexpected branch %q: resume requires %q", branch, issueBranch)
	}
	status, err := commands.Output(root, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect working tree: %w", err)
	}
	if status == "" {
		return errors.New("working tree has no interrupted candidate changes to resume")
	}
	ignored, err := commands.Output(root, "git", "ls-files", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("inspect ignored files: %w", err)
	}
	if ignored != "" {
		return errors.New("ignored files are present; resume refuses local data it cannot safely preserve")
	}
	if _, err := commands.Output(root, "git", "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("refresh origin/main: %w", err)
	}
	head, err := commands.Output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read interrupted branch revision: %w", err)
	}
	remoteMain, err := commands.Output(root, "git", "rev-parse", "origin/main")
	if err != nil {
		return fmt.Errorf("read origin/main revision: %w", err)
	}
	if head != remoteMain {
		return fmt.Errorf("interrupted branch history is not exactly origin/main; refusing unrelated history (branch %s, main %s)", head, remoteMain)
	}
	remoteBranch, err := commands.Output(root, "git", "ls-remote", "--heads", "origin", "refs/heads/"+issueBranch)
	if err != nil {
		return fmt.Errorf("check remote issue branch: %w", err)
	}
	if remoteBranch != "" {
		return fmt.Errorf("issue branch %q already exists on origin; refusing resume", issueBranch)
	}
	existingPRs, err := commands.Output(root, "gh", "pr", "list", "--repo", repository, "--state", "all", "--head", issueBranch, "--json", "number,url")
	if err != nil {
		return fmt.Errorf("check existing pull requests: %w", err)
	}
	var existing []pullRequest
	if err := json.Unmarshal([]byte(existingPRs), &existing); err != nil {
		return fmt.Errorf("decode existing pull requests: %w", err)
	}
	if len(existing) != 0 {
		return fmt.Errorf("pull request for %q already exists: %s", issueBranch, existing[0].URL)
	}
	issueData, err := commands.Output(root, "gh", "issue", "view", rawNumber, "--repo", repository, "--json", "number,title,body,state,url")
	if err != nil {
		return fmt.Errorf("retrieve issue #%d: %w", number, err)
	}
	var target issue
	if err := json.Unmarshal([]byte(issueData), &target); err != nil {
		return fmt.Errorf("decode issue #%d: %w", number, err)
	}
	if target.Number != number || target.State != "OPEN" {
		return fmt.Errorf("issue #%d is not an open matching issue", number)
	}
	return publishCandidate(root, number, issueBranch, target, runtime, nil, commands, stdout)
}

func publishCandidate(root string, number int, issueBranch string, target issue, runtime goRuntime, report *executorReport, commands commandRunner, stdout io.Writer) error {
	checks, err := verifyBootstrapChecks(runtime)
	if err != nil {
		return err
	}
	if err := commands.Run(root, "git", "add", "--all"); err != nil {
		return fmt.Errorf("stage candidate changes: %w", err)
	}
	if err := commands.Run(root, "git", "commit", "-m", fmt.Sprintf("Implement issue #%d", number)); err != nil {
		return fmt.Errorf("commit candidate changes: %w", err)
	}
	head, err := commands.Output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read final revision: %w", err)
	}
	if err := commands.Run(root, "git", "push", "--set-upstream", "origin", issueBranch); err != nil {
		return fmt.Errorf("push issue branch %q: %w", issueBranch, err)
	}
	prBodyFile, err := writePRBody(target)
	if err != nil {
		return err
	}
	defer os.Remove(prBodyFile)
	if err := commands.Run(root, "gh", "pr", "create", "--repo", repository, "--base", "main", "--head", issueBranch, "--title", fmt.Sprintf("Issue #%d: %s", number, target.Title), "--body-file", prBodyFile); err != nil {
		return fmt.Errorf("create pull request: %w", err)
	}
	prData, err := commands.Output(root, "gh", "pr", "view", issueBranch, "--repo", repository, "--json", "number,url,state,headRefName,headRefOid,baseRefName,body")
	if err != nil {
		return fmt.Errorf("find required pull request for %q: %w", issueBranch, err)
	}
	var pr pullRequest
	if err := json.Unmarshal([]byte(prData), &pr); err != nil {
		return fmt.Errorf("decode pull request: %w", err)
	}
	if pr.State != "OPEN" || pr.HeadRefName != issueBranch || pr.BaseRefName != "main" || pr.HeadRefOID != head {
		return fmt.Errorf("pull request does not match required open %s -> main at %s", issueBranch, head)
	}
	if !strings.Contains(strings.ToLower(pr.Body), strings.ToLower(fmt.Sprintf("closes #%d", number))) {
		return fmt.Errorf("pull request must contain %q", fmt.Sprintf("Closes #%d", number))
	}
	commentFile, err := writeComment(target, issueBranch, pr, head, runtime, report, checks)
	if err != nil {
		return err
	}
	defer os.Remove(commentFile)
	if err := commands.Run(root, "gh", "pr", "comment", strconv.Itoa(pr.Number), "--repo", repository, "--body-file", commentFile); err != nil {
		return fmt.Errorf("publish durable pull request handoff: %w", err)
	}
	fmt.Fprintf(stdout, "Handoff published: %s\nHead: %s\nStopped before merge; external review is required.\n", pr.URL, head)
	return nil
}

func selfhostPrompt(number int, branch, contextFile string) string {
	return fmt.Sprintf(`Implement GitHub issue #%d only. Repository-owned context is authoritative.

Before editing, read AGENTS.md, docs/principles.md, docs/architecture.md, all accepted docs/adr/, docs/spec/v0.1.md, and the live GitHub snapshots for issue #%d and M0 tracker #12 at %s.

You are already on dedicated branch %s. Stay on it. Follow the issue acceptance criteria and run gofmt -l ., go vet ./..., and go test ./.... Do not commit, push, open or modify a PR, or merge; the bootstrap harness owns Git and GitHub publication.

Report executor observations using validation IDs format, vet, and test where applicable; record the actual command/environment used separately. Report every validation observation, architectural deviation, and unresolved question in the requested structured final response. The bootstrap harness independently reruns deterministic checks before publication.`, number, number, contextFile, branch)
}

const reportSchema = `{"type":"object","properties":{"validation":{"type":"array","minItems":1,"items":{"type":"object","properties":{"id":{"type":"string","enum":["format","vet","test"]},"command":{"type":"string"},"result":{"type":"string","enum":["pass","fail","not_run"]}},"required":["id","command","result"],"additionalProperties":false}},"architectural_deviations":{"type":"array","items":{"type":"string"}},"unresolved_questions":{"type":"array","items":{"type":"string"}}},"required":["validation","architectural_deviations","unresolved_questions"],"additionalProperties":false}`

func expectedOrigin(origin string) bool {
	origin = strings.TrimSuffix(origin, ".git")
	return origin == "https://github.com/"+repository ||
		origin == "git@github.com:"+repository ||
		origin == "ssh://git@github.com/"+repository
}

func verifyBootstrapChecks(runtime goRuntime) ([]harnessCheck, error) {
	completed := make([]harnessCheck, 0, len(bootstrapChecks))
	for _, check := range bootstrapChecks {
		var err error
		if check.ID == "format" {
			files, listErr := runtime.commands.Output(runtime.repositoryRoot, "git", "ls-files", "-z", "--", "*.go")
			if listErr != nil {
				return nil, fmt.Errorf("list tracked Go files for harness verification: %w", listErr)
			}
			goFiles := strings.Split(strings.TrimSuffix(files, "\x00"), "\x00")
			if len(goFiles) == 1 && goFiles[0] == "" {
				goFiles = nil
			}
			if len(goFiles) == 0 {
				completed = append(completed, check)
				continue
			}
			var output string
			output, err = runtime.OutputTool("gofmt", append([]string{"-l"}, goFiles...)...)
			if err == nil && strings.TrimSpace(output) != "" {
				err = fmt.Errorf("gofmt reported unformatted files: %s", strings.TrimSpace(output))
			}
		} else if check.ID == "vet" {
			err = runtime.Run("vet", "./...")
		} else {
			err = runtime.Run("test", "./...")
		}
		if err != nil {
			return nil, fmt.Errorf("harness verification %q failed: %w", check.ID, err)
		}
		completed = append(completed, check)
	}
	return completed, nil
}

func temporaryReportFiles() (string, string, func(), error) {
	report, err := os.CreateTemp("", "zenchron-report-*.json")
	if err != nil {
		return "", "", nil, fmt.Errorf("create report file: %w", err)
	}
	report.Close()
	schema, err := os.CreateTemp("", "zenchron-report-schema-*.json")
	if err != nil {
		os.Remove(report.Name())
		return "", "", nil, fmt.Errorf("create report schema: %w", err)
	}
	if _, err := schema.WriteString(reportSchema); err != nil {
		schema.Close()
		os.Remove(report.Name())
		os.Remove(schema.Name())
		return "", "", nil, fmt.Errorf("write report schema: %w", err)
	}
	if err := schema.Close(); err != nil {
		os.Remove(report.Name())
		os.Remove(schema.Name())
		return "", "", nil, fmt.Errorf("close report schema: %w", err)
	}
	cleanup := func() { os.Remove(report.Name()); os.Remove(schema.Name()) }
	return report.Name(), schema.Name(), cleanup, nil
}

func writeComment(target issue, branch string, pr pullRequest, head string, runtime goRuntime, report *executorReport, checks []harnessCheck) (string, error) {
	file, err := os.CreateTemp("", "zenchron-handoff-*.md")
	if err != nil {
		return "", fmt.Errorf("create handoff comment: %w", err)
	}
	defer file.Close()
	fmt.Fprintf(file, "## Zenchron self-host bootstrap handoff\n\n- Target issue: #%d — %s\n- Branch: `%s`\n- PR: #%d — %s\n- Exact head: `%s`\n- Harness Go runtime: `%s`\n- Stopped before merge: yes\n- Authority: external review required; execution and validation do not authorize merge\n\n### Executor-reported observations\n\n", target.Number, target.Title, branch, pr.Number, pr.URL, head, runtime)
	if report == nil {
		fmt.Fprintln(file, "Unavailable: the interrupted run did not preserve a durable executor report.")
	} else {
		reportJSON, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encode executor report: %w", err)
		}
		fmt.Fprintf(file, "```json\n%s\n```\n", reportJSON)
	}
	fmt.Fprint(file, "\n### Harness-verified deterministic checks\n\n")
	for _, check := range checks {
		fmt.Fprintf(file, "- `%s` — pass (`%s`)\n", check.ID, check.Command)
	}
	return file.Name(), nil
}

func writePRBody(target issue) (string, error) {
	file, err := os.CreateTemp("", "zenchron-pr-*.md")
	if err != nil {
		return "", fmt.Errorf("create pull request body: %w", err)
	}
	defer file.Close()
	fmt.Fprintf(file, "Closes #%d\n\nCreated by `zenchron-engineering selfhost issue %d`.\n\nThis is an untrusted candidate change requiring external review. Codex completion, validation, and PR creation do not authorize merge. Automatic merge was not performed.\n", target.Number, target.Number)
	return file.Name(), nil
}

func writeIssueContext(target, tracker issue) (string, string, func(), error) {
	dir, err := os.MkdirTemp("", "zenchron-context-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("create issue context directory: %w", err)
	}
	data, err := json.MarshalIndent(struct {
		TargetIssue issue `json:"target_issue"`
		M0Tracker   issue `json:"m0_tracker"`
	}{target, tracker}, "", "  ")
	if err != nil {
		os.RemoveAll(dir)
		return "", "", nil, fmt.Errorf("encode issue context: %w", err)
	}
	path := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", nil, fmt.Errorf("write issue context: %w", err)
	}
	return path, dir, func() { os.RemoveAll(dir) }, nil
}
