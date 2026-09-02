package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestProductionCompositionCanBrokerACandidateCommand is the regression for
// M1-C finding #47, and it is deliberately a PRODUCTION-COMPOSITION test.
//
// The existing broker tests all constructed a DockerSandbox by hand and set
// OperationID and StateDir while doing so, so every one of them passed against
// a build in which the real CLI supplied neither. candidate.run therefore
// failed on every brokered call in production - 31 consecutive failures across
// runs for #42 and #46 - with "runtime-owned Docker operation identity and
// state directory are required", while the whole suite stayed green.
//
// This test walks the same path the CLI walks: newComposition builds the shared
// sandbox, executionProvider wraps it, and candidateBoundProvider binds it per
// request. It asserts the binding exists rather than that Docker runs, so it
// needs no daemon and still fails against the pre-#47 omission.
func TestProductionCompositionCanBrokerACandidateCommand(t *testing.T) {
	fixture := newCompositionFixture(t)
	built, err := newComposition(autonomyFlags{Config: fixture.configPath}, autonomyOverrides{})
	if err != nil {
		t.Fatalf("the production composition did not build: %v", err)
	}
	defer built.release()

	bound, ok := built.provider.(candidateBoundProvider)
	if !ok {
		t.Fatalf("the production execution provider is %T, not the candidate-bound one", built.provider)
	}

	// The shared sandbox carries the runtime-owned record location. Without it
	// no brokered command can run at all, whatever else is configured.
	stateDir := bound.base.Broker.Sandbox.StateDir
	if strings.TrimSpace(stateDir) == "" {
		t.Fatal("the production sandbox has no runtime-owned state directory, so every brokered candidate command refuses")
	}
	if !strings.HasPrefix(stateDir, built.config.StateDir) {
		t.Fatalf("the Docker operation record lives at %q, outside the runtime state directory %q", stateDir, built.config.StateDir)
	}

	// And the per-request binding supplies the operation identity.
	candidate := t.TempDir()
	request := runtime.ExecutionRequest{
		RunID:        "run-fixture",
		OperationID:  "run-fixture:execution.invoke:execution.invoke#initial|1|abc",
		CandidateDir: candidate,
	}
	seen, err := captureBoundBroker(bound, request)
	if err != nil {
		t.Fatalf("the production provider refused a fully bound request: %v", err)
	}
	if seen.CandidateDir != candidate {
		t.Fatalf("broker candidate dir = %q, want %q", seen.CandidateDir, candidate)
	}
	if seen.Sandbox.OperationID != request.OperationID {
		t.Fatalf("broker operation id = %q, want the runtime operation %q", seen.Sandbox.OperationID, request.OperationID)
	}
	if seen.Sandbox.StateDir != stateDir {
		t.Fatalf("broker state dir = %q, want %q", seen.Sandbox.StateDir, stateDir)
	}
}

// TestBrokeredExecutionRefusesAnUnboundOperation proves the binding is required
// rather than defaulted. A brokered container with no owning operation has no
// durable record a crashed controller could reconcile against, and every
// alternative identity - a fixed global id, the process id, a random or
// model-supplied string - would let recovery target a container this operation
// does not own.
func TestBrokeredExecutionRefusesAnUnboundOperation(t *testing.T) {
	fixture := newCompositionFixture(t)
	built, err := newComposition(autonomyFlags{Config: fixture.configPath}, autonomyOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	defer built.release()
	bound := built.provider.(candidateBoundProvider)

	for name, request := range map[string]runtime.ExecutionRequest{
		"no operation identity":    {RunID: "r", CandidateDir: t.TempDir()},
		"no candidate workspace":   {RunID: "r", OperationID: "op"},
		"blank operation identity": {RunID: "r", CandidateDir: t.TempDir(), OperationID: "   "},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			if _, err := bound.Execute(context.Background(), request); err == nil {
				t.Fatal("an unbound brokered execution was accepted")
			}
		})
	}

	// A composition whose sandbox lost its state directory refuses too, rather
	// than discovering the problem one brokered call at a time.
	stripped := bound
	stripped.base.Broker.Sandbox.StateDir = ""
	if _, err := stripped.Execute(context.Background(), runtime.ExecutionRequest{
		RunID: "r", CandidateDir: t.TempDir(), OperationID: "op",
	}); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("a sandbox with no operation record location was accepted: %v", err)
	}
}

// TestBrokeredOperationRecordLivesOutsideTheCandidate: the record is host
// controller state. It identifies a container; it is not storage the model may
// see. The container boundary itself is asserted in the runtime package, where
// the mount arguments live.
func TestBrokeredOperationRecordLivesOutsideTheCandidate(t *testing.T) {
	fixture := newCompositionFixture(t)
	built, err := newComposition(autonomyFlags{Config: fixture.configPath}, autonomyOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	defer built.release()
	bound := built.provider.(candidateBoundProvider)

	candidate := t.TempDir()
	stateDir := bound.base.Broker.Sandbox.StateDir
	if strings.HasPrefix(stateDir, candidate) {
		t.Fatal("the Docker operation record lives inside the candidate workspace")
	}
	if !strings.Contains(stateDir, "docker-operations") {
		t.Fatalf("the operation record location is not the canonical one: %q", stateDir)
	}
}

// compositionFixture writes the smallest operator configuration newComposition
// accepts, so the test exercises the real loader rather than a hand-built one.
type compositionFixture struct{ configPath string }

func newCompositionFixture(t *testing.T) compositionFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(root, "openai.key")
	if err := os.WriteFile(credential, []byte("sk-fixture-not-a-real-credential\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	copyFixture := func(name, into string) string {
		body, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "v0.1", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, into)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	model := copyFixture("security-sensitive.project-model.json", "model.json")
	policy := copyFixture("security-sensitive.engineering-policy.json", "policy.json")
	config := map[string]any{
		"state_dir":          stateDir,
		"project_model_path": model,
		"policy_path":        policy,
		"provider": map[string]any{
			"kind": "openai", "model": "gpt-fixture", "auth_mode": "api",
			"credential_path": credential,
		},
		"assurance": map[string]any{
			"image":                "sha256:" + strings.Repeat("a", 64),
			"dependency_cache_dir": cache,
		},
		"github":  map[string]any{"credential_mode": "none"},
		"budgets": map[string]any{"wall_limit_seconds": 600, "max_execution_attempts": 3, "max_remediation_attempts": 2, "max_assurance_attempts": 2},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return compositionFixture{configPath: path}
}

// captureBoundBroker runs the per-request binding and returns the broker the
// provider would have used, without contacting a model or a daemon.
func captureBoundBroker(p candidateBoundProvider, request runtime.ExecutionRequest) (runtime.ToolBroker, error) {
	bound, err := p.bind(request)
	return bound.Broker, err
}
