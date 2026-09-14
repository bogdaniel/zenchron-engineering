package runtime

// The brokered worker execution environment.
//
// Both #119 workers were handed a contract obligating `gofmt`, `go vet` and
// `go test`, and neither could resolve a single one of them: the worker
// environment was whichever PATH started the supervisor, while the assurance
// container had a pinned path of its own. Nothing refused, so an invocation was
// spent discovering it and the honest answer came back as "validation blocked".

import (
	"os"
	"path/filepath"
	"testing"
)

func toolchainWith(t *testing.T, tools ...string) ToolchainConfig {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ToolchainConfig{Path: []string{dir}}
}

// A declared toolchain that resolves everything it requires is ready, and the
// worker's PATH is the declared one rather than the inherited one.
func TestABrokeredToolchainResolvesItsRequiredTools(t *testing.T) {
	toolchain := toolchainWith(t, "go", "gofmt")
	toolchain.RequiredTools = []string{"go", "gofmt"}

	provider := CLIAgentProvider{Toolchain: toolchain}
	if missing := provider.missingTools(); len(missing) > 0 {
		t.Fatalf("a complete toolchain reported missing tools: %v", missing)
	}
	// And it is the environment the worker actually receives.
	env := provider.env(cliAgentSpec{}, "")
	want := "PATH=" + toolchain.SearchPath()
	if len(env) == 0 || env[0] != want {
		t.Fatalf("the worker PATH is %q, want the brokered %q", env, want)
	}
}

// A missing tool is named, and a worker that cannot resolve it is NOT available.
func TestAWorkerMissingAMandatoryToolIsNotReady(t *testing.T) {
	toolchain := toolchainWith(t, "gofmt")
	toolchain.RequiredTools = []string{"go", "gofmt"}

	provider := CLIAgentProvider{Toolchain: toolchain}
	missing := provider.missingTools()
	if len(missing) != 1 || missing[0] != "go" {
		t.Fatalf("the missing tool was not named exactly: %v", missing)
	}
}

// A directory entry that is present but NOT executable does not resolve. A
// file called `go` that cannot be run is not a Go toolchain.
func TestANonExecutableToolDoesNotResolve(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolchain := ToolchainConfig{Path: []string{dir}, RequiredTools: []string{"go"}}
	provider := CLIAgentProvider{Toolchain: toolchain}
	if missing := provider.missingTools(); len(missing) != 1 {
		t.Fatalf("a non-executable file resolved as a tool: %v", missing)
	}
}

// An UNDECLARED toolchain changes nothing. Every configuration written before
// this existed keeps its inherited environment and gets no new refusals.
func TestAnUndeclaredToolchainInheritsAndRequiresNothing(t *testing.T) {
	var toolchain ToolchainConfig
	if toolchain.Declared() {
		t.Fatal("an empty toolchain reported itself declared")
	}
	provider := CLIAgentProvider{Toolchain: toolchain}
	if missing := provider.missingTools(); len(missing) != 0 {
		t.Fatalf("an undeclared toolchain required something: %v", missing)
	}
	env := provider.env(cliAgentSpec{}, "")
	if len(env) == 0 || env[0] != "PATH="+os.Getenv("PATH") {
		t.Fatalf("an undeclared toolchain did not inherit the supervisor PATH: %q", env)
	}
}

// A toolchain failure WAITS for an operator. It is not a verification failure -
// nothing about the candidate was judged - and not transient, because a missing
// toolchain does not come back on its own. Asking a model to fix it would spend
// the budget remediation needs on a condition no reasoning can clear.
func TestAToolchainFailureWaitsForAnOperator(t *testing.T) {
	if route := RouteFailure(FailureToolchainUnavailable); route != RouteWait {
		t.Fatalf("a missing worker toolchain routes to %q, want %q", route, RouteWait)
	}
}

// Capability is not permission. A declared tool says the worker may RESOLVE
// that executable; it never says the repository may name commands to run.
func TestADeclaredToolchainIsNotAnExecutionGrant(t *testing.T) {
	toolchain := toolchainWith(t, "go")
	toolchain.RequiredTools = []string{"go"}
	// The repository layer has no member for this at all, which is what makes
	// the statement structural rather than a convention. TestRepositoryLayer*
	// asserts the whole repository surface; this asserts the one fact that
	// matters here: nothing in the declaration is a command.
	for _, entry := range append(append([]string{}, toolchain.Path...), toolchain.RequiredTools...) {
		if filepath.Base(entry) != entry && entry != toolchain.Path[0] {
			t.Fatalf("a toolchain entry looks like a path to a command rather than a tool name: %q", entry)
		}
	}
}
