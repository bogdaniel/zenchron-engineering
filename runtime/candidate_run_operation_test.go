package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrokeredCommandRequiresRuntimeOwnedOperationIdentity pins the reason the
// identity exists at all. A brokered candidate command is a runtime-owned side
// effect, so a crashed controller must retain the EXACT container name it alone
// may reconcile. Without an operation id and a state directory there is no
// durable record, and recovery would have to guess by prefix or label.
func TestBrokeredCommandRequiresRuntimeOwnedOperationIdentity(t *testing.T) {
	for name, sandbox := range map[string]DockerSandbox{
		"no identity":  {Image: "img", StateDir: t.TempDir()},
		"no state dir": {Image: "img", OperationID: "op"},
		"neither":      {Image: "img"},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			if _, err := sandbox.operationRecordPath(); err == nil ||
				!strings.Contains(err.Error(), "runtime-owned Docker operation identity and state directory are required") {
				t.Fatalf("a container without a durable record was permitted: %v", err)
			}
		})
	}
	// Both present is the only accepted shape.
	complete := DockerSandbox{Image: "img", OperationID: "op", StateDir: t.TempDir()}
	if _, err := complete.operationRecordPath(); err != nil {
		t.Fatalf("a fully bound sandbox was refused: %v", err)
	}
}

// TestOperationIdentityNamesExactlyOneRecord proves the binding is exact rather
// than a search key: the same operation always resolves to the same durable
// record, and a different operation never collides with it. Recovery therefore
// targets one container it owns, never a prefix or label match.
func TestOperationIdentityNamesExactlyOneRecord(t *testing.T) {
	stateDir := t.TempDir()
	base := DockerSandbox{Image: "sha256:img", StateDir: stateDir}
	first, second := base, base
	first.OperationID = "run-a:execution.invoke:execution.invoke#initial|1|abc"
	second.OperationID = "run-b:execution.invoke:execution.invoke#initial|1|abc"

	firstPath, err := first.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	againPath, err := first.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	if firstPath != againPath {
		t.Fatal("the same operation resolved to two different records")
	}
	secondPath, err := second.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	if secondPath == firstPath {
		t.Fatal("two different execution operations share one durable record")
	}
	// The record lives under the runtime state directory the composition owns,
	// and its name is derived rather than taken from caller text.
	for _, path := range []string{firstPath, secondPath} {
		if !strings.HasPrefix(path, stateDir) {
			t.Fatalf("record %q is outside the runtime state directory %q", path, stateDir)
		}
		if strings.Contains(filepath.Base(path), "run-a") || strings.Contains(filepath.Base(path), "execution.invoke") {
			t.Fatalf("the record name embeds caller text: %q", path)
		}
	}
}

// TestSequentialBrokeredCommandsReuseOneOperationSafely is why one identity per
// execution.invoke is enough. The provider's tool loop is strictly sequential
// and each container is created, waited on and removed before the next call, so
// reusing the operation is exact rather than merely unique - and the record it
// writes is owner-only host state.
func TestSequentialBrokeredCommandsReuseOneOperationSafely(t *testing.T) {
	sandbox := DockerSandbox{Image: "sha256:img", StateDir: t.TempDir(), OperationID: "run-a:execution.invoke:1"}
	path, err := sandbox.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerOperation(path, dockerOperationRecord{
		OperationID: sandbox.OperationID, ContainerName: "zenchron-fixture", Phase: "removed",
	}); err != nil {
		t.Fatal(err)
	}
	again, err := sandbox.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Fatal("a sequential reuse of the same operation resolved elsewhere")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("operation record mode = %#o, want owner-only", info.Mode().Perm())
	}
}

// TestBrokeredContainerBoundaryIsUnchanged: restoring candidate.run must not
// widen anything. The candidate workspace is the only host path mounted, and
// the runtime state directory that holds the operation record is not among the
// mounts.
func TestBrokeredContainerBoundaryIsUnchanged(t *testing.T) {
	candidate := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "artifacts", "docker-operations")
	args := append(dockerBase(candidate, false), "--workdir", "/candidate", "sha256:img", "go", "test", "./...")
	rendered := strings.Join(args, " ")

	for _, want := range []string{
		"--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--pids-limit 256",
		"dst=/candidate", "/candidate/.git",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the brokered boundary no longer states %q: %s", want, rendered)
		}
	}
	for _, forbidden := range []string{stateDir, "docker.sock", "OPENAI", "GITHUB_TOKEN", "runtime.db", "/locks"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("the brokered container can reach %q: %s", forbidden, rendered)
		}
	}
	// Exactly one host workspace is mounted.
	if mounts := strings.Count(rendered, "type=bind,src="); mounts != 1 {
		t.Fatalf("the brokered container mounts %d host paths, want exactly the candidate: %s", mounts, rendered)
	}
}

// TestExecutionRequestCarriesTheAuthorizingOperation proves the field is the
// runtime's own operation identity rather than something the provider invents.
func TestExecutionRequestCarriesTheAuthorizingOperation(t *testing.T) {
	fixture := newPhase8Fixture(t)
	recorder := &recordingExecutionProvider{inner: fixture.deps.Provider}
	fixture.deps.Provider = recorder
	fixture.runtime = fixture.newRuntime(fixture.deps)

	runID := fixture.start()
	fixture.reconcile(runID)

	if len(recorder.requests) == 0 {
		t.Fatal("the provider was never invoked")
	}
	for _, request := range recorder.requests {
		if strings.TrimSpace(request.OperationID) == "" {
			t.Fatal("an execution request carried no authorizing operation")
		}
		if !strings.HasPrefix(request.OperationID, runID+":execution.invoke:") {
			t.Fatalf("operation id %q is not this run's execution operation", request.OperationID)
		}
	}
}

type recordingExecutionProvider struct {
	inner    ExecutionProvider
	requests []ExecutionRequest
}

// Isolation is forwarded when the wrapped provider reports one, so wrapping
// cannot quietly make an isolated provider look unprotected.
func (p *recordingExecutionProvider) Isolation() ProviderIsolation {
	if isolated, ok := p.inner.(interface{ Isolation() ProviderIsolation }); ok {
		return isolated.Isolation()
	}
	return ProviderIsolation{}
}

func (p *recordingExecutionProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	return p.inner.Execute(ctx, request)
}
