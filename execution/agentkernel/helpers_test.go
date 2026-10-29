package agentkernel

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	testTree     = "89abcdef0123456789abcdef0123456789abcdef"
)

var testLimits = Limits{
	MaxWall: time.Minute, MaxIterations: 8, MaxToolCalls: 16,
	MaxInputTokens: 200_000, MaxOutputTokens: 8_000, MaxArtifactBytes: 1 << 20,
}

// transcript is an in-memory TranscriptWriter that can refuse one event kind.
type transcript struct {
	mu     sync.Mutex
	lines  []api.Event
	failOn api.EventKind
}

func (w *transcript) WriteEvent(_ context.Context, _ execution.AttemptRef, line []byte) error {
	var ev api.Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ev.Kind == w.failOn {
		return errors.New("transcript disk full")
	}
	w.lines = append(w.lines, ev)
	return nil
}

func (w *transcript) kinds() []api.EventKind {
	w.mu.Lock()
	defer w.mu.Unlock()
	var kinds []api.EventKind
	for _, ev := range w.lines {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

// runner is a CommandRunner that records calls and may block until its
// context ends.
type runner struct {
	mu      sync.Mutex
	calls   []Command
	block   bool
	entered chan struct{}
	exit    int
}

func (r *runner) Run(ctx context.Context, cmd Command) (CommandOutput, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()
	if r.entered != nil {
		close(r.entered)
	}
	if r.block {
		<-ctx.Done()
		return CommandOutput{}, ctx.Err()
	}
	return CommandOutput{ExitCode: r.exit, Stdout: []byte("ok")}, nil
}

func (r *runner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func testBinding() api.ProviderBinding {
	return api.ProviderBinding{
		ID: "kernel-scripted", Kind: "scripted", Model: "model-a", ModelVersion: api.UnknownVersion,
		ConfigFingerprint: "fp-1", ContextWindow: 200_000, MaxOutputTokens: 4_096,
	}
}

func testConfig(t *testing.T, provider api.Provider) Config {
	t.Helper()
	return Config{
		AgentID: "kernel-agent", ProviderKind: "agentkernel", AuthMode: "api_key", TrustMode: "brokered",
		Binding: testBinding(), Provider: provider, StateDir: realDir(t), Limits: testLimits,
		Transcript: &transcript{},
	}
}

func newTestAdapter(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// realDir is a temp dir with symlinks resolved, so it is a clean absolute
// path the kernel workspace accepts on every platform.
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func testRequest(t *testing.T) execution.Request {
	t.Helper()
	return execution.Request{
		RunID: "run-1", OperationID: "op-1", Attempt: 1,
		Candidate:    execution.Candidate{Branch: "zenchron/run-1", Revision: testRevision, Tree: testTree},
		CandidateDir: realDir(t),
		Objective:    "make the change",
	}
}

func done(text string) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{
		Text: text, Stop: api.StopEnd, Usage: api.TokenUsage{Input: api.Count(100), Output: api.Count(20)},
	}}
}

func toolUse(id, name, args string) scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{
		Stop: api.StopToolUse, ToolCalls: []api.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}},
	}}
}

func terminationOf(t *testing.T, err error) *TerminationError {
	t.Helper()
	var te *TerminationError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *TerminationError", err)
	}
	return te
}
