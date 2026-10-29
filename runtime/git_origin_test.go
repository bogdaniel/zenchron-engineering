package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chain turns a controller-rooted process list into the parent lookup the
// establishment walks. The slice reads in execution order - controller first,
// the broker last - because that is the order the topology is described in
// everywhere else, and a reversed fixture is a silent way to assert the
// opposite of what a case is named for.
func chain(pids ...int) func(int) (int, bool) {
	parent := map[int]int{}
	for i := 1; i < len(pids); i++ {
		parent[pids[i]] = pids[i-1]
	}
	return func(pid int) (int, bool) {
		if p, ok := parent[pid]; ok {
			return p, true
		}
		// Unlisted means the walk left the fixture. Reporting init rather than
		// a read failure keeps "the controller is not an ancestor" and "the
		// process table could not be read" distinguishable, which are two
		// different unknowns.
		return 1, true
	}
}

func TestGitActorOriginIsEstablishedFromTopologyAndNeverPromoted(t *testing.T) {
	const controller, provider, toolContext, buildTool, testBinary, broker = 100, 200, 300, 400, 500, 600
	claude := GitOriginAnchor{ControllerPID: controller, ToolCallDepth: ToolCallDepthFor(AgentKindClaudeCode)}
	undeclared := GitOriginAnchor{ControllerPID: controller}

	for _, test := range []struct {
		name   string
		anchor GitOriginAnchor
		parent func(int) (int, bool)
		want   GitActorOrigin
	}{
		{"the controller's own Git", claude, chain(controller, broker), GitOriginZenchronRuntime},
		{"the provider's own machinery", claude, chain(controller, provider, broker), GitOriginProviderRuntime},
		{"one model tool call", claude, chain(controller, provider, toolContext, broker), GitOriginModelTool},
		// The 51-commit shape from run-0bd3b7ab3a773b88566570b7a99a7a0e: the
		// model ran `go test ./...` and a test binary three generations lower
		// attempted the commits. Calling that model_tool is the exact false
		// conclusion #259 exists to eliminate.
		{"a test binary the model's command started", claude,
			chain(controller, provider, toolContext, buildTool, testBinary, broker), GitOriginWorkloadSubprocess},
		// An unmeasured provider has nothing to place a tool call at, so the
		// same chain must decline to name the model rather than assume the
		// topology of the one kind that was measured.
		{"an undeclared provider never yields the model", undeclared,
			chain(controller, provider, toolContext, broker), GitOriginWorkloadSubprocess},
		{"no anchor", GitOriginAnchor{}, chain(controller, provider, broker), GitOriginUnknown},
		{"the controller is not an ancestor", claude, chain(999, provider, broker), GitOriginUnknown},
		{"the process table cannot be read", claude,
			func(int) (int, bool) { return 0, false }, GitOriginUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := EstablishGitActorOrigin(test.anchor, broker, test.parent)
			if got.Origin != test.want {
				t.Fatalf("origin = %q, want %q (basis %q)", got.Origin, test.want, got.Basis)
			}
			if strings.TrimSpace(got.Basis) == "" {
				t.Fatal("every attribution must say how it was established")
			}
		})
	}
}

// A cycle in the reported ancestry must terminate the walk. The process table
// is read from outside this process and nothing here can promise it is a tree.
func TestGitActorOriginTerminatesOnACycle(t *testing.T) {
	cyclic := func(pid int) (int, bool) {
		if pid == 10 {
			return 11, true
		}
		return 10, true
	}
	got := EstablishGitActorOrigin(GitOriginAnchor{ControllerPID: 1234}, 10, cyclic)
	if got.Origin != GitOriginUnknown {
		t.Fatalf("origin = %q, want %q", got.Origin, GitOriginUnknown)
	}
}

// The refusal record is where the attribution has to survive: the count and
// the shape were already durable, and the actor is what made them readable.
func TestRefusalRecordsTheActorIncludingUnknown(t *testing.T) {
	for _, test := range []struct {
		name  string
		actor GitActorAttribution
		want  GitActorOrigin
	}{
		{"an established actor", GitActorAttribution{Origin: GitOriginProviderRuntime, Basis: "measured"}, GitOriginProviderRuntime},
		{"an actor nobody established", GitActorAttribution{}, GitOriginUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "refused.jsonl")
			if _, err := refuseGitCommand(t.TempDir(), log, []string{"git", "commit", "-m", "x"},
				"the runtime owns this", GitOperationRuntimeOwned, GitTargetCandidate, test.actor, &strings.Builder{}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			var recorded GitRefusal
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &recorded); err != nil {
				t.Fatal(err)
			}
			if recorded.Origin != test.want {
				t.Fatalf("recorded origin = %q, want %q", recorded.Origin, test.want)
			}
		})
	}
}

// THE DECISION MUST NOT MOVE. Attribution informs interpretation and never
// authority, so the same argv against the same resource has to be refused
// identically whoever asked - including when nobody could be established.
func TestOriginChangesNoDecision(t *testing.T) {
	candidate, scratch := t.TempDir(), t.TempDir()
	var codes []int
	var reasons []string
	for _, actor := range []GitActorAttribution{
		{Origin: GitOriginModelTool}, {Origin: GitOriginProviderRuntime},
		{Origin: GitOriginZenchronRuntime}, {Origin: GitOriginWorkloadSubprocess},
		{Origin: GitOriginUnknown}, {},
	} {
		log := filepath.Join(t.TempDir(), "refused.jsonl")
		var stderr strings.Builder
		code, err := refuseGitCommand(candidate, log, []string{"git", "reset", "--hard"},
			"would replace working-tree state from another revision",
			GitOperationDiscard, GitTargetCandidate, actor, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		codes, reasons = append(codes, code), append(reasons, stderr.String())
		_ = scratch
	}
	for i := range codes {
		if codes[i] != codes[0] || reasons[i] != reasons[0] {
			t.Fatalf("origin %d changed the refusal: code %d/%d", i, codes[i], codes[0])
		}
	}
}

// The anchor reaches the broker through the shim, which is the only channel
// that is runtime-owned end to end. A shim that carried no anchor would leave
// every invocation unattributable while looking installed.
func TestGeneratedShimCarriesTheRuntimeAnchor(t *testing.T) {
	dir := t.TempDir()
	guard, err := PrepareGitGuard(dir, ExecutionAttemptRef{
		RunID: "run-origin", OperationID: "run-origin:execution.invoke:initial|1|base", Attempt: 1,
	}, dir, "", []string{"/bin/true"}, AgentKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	shim, err := os.ReadFile(filepath.Join(guard.BinDir, "git"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shim), "--controller-pid") {
		t.Fatalf("the shim names no controller identity:\n%s", shim)
	}
	if !strings.Contains(string(shim), "--tool-call-depth") {
		t.Fatalf("a measured provider kind must declare its tool-call depth:\n%s", shim)
	}

	// An unmeasured kind declares no depth, which is what stops the model from
	// being named on a topology nobody measured.
	unmeasured, err := PrepareGitGuard(dir, ExecutionAttemptRef{
		RunID: "run-origin", OperationID: "run-origin:execution.invoke:initial|1|base", Attempt: 2,
	}, dir, "", []string{"/bin/true"}, AgentKindCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(filepath.Join(unmeasured.BinDir, "git"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(other), "--tool-call-depth") {
		t.Fatalf("an unmeasured provider kind must declare no depth:\n%s", other)
	}
}
