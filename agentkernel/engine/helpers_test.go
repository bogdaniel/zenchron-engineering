package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// budgetSpan keeps the budget deadline far beyond wall-clock time, because
// the engine also derives a real context deadline from it; deadline tests
// move the fake clock past it instead.
const budgetSpan = 10 * 365 * 24 * time.Hour

// clock is a controllable api.Clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sink records events; failAt > 0 fails that (1-based) Record call and all later ones.
type sink struct {
	mu     sync.Mutex
	events []api.Event
	calls  int
	failAt int
}

func (s *sink) Record(_ context.Context, ev api.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAt > 0 && s.calls >= s.failAt {
		return errors.New("sink unavailable")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *sink) kinds() []api.EventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.EventKind
	for _, e := range s.events {
		out = append(out, e.Kind)
	}
	return out
}

func item(id string, kind api.ContextKind, trust api.Trust, required bool, content string) api.ContextItem {
	return api.ContextItem{ID: id, Kind: kind, Trust: trust, Required: required, Content: content, ContentDigest: api.Digest([]byte(content))}
}

func request() api.ExecutionRequest {
	return api.ExecutionRequest{
		Version: api.ExecutionVersion, ExecutionID: "exec-1", AttemptID: "att-1", Objective: "summarize a.txt",
		Mode:        api.ModeReadOnly,
		Workspace:   api.WorkspaceRef{ID: "ws-1", ManifestDigest: api.Digest([]byte("manifest"))},
		Constraints: api.Constraints{InstructionDigest: api.Digest([]byte("instructions"))},
		Context:     []api.ContextItem{item("host-rules", api.ContextInstruction, api.TrustHost, true, "Answer in one line.")},
		Grants:      []api.Capability{{Handle: "read-all", Kind: api.CapabilityFileRead, Roots: []string{"."}}},
		Budget: api.Budget{
			Deadline: epoch.Add(budgetSpan), MaxIterations: 5, MaxToolCalls: 5, MaxInputTokens: 100000,
			MaxOutputTokens: 4000, MaxArtifactBytes: 1 << 20, MaxProviderRetries: 2,
		},
		Providers: []api.ProviderBinding{{
			ID: "p1", Kind: "scripted", Model: "m", ModelVersion: api.UnknownVersion, ConfigFingerprint: "sha256:cfg",
			Eligible: true, Isolation: api.IsolationUnproven, ContextWindow: 50000, MaxOutputTokens: 1000,
		}},
	}
}

type fixture struct {
	clock     *clock
	sink      *sink
	artifacts *storage.MemoryArtifacts
	provider  *scripted.Provider
	engine    *engine.Engine
	dir       string
}

type option func(*engine.Config)

// newFixture builds an engine over the real broker, workspace tools and
// in-memory artifact store, with the scripted provider bound to "p1".
func newFixture(t *testing.T, steps []scripted.Step, opts ...option) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha beta gamma delta epsilon"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(ws.ReadFile(), ws.Search(), ws.WriteFile())
	if err != nil {
		t.Fatal(err)
	}
	arts, err := storage.NewMemoryArtifacts(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{clock: &clock{now: epoch}, sink: &sink{}, artifacts: arts, provider: scripted.New(steps...), dir: dir}
	cfg := engine.Config{
		Providers: map[string]api.Provider{"p1": f.provider}, Broker: broker, Artifacts: arts,
		Events: f.sink, Clock: f.clock, OutputLimit: 4096,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if f.engine, err = engine.New(cfg); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) run(t *testing.T, ctx context.Context, req api.ExecutionRequest) api.ExecutionResult {
	t.Helper()
	res, err := f.engine.Execute(ctx, req)
	if err != nil {
		t.Fatalf("Execute returned error %v; every evaluable request must settle", err)
	}
	f.checkEvents(t, res)
	return res
}

// checkEvents asserts the event invariants every execution must keep:
// monotonic sequence from 1, binding, and observations mirroring the sink.
func (f *fixture) checkEvents(t *testing.T, res api.ExecutionResult) {
	t.Helper()
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	if int64(len(f.sink.events)) != res.EventCount || len(res.Observations) != len(f.sink.events) {
		t.Fatalf("event count %d, observations %d, sink %d", res.EventCount, len(res.Observations), len(f.sink.events))
	}
	for i, ev := range f.sink.events {
		if ev.Seq != int64(i+1) || ev.ExecutionID != res.ExecutionID || ev.AttemptID != res.AttemptID || ev.Version != api.ExecutionVersion {
			t.Fatalf("event %d malformed: %+v", i, ev)
		}
		if res.Observations[i].Seq != ev.Seq || res.Observations[i].Kind != ev.Kind {
			t.Fatalf("observation %d does not mirror event", i)
		}
	}
}

func want(t *testing.T, res api.ExecutionResult, outcome api.Outcome, cause api.Cause) {
	t.Helper()
	if res.Termination.Outcome != outcome || res.Termination.Cause != cause {
		t.Fatalf("termination %s/%s (%s), want %s/%s", res.Termination.Outcome, res.Termination.Cause,
			res.Termination.Detail, outcome, cause)
	}
}

func end(text string) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{Text: text, Stop: api.StopEnd,
		Usage: api.TokenUsage{Input: api.Count(100), Output: api.Count(10), CachedInput: api.Count(0)}}}
}

func toolUse(calls ...api.ToolCall) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{ToolCalls: calls, Stop: api.StopToolUse,
		Usage: api.TokenUsage{Input: api.Count(100), Output: api.Count(10), CachedInput: api.Count(0)}}}
}

func readCall(id, path string) api.ToolCall {
	args, _ := json.Marshal(map[string]string{"path": path})
	return api.ToolCall{ID: id, Name: "read_file", Arguments: args}
}
