// Package conformance is the independent, black-box conformance suite of the
// Agent Execution Kernel: every test drives the module through its public
// packages only (api, engine, tools, storage, context sources, providers),
// never through unexported internals, against the #446 acceptance matrix.
package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

var epoch = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// far keeps the budget deadline beyond wall-clock time: the engine derives a
// real context deadline from it.
const far = 10 * 365 * 24 * time.Hour

// sink is a recording api.EventSink.
type sink struct {
	mu     sync.Mutex
	events []api.Event
}

func (s *sink) Record(_ context.Context, ev api.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *sink) count(kind api.EventKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// countingSource is an api.ContextSource that records how often it is asked.
type countingSource struct {
	mu    sync.Mutex
	calls int
	items []api.ContextItem
	err   error
}

func (s *countingSource) ContextItems(context.Context, api.ContextQuery) ([]api.ContextItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.items, s.err
}

func (s *countingSource) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func item(id string, kind api.ContextKind, trust api.Trust, required bool, content string) api.ContextItem {
	return api.ContextItem{ID: id, Kind: kind, Trust: trust, Required: required, Content: content,
		ContentDigest: api.Digest([]byte(content))}
}

func binding(id string) api.ProviderBinding {
	return api.ProviderBinding{
		ID: id, Kind: "scripted", Model: "m", ModelVersion: api.UnknownVersion, ConfigFingerprint: "cfg-" + id,
		Eligible: true, Isolation: api.IsolationUnproven, ContextWindow: 50000, MaxOutputTokens: 1000,
		Features: []string{api.FeatureTools},
	}
}

// request is a valid read-write request over the whole workspace.
func request(id string) api.ExecutionRequest {
	return api.ExecutionRequest{
		Version: api.ExecutionVersion, ExecutionID: id, AttemptID: "att-1", Objective: "inspect notes.txt",
		Mode:        api.ModeReadWrite,
		Workspace:   api.WorkspaceRef{ID: "ws-1", ManifestDigest: api.Digest([]byte("manifest"))},
		Constraints: api.Constraints{InstructionDigest: api.Digest([]byte("instructions"))},
		Context:     []api.ContextItem{item("host-rules", api.ContextInstruction, api.TrustHost, true, "Answer briefly.")},
		Grants: []api.Capability{
			{Handle: "read-all", Kind: api.CapabilityFileRead, Roots: []string{"."}},
			{Handle: "write-out", Kind: api.CapabilityFileWrite, Roots: []string{"out"}},
		},
		Budget: api.Budget{
			Deadline: epoch.Add(far), MaxIterations: 6, MaxToolCalls: 6, MaxInputTokens: 200000,
			MaxOutputTokens: 4000, MaxArtifactBytes: 1 << 20, MaxProviderRetries: 2,
		},
		Providers: []api.ProviderBinding{binding("p1")},
	}
}

type kernel struct {
	engine    *engine.Engine
	sink      *sink
	dir       string
	artifacts storage.Artifacts
}

type config struct {
	providers  map[string]api.Provider
	sources    []api.ContextSource
	artifacts  storage.Artifacts
	extra      []tools.Tool
	admissions *storage.FileRecords
}

// newKernel builds an engine over the real broker and file tools in a fresh
// workspace containing notes.txt and an out/ directory. The test is the
// host: its workers serve the providers, sources and sink until it ends.
func newKernel(t *testing.T, c config) *kernel {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.txt"), "alpha beta gamma\n")
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(append([]tools.Tool{ws.ReadFile(), ws.Search(), ws.WriteFile(), ws.ApplyPatch()}, c.extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	arts := c.artifacts
	if arts == nil {
		if arts, err = storage.NewMemoryArtifacts(1 << 22); err != nil {
			t.Fatal(err)
		}
	}
	k := &kernel{sink: &sink{}, dir: dir, artifacts: arts}
	served := map[string]chan<- api.ProviderCall{}
	for id, p := range c.providers {
		served[id] = api.ServeProvider(t.Context(), p)
	}
	var sources []chan<- api.ContextRequest
	for _, src := range c.sources {
		sources = append(sources, api.ServeContext(t.Context(), src))
	}
	k.engine, err = engine.New(engine.Config{
		Providers: served, Broker: broker, Artifacts: arts, Events: api.ServeEvents(t.Context(), k.sink),
		Clock: api.NewManualClock(epoch), Sources: sources, OutputLimit: 256, Admissions: c.admissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *kernel) run(t *testing.T, ctx context.Context, req api.ExecutionRequest) api.ExecutionResult {
	t.Helper()
	res, err := k.engine.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute returned error %v; an evaluable request must settle", err)
	}
	if n := k.sink.count(api.EventSettled); n > 1 {
		t.Fatalf("settled %d times", n)
	}
	return res
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeDigest hashes every path, mode and content under dir, so any mutation
// (create, delete, rename, write) changes it.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := p + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func want(t *testing.T, res api.ExecutionResult, outcome api.Outcome, cause api.Cause) {
	t.Helper()
	if res.Termination.Outcome != outcome || res.Termination.Cause != cause {
		t.Fatalf("termination %s/%s (%s), want %s/%s", res.Termination.Outcome, res.Termination.Cause,
			res.Termination.Detail, outcome, cause)
	}
}

func usage(in, out int64) api.TokenUsage {
	return api.TokenUsage{Input: api.Count(in), Output: api.Count(out), CachedInput: api.Count(0), CacheWriteInput: api.Count(0)}
}

func end(text string) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{Text: text, Stop: api.StopEnd, Usage: usage(100, 10)}}
}

func toolUse(calls ...api.ToolCall) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{ToolCalls: calls, Stop: api.StopToolUse, Usage: usage(100, 10)}}
}

func call(id, name string, args map[string]any) api.ToolCall {
	data, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return api.ToolCall{ID: id, Name: name, Arguments: data}
}

// toolResults returns the tool messages the provider was sent, by call id.
func toolResults(t *testing.T, reqs []api.ProviderRequest) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range reqs {
		for _, m := range r.Messages {
			if m.Role != api.RoleTool {
				continue
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(m.Content), &v); err != nil {
				t.Fatalf("tool message %q is not JSON: %v", m.Content, err)
			}
			out[m.ToolCallID] = v
		}
	}
	return out
}

func providers(ps ...api.Provider) map[string]api.Provider {
	m := map[string]api.Provider{}
	for i, p := range ps {
		m["p"+string(rune('1'+i))] = p
	}
	return m
}
