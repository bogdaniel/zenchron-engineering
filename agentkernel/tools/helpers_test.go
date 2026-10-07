package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// fixture is a workspace plus a sibling directory outside it.
type fixture struct {
	ws      *Workspace
	root    string
	outside string
	store   *storage.MemoryArtifacts
	broker  *Broker
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: filepath.Join(base, "ws"), outside: filepath.Join(base, "outside")}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeFile(t, filepath.Join(f.root, name), content)
	}
	writeFile(t, filepath.Join(f.outside, "outside.txt"), "outside secret")
	if f.ws, err = NewWorkspace(f.root); err != nil {
		t.Fatal(err)
	}
	if f.store, err = storage.NewMemoryArtifacts(1 << 20); err != nil {
		t.Fatal(err)
	}
	macro, err := f.ws.ReadFiles()
	if err != nil {
		t.Fatal(err)
	}
	f.broker, err = NewBroker(f.ws.ReadFile(), f.ws.Search(), f.ws.WriteFile(), f.ws.ApplyPatch(), macro)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) env(mode api.Mode, grants ...api.Capability) Env {
	return Env{Grants: grants, Mode: mode, Artifacts: f.store, Producer: "exec/attempt/call", OutputLimit: 4096}
}

func grant(handle string, kind api.CapabilityKind, roots ...string) api.Capability {
	return api.Capability{Handle: handle, Kind: kind, Roots: roots}
}

func call(name string, args any) api.ToolCall {
	raw, ok := args.(string)
	if !ok {
		b, err := json.Marshal(args)
		if err != nil {
			panic(err)
		}
		raw = string(b)
	}
	return api.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(raw)}
}

// digests snapshots every regular file under dirs, so a test can assert that
// a refused call changed nothing anywhere.
func digests(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(p)
			out[p] = api.Digest(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func sameDigests(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("file set changed: %v -> %v", before, after)
	}
	for p, d := range before {
		if after[p] != d {
			t.Fatalf("%s changed", p)
		}
	}
}

// fakeTool returns a fixed result; it lets broker tests control output,
// errors and mutation without touching a filesystem.
type fakeTool struct {
	name    string
	kind    api.CapabilityKind
	result  api.ToolResult
	err     error
	invoked int
}

func (f *fakeTool) Spec() api.ToolSpec {
	return api.ToolSpec{Name: f.name, InputSchema: json.RawMessage(
		`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"}}}`)}
}
func (f *fakeTool) Kind() api.CapabilityKind { return f.kind }
func (f *fakeTool) kernelOwned()             {}
func (f *fakeTool) Scope(args json.RawMessage) (Scope, error) {
	return pathScope(args)
}
func (f *fakeTool) Invoke(context.Context, Invocation) (api.ToolResult, error) {
	f.invoked++
	return f.result, f.err
}

// failingStore refuses every Put, as a full or broken artifact store would.
type failingStore struct{}

func (failingStore) Put(context.Context, api.ArtifactInput) (api.ArtifactRef, error) {
	return api.ArtifactRef{}, errors.New("disk full")
}
func (failingStore) Get(context.Context, api.ArtifactRef) ([]byte, error) {
	return nil, storage.ErrNotFound
}

// fakeRunner records what it was asked to run.
type fakeRunner struct {
	requests []api.CommandRequest
	result   api.CommandResult
	err      error
}

func (r *fakeRunner) Run(_ context.Context, req api.CommandRequest) (api.CommandResult, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}
