package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoAssuranceUsesIsolatedGitAndExecutableTestScratch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkout")
	commit := initFixtureRepo(t, dir, "go.mod", "module x\n")
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	docker := &assuranceEnvironmentDocker{}
	verifier := scriptedVerifier(t, &docker.scriptedDocker, t.TempDir())
	verifier.Sandbox.Executor = docker
	result, err := verifier.Assure(context.Background(), AssuranceRequest{RunID: "run-env", Attempt: 1, Commit: commit, Tree: strings.TrimSpace(tree), CheckoutDir: dir, Contract: Ref{ID: "contract", Revision: "1"}})
	if err != nil || !result.Passed {
		t.Fatalf("assurance: passed=%v err=%v", result.Passed, err)
	}
	var commands []string
	for _, args := range docker.calls {
		commands = append(commands, strings.Join(args, " "))
	}
	text := strings.Join(commands, "\n")
	for _, want := range []string{"TMPDIR=" + sandboxBuildDir, "git init --quiet; git add --force --all", "/candidate/.git:rw,nosuid,nodev,noexec", "src=" + dir + ",dst=/candidate,readonly", "--network none", "go vet ./...; go test ./..."} {
		if !strings.Contains(text, want) {
			t.Fatalf("assurance environment lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "GIT_DIR=") || strings.Contains(text, "GIT_WORK_TREE=") {
		t.Fatal("assurance overrides fixture Git repositories")
	}
}

type assuranceEnvironmentDocker struct {
	scriptedDocker
	calls [][]string
}

func (d *assuranceEnvironmentDocker) Output(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	d.calls = append(d.calls, append([]string{name}, args...))
	return d.scriptedDocker.Output(ctx, name, args, dir, env, grace)
}
func (d *assuranceEnvironmentDocker) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	return d.Output(ctx, name, args, dir, env, grace)
}
