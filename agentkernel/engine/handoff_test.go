package engine_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

const grace = 100 * time.Millisecond

// host is a test-owned worker for one hand-off port. It answers calls with
// handle until its stopAt-th call (1-based), which it either never takes or
// takes and holds; answerLate then lets it answer that call late and exit.
type host[Q, R any] struct {
	calls   chan api.Call[Q, R]
	stuck   chan struct{} // closed when the host stops
	release chan struct{}
	done    chan struct{} // closed when the host goroutine has exited
	once    sync.Once
}

func newHost[Q, R any](t *testing.T, handle func(Q) R, stopAt int, take bool) *host[Q, R] {
	h := &host[Q, R]{calls: make(chan api.Call[Q, R]), stuck: make(chan struct{}),
		release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(h.answerLate)
	go h.serve(handle, stopAt, take)
	return h
}

func (h *host[Q, R]) answerLate() { h.once.Do(func() { close(h.release) }) }

func (h *host[Q, R]) serve(handle func(Q) R, stopAt int, take bool) {
	defer close(h.done)
	for n := 1; ; n++ {
		if n == stopAt && !take {
			close(h.stuck)
			<-h.release
			return
		}
		var c api.Call[Q, R]
		select {
		case c = <-h.calls:
		case <-h.release:
			return
		}
		if n == stopAt {
			close(h.stuck)
			<-h.release
		}
		c.Reply <- handle(c.Request) // capacity 1: never blocks, even when late
		if n == stopAt {
			return
		}
	}
}

// waitForGoroutines fails unless the goroutine count returns to base, the
// count taken after the test's own host goroutines started: a kernel
// goroutine outliving Execute would keep it above.
func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines outlived Execute beyond the %d running before it: the kernel leaked one",
				runtime.NumGoroutine()-base, base)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// bounded runs Execute and fails unless it returns within limit (plus
// scheduling slack) after the host got stuck, and leaves no goroutine.
type bounded struct {
	deadline time.Duration
	cancel   bool
	limit    time.Duration
	adjust   func(*api.ExecutionRequest) // optional request changes
}

func (b bounded) execute(t *testing.T, f *fixture, stuck <-chan struct{}) api.ExecutionResult {
	t.Helper()
	now := time.Now()
	f.clock.Set(now)
	req := request()
	req.Budget.Deadline = now.Add(b.deadline)
	if b.adjust != nil {
		b.adjust(&req)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runtime.NumGoroutine()
	done := make(chan api.ExecutionResult, 1)
	go func() {
		res, _ := f.engine.Execute(ctx, req)
		done <- res
	}()
	select {
	case <-stuck:
	case <-time.After(5 * time.Second):
		t.Fatal("the host never reached the call it gets stuck on")
	}
	if b.cancel {
		cancel()
	}
	var res api.ExecutionResult
	select {
	case res = <-done:
	case <-time.After(b.limit + 2*time.Second):
		t.Fatalf("Execute still running %s after the host got stuck; the host held it open", b.limit+2*time.Second)
	}
	waitForGoroutines(t, base)
	return res
}

func eventHost(t *testing.T, stopAt int, take bool) *host[api.Event, error] {
	return newHost(t, func(api.Event) error { return nil }, stopAt, take)
}

var shortDeadline = bounded{deadline: 300 * time.Millisecond, limit: 300*time.Millisecond + grace}

// TestStuckEventSinkCannotHoldExecutionOpen is review item 6: a sink worker
// that never takes an event, or takes it and never acknowledges it, at an
// in-loop event or at the terminal settled event, cannot keep Execute running
// past the deadline (or a host cancellation) plus the settlement grace, and
// leaves no kernel goroutine behind; the result is incomplete/recording_failed.
func TestStuckEventSinkCannotHoldExecutionOpen(t *testing.T) {
	afterSettle := bounded{deadline: time.Hour, limit: grace}
	cases := map[string]struct {
		stopAt   int // started, routing, context, requested(4), responded, settled(6)
		take     bool
		bound    bounded
		detail   string
		observed string
	}{
		"mid_loop_never_taken":     {stopAt: 4, bound: shortDeadline, detail: "did not take"},
		"mid_loop_never_acked":     {stopAt: 4, take: true, bound: shortDeadline, detail: "did not answer"},
		"mid_loop_host_cancel":     {stopAt: 4, take: true, bound: bounded{deadline: time.Hour, cancel: true, limit: grace}, detail: "did not answer"},
		"terminal_never_taken":     {stopAt: 6, bound: afterSettle, detail: "did not take", observed: "completed/loop_completed"},
		"terminal_settled_unacked": {stopAt: 6, take: true, bound: afterSettle, detail: "did not answer", observed: "completed/loop_completed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := eventHost(t, c.stopAt, c.take)
			f := newFixture(t, []scripted.Step{end("done")}, func(cfg *engine.Config) {
				cfg.Events, cfg.SettleTimeout = h.calls, grace
			})
			res := c.bound.execute(t, f, h.stuck)
			want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
			if !strings.Contains(res.Termination.Detail, c.detail) || !strings.Contains(res.Termination.Detail, c.observed) {
				t.Fatalf("detail %q", res.Termination.Detail)
			}
			if c.stopAt == 4 && len(f.provider.Requests()) != 0 {
				t.Fatal("provider called after the recording failed")
			}
		})
	}
}

// TestStuckContextSourceCannotHoldExecutionOpen: a source worker that never
// takes the query, or never answers it, is given up at the budget deadline;
// the execution settles exhausted on the deadline without calling the
// provider and leaves no kernel goroutine behind.
func TestStuckContextSourceCannotHoldExecutionOpen(t *testing.T) {
	for name, take := range map[string]bool{"never_taken": false, "never_answered": true} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t, func(api.ContextQuery) api.Reply[[]api.ContextItem] { return api.Reply[[]api.ContextItem]{} }, 1, take)
			f := newFixture(t, []scripted.Step{end("never")}, func(cfg *engine.Config) {
				cfg.Sources, cfg.SettleTimeout = []chan<- api.ContextRequest{h.calls}, grace
			})
			res := shortDeadline.execute(t, f, h.stuck)
			want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
			if res.Termination.Dimension != api.DimensionDeadline || len(f.provider.Requests()) != 0 {
				t.Fatalf("termination %+v after %d calls", res.Termination, len(f.provider.Requests()))
			}
		})
	}
}

// TestStuckProviderCannotHoldExecutionOpen: a provider worker that never
// takes the call settles on the deadline with no call counted; one that
// takes it and never answers is counted with its usage unknown; a host
// cancellation ends the wait after the grace. A late answer is never used.
func TestStuckProviderCannotHoldExecutionOpen(t *testing.T) {
	cases := map[string]struct {
		take    bool
		bound   bounded
		outcome api.Outcome
		cause   api.Cause
		calls   int
	}{
		"never_taken":    {bound: shortDeadline, outcome: api.OutcomeExhausted, cause: api.CauseBudgetExhausted},
		"never_answered": {take: true, bound: shortDeadline, outcome: api.OutcomeExhausted, cause: api.CauseBudgetExhausted, calls: 1},
		"host_cancel":    {take: true, bound: bounded{deadline: time.Hour, cancel: true, limit: grace}, outcome: api.OutcomeCancelled, cause: api.CauseHostCancelled, calls: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t, func(api.ProviderRequest) api.Reply[api.ProviderResponse] {
				return api.Reply[api.ProviderResponse]{Value: api.ProviderResponse{Text: "late", Stop: api.StopEnd}}
			}, 1, c.take)
			f := newFixture(t, nil, func(cfg *engine.Config) {
				cfg.Providers = map[string]chan<- api.ProviderCall{"p1": h.calls}
				cfg.SettleTimeout = grace
			})
			res := c.bound.execute(t, f, h.stuck)
			want(t, res, c.outcome, c.cause)
			if res.Usage.ProviderCalls != c.calls || res.FinalText != "" {
				t.Fatalf("usage %+v, final %q", res.Usage, res.FinalText)
			}
		})
	}
}

// TestLateHostRepliesAreIgnored: a reply arriving after the kernel stopped
// waiting goes into the call's own buffered channel, so the worker is not
// blocked and moves on; nothing of the kernel's is waiting to receive it.
func TestLateHostRepliesAreIgnored(t *testing.T) {
	h := eventHost(t, 4, true)
	f := newFixture(t, []scripted.Step{end("done")}, func(cfg *engine.Config) {
		cfg.Events, cfg.SettleTimeout = h.calls, grace
	})
	res := shortDeadline.execute(t, f, h.stuck)
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	h.answerLate()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the late acknowledgement blocked the host worker")
	}
}

// TestStuckHostToolIsAnUncertainMutation: a host write tool whose worker takes
// the call and never answers stops the execution at the deadline as an
// uncertain side effect, observed as such, with no kernel goroutine left.
func TestStuckHostToolIsAnUncertainMutation(t *testing.T) {
	h := newHost(t, func(tools.HostInvocation) api.Reply[api.ToolResult] { return api.Reply[api.ToolResult]{} }, 1, true)
	write, err := tools.NewHostTool(tools.HostTool{
		Spec: api.ToolSpec{Name: "host_write", Description: "test", InputSchema: json.RawMessage(
			`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"}}}`)},
		Kind: api.CapabilityFileWrite, PathArgument: "path", Calls: h.calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(write)
	if err != nil {
		t.Fatal(err)
	}
	call := api.ToolCall{ID: "w1", Name: "host_write", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	f := newFixture(t, []scripted.Step{toolUse(call), end("never")}, func(cfg *engine.Config) {
		cfg.Broker, cfg.SettleTimeout = broker, grace
	})
	b := shortDeadline
	b.adjust = func(r *api.ExecutionRequest) {
		r.Mode = api.ModeReadWrite
		r.Grants = []api.Capability{{Handle: "w", Kind: api.CapabilityFileWrite, Roots: []string{"."}}}
	}
	res := b.execute(t, f, h.stuck)
	want(t, res, api.OutcomeFailed, api.CauseToolFailed)
	last := res.Observations[len(res.Observations)-2] // the one before settlement
	if last.Kind != api.EventToolExecuted || !strings.Contains(last.Detail, "uncertain") {
		t.Fatalf("observations %+v: the unanswered write was not observed as uncertain", res.Observations)
	}
}

// TestHandOffIDsAreStableAndUnique: every call handed to a host carries an
// ID naming its execution, attempt and position, unique within the attempt,
// so a host can deduplicate delayed or repeated processing.
func TestHandOffIDsAreStableAndUnique(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	record := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, id)
	}
	events := make(chan api.EventDelivery)
	sources := make(chan api.ContextRequest)
	go func() {
		for {
			select {
			case c := <-events:
				record(c.ID)
				c.Reply <- nil
			case c := <-sources:
				record(c.ID)
				c.Reply <- api.Reply[[]api.ContextItem]{}
			case <-t.Context().Done():
				return
			}
		}
	}()
	f := newFixture(t, []scripted.Step{toolUse(readCall("a", "a.txt")), end("done")}, func(cfg *engine.Config) {
		cfg.Events, cfg.Sources = events, []chan<- api.ContextRequest{sources}
	})
	res, _ := f.engine.Execute(context.Background(), request())
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || !strings.HasPrefix(id, "exec-1/att-1/") {
			t.Fatalf("ID %q repeated or unbound in %v", id, ids)
		}
		seen[id] = true
	}
	if !seen["exec-1/att-1/event-1"] || !seen["exec-1/att-1/source-0"] || int64(len(ids)) != res.EventCount+1 {
		t.Fatalf("ids %v for %d events", ids, res.EventCount)
	}
}
