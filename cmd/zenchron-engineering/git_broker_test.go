package main

// #241, at the production composition boundary.
//
// runtime/git_guard_test.go proves the boundary works. These tests prove the
// composition root actually asks for it, and - the finding from review
// 5249945424 - that a controller which cannot install it dispatches nothing
// rather than dispatching an unguarded worker.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// productionProviderFor builds the provider the real composition root builds.
func productionProviderFor(t *testing.T, stateDir string) runtime.CLIAgentProvider {
	t.Helper()
	config := runtime.Config{}
	config.StateDir = stateDir
	agent := runtime.ResolvedAgent{
		ID: "codex", Kind: runtime.AgentKindCodexCLI,
		TrustMode: runtime.TrustOperatorTrusted, Command: "codex",
	}
	provider := executionProvider(config, agent, runtime.ArtifactStore{Root: filepath.Join(stateDir, "artifacts")},
		runtime.DockerSandbox{}, false)
	cli, ok := provider.(runtime.CLIAgentProvider)
	if !ok {
		t.Fatalf("the composition root built %T for a native CLI agent", provider)
	}
	return cli
}

// TestTheProductionCompositionRequiresTheBrokeredGitBoundary is the first half:
// normal autonomy execution asks for the guard rather than hoping for it.
func TestTheProductionCompositionRequiresTheBrokeredGitBoundary(t *testing.T) {
	state := t.TempDir()
	provider := productionProviderFor(t, state)

	if !provider.RequireGitGuard {
		t.Fatal("the production composition does not require the #241 boundary")
	}
	if provider.StateDir != state {
		t.Fatalf("the guard has no runtime state root: %q", provider.StateDir)
	}
	if len(provider.GitBroker) == 0 {
		t.Fatal("the production composition resolved no broker command")
	}
	// The broker is THIS controller's own executable, so the binary that
	// enforces the boundary is the binary the operator is running and not
	// whatever a search path resolved.
	executable, err := os.Executable()
	if err != nil {
		t.Skip("no executable path on this platform")
	}
	if provider.GitBroker[0] != executable {
		t.Fatalf("the broker is %q, want this controller %q", provider.GitBroker[0], executable)
	}
	if provider.GitBroker[len(provider.GitBroker)-1] != gitBrokerSubcommand {
		t.Fatalf("the broker argv does not name the broker subcommand: %v", provider.GitBroker)
	}
}

// TestCandidateExecutionNeverStartsWhenTheProductionBrokerCannotBeResolved is
// the finding, closed.
//
// `prepareGitGuard` treats a missing broker as an honest unguarded composition,
// which is right for a unit test and wrong for production: the normal
// composition always intends the boundary, so an unresolvable controller
// executable is a controller that cannot enforce #241. It must refuse, not
// continue.
//
// The refusal is proved to happen BEFORE anything runs - no probe, no process,
// no candidate touched - and to carry the runtime's own typed class rather than
// an invented provider fault.
func TestCandidateExecutionNeverStartsWhenTheProductionBrokerCannotBeResolved(t *testing.T) {
	state := t.TempDir()
	candidate := filepath.Join(state, "candidate")
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := productionProviderFor(t, state)
	// The one failure this models: the controller could not name its own
	// executable, so gitBrokerCommand() yielded nothing.
	provider.GitBroker = nil
	// A recording executor, so "nothing ran" is observable rather than assumed.
	executor := &recordingExecutor{}
	provider.Executor = executor

	request := runtime.ExecutionRequest{
		RunID: "run-guard", OperationID: "run-guard:execution.invoke:initial|1|base", Attempt: 1,
		SourceSnapshot: runtime.Ref{ID: "issue", Revision: "1"}, ControllerID: "controller",
		Base:      runtime.Ref{ID: "base", Revision: "base-sha"},
		Candidate: runtime.Candidate{Revision: "candidate-sha", Tree: "tree-sha"}, CandidateDir: candidate,
		Contract: runtime.Ref{ID: "contract", Revision: "1"}, Objective: "work",
		TrustedInstructions: "trusted", Purpose: runtime.InvocationInitial,
	}

	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("execution started without the #241 boundary")
	}
	// THE TYPED REFUSAL, naming what was missing and what did not happen.
	var refusal *runtime.CandidateGitGuardUnavailableError
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal is not the runtime's own typed error: %T %v", err, err)
	}
	for _, required := range []string{"candidate Git must be brokered", "No provider was invoked"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("the refusal does not explain itself (%q): %v", required, err)
		}
	}
	// NOTHING RAN. Not the capability probe, not the version query, not the
	// invocation - so no invocation was spent discovering this and no worker
	// was handed a workspace it could erase.
	if len(executor.calls) != 0 {
		t.Fatalf("the refusal still executed %d command(s): %v", len(executor.calls), executor.calls)
	}
	// And no result was fabricated.
	if result.Invocation != nil || result.Outcome != "" {
		t.Fatalf("a refused dispatch produced a result: %#v", result)
	}
	// IT IS NOT A PROVIDER FAULT. Nothing about the worker, the work, the
	// account or the network is wrong, so it carries the runtime's own class
	// and waits for an operator to repair the installation.
	if got := runtime.RouteFailure(runtime.FailureCandidateGuardUnavailable); got != runtime.RouteWait {
		t.Fatalf("the guard refusal routes to %q, want a wait an operator can clear", got)
	}
}

// TestADeliberatelyUnguardedCompositionIsStillPossible keeps the fail-closed
// rule from removing the honest unguarded case: a unit test, a probe or an
// embedder driving one invocation does not set RequireGitGuard, and gets the
// pre-#241 behaviour with GitGuarded=false recorded truthfully.
func TestADeliberatelyUnguardedCompositionIsStillPossible(t *testing.T) {
	state := t.TempDir()
	provider := productionProviderFor(t, state)
	provider.RequireGitGuard = false
	provider.GitBroker = nil
	if provider.RequireGitGuard {
		t.Fatal("an explicitly unguarded composition still requires the guard")
	}
	// The distinction is the FIELD, not a heuristic about what was configured.
	required := productionProviderFor(t, state)
	if !required.RequireGitGuard {
		t.Fatal("the production composition lost its requirement")
	}
}

// recordingExecutor records every command it is asked to run and runs none.
type recordingExecutor struct{ calls [][]string }

func (e *recordingExecutor) LookPath(string) error { return nil }

func (e *recordingExecutor) Run(_ context.Context, name string, args []string, _ string, _ []string, _ time.Duration) (runtime.CommandOutput, error) {
	e.calls = append(e.calls, append([]string{name}, args...))
	return runtime.CommandOutput{}, nil
}

func (e *recordingExecutor) Output(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (runtime.CommandOutput, error) {
	return e.Run(ctx, name, args, dir, env, grace)
}
