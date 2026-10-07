package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// stuckSink records normally until its blockAt-th (1-based) Record call,
// which blocks ignoring its context until the test ends.
type stuckSink struct {
	mu      sync.Mutex
	calls   int
	blockAt int
	stuck   chan struct{} // closed when the blocking call starts
	release chan struct{}
}

func newStuckSink(t *testing.T, blockAt int) *stuckSink {
	s := &stuckSink{blockAt: blockAt, stuck: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *stuckSink) Record(context.Context, api.Event) error {
	s.mu.Lock()
	s.calls++
	block := s.calls == s.blockAt
	s.mu.Unlock()
	if block {
		close(s.stuck)
		<-s.release
	}
	return nil
}

const grace = 100 * time.Millisecond

// TestStuckEventSinkCannotHoldExecutionOpen is review item 6: a sink that
// never returns, at an in-loop event or at the terminal settled event, cannot
// keep Execute running past the deadline (or a host cancellation) plus the
// settlement grace; the result is incomplete/recording_failed.
func TestStuckEventSinkCannotHoldExecutionOpen(t *testing.T) {
	cases := map[string]struct {
		blockAt  int // started, routing, context, requested(4), responded, settled(6)
		deadline time.Duration
		cancel   bool
		limit    time.Duration
		observed string
	}{
		"mid_loop_until_deadline": {blockAt: 4, deadline: 300 * time.Millisecond, limit: 300*time.Millisecond + grace},
		"mid_loop_host_cancel":    {blockAt: 4, deadline: time.Hour, cancel: true, limit: grace},
		"terminal_settled":        {blockAt: 6, deadline: time.Hour, limit: grace, observed: "completed/loop_completed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStuckSink(t, c.blockAt)
			f := newFixture(t, []scripted.Step{end("done")}, func(cfg *engine.Config) {
				cfg.Events, cfg.SettleTimeout = s, grace
			})
			now := time.Now()
			f.clock.now = now
			req := request()
			req.Budget.Deadline = now.Add(c.deadline)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan api.ExecutionResult, 1)
			go func() {
				res, _ := f.engine.Execute(ctx, req)
				done <- res
			}()
			select {
			case <-s.stuck:
			case <-time.After(5 * time.Second):
				t.Fatal("the sink never reached the blocking event")
			}
			if c.cancel {
				cancel()
			}
			slack := 2 * time.Second
			var res api.ExecutionResult
			select {
			case res = <-done:
			case <-time.After(c.limit + slack):
				t.Fatalf("Execute still running %s after the bound; a stuck sink held it open", c.limit+slack)
			}
			want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
			if !strings.Contains(res.Termination.Detail, "did not return within") ||
				!strings.Contains(res.Termination.Detail, c.observed) {
				t.Fatalf("detail %q", res.Termination.Detail)
			}
			if c.blockAt == 4 && len(f.provider.Requests()) != 0 {
				t.Fatal("provider called after the recording failed")
			}
		})
	}
}

// stuckSource never returns, ignoring its context, until the test ends.
type stuckSource struct{ release chan struct{} }

func (s stuckSource) ContextItems(context.Context, api.ContextQuery) ([]api.ContextItem, error) {
	<-s.release
	return nil, nil
}

// TestStuckContextSourceCannotHoldExecutionOpen: a source that never returns
// is abandoned at the budget deadline; the execution settles exhausted on
// the deadline without calling the provider.
func TestStuckContextSourceCannotHoldExecutionOpen(t *testing.T) {
	src := stuckSource{release: make(chan struct{})}
	t.Cleanup(func() { close(src.release) })
	f := newFixture(t, []scripted.Step{end("never")}, func(cfg *engine.Config) {
		cfg.Sources, cfg.SettleTimeout = []api.ContextSource{src}, grace
	})
	now := time.Now()
	f.clock.now = now
	req := request()
	req.Budget.Deadline = now.Add(300 * time.Millisecond)
	done := make(chan api.ExecutionResult, 1)
	go func() {
		res, _ := f.engine.Execute(context.Background(), req)
		done <- res
	}()
	select {
	case res := <-done:
		want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
		if res.Termination.Dimension != api.DimensionDeadline || len(f.provider.Requests()) != 0 {
			t.Fatalf("termination %+v after %d calls", res.Termination, len(f.provider.Requests()))
		}
	case <-time.After(300*time.Millisecond + grace + 2*time.Second):
		t.Fatal("Execute still running after deadline + grace; a stuck source held it open")
	}
}

// stuckRecords is an admission store whose blockAt-th (1-based) Get or Put
// blocks ignoring its context until the test ends.
type stuckRecords struct {
	*storage.MemoryRecords
	mu      sync.Mutex
	calls   int
	blockAt int
	release chan struct{}
}

func (s *stuckRecords) wait() {
	s.mu.Lock()
	s.calls++
	block := s.calls == s.blockAt
	s.mu.Unlock()
	if block {
		<-s.release
	}
}

func (s *stuckRecords) Get(ctx context.Context, partition, key string) ([]byte, error) {
	s.wait()
	return s.MemoryRecords.Get(ctx, partition, key)
}

func (s *stuckRecords) Put(ctx context.Context, partition, key string, value []byte) error {
	s.wait()
	return s.MemoryRecords.Put(ctx, partition, key, value)
}

// TestStuckAdmissionStoreCannotHoldExecutionOpen: an admission store that
// never returns is abandoned like a stuck sink. Before any side effect the
// attempt is refused (nothing ran); at settlement the result is
// incomplete/recording_failed and the attempt stays unsettled.
func TestStuckAdmissionStoreCannotHoldExecutionOpen(t *testing.T) {
	cases := map[string]struct {
		blockAt  int // admission Get(1), Put(2); settlement Get(3), Put(4)
		deadline time.Duration
		limit    time.Duration
		detail   string
		calls    int
	}{
		"admission_read":  {blockAt: 1, deadline: 300 * time.Millisecond, limit: 300*time.Millisecond + grace, detail: "nothing ran"},
		"admission_write": {blockAt: 2, deadline: 300 * time.Millisecond, limit: 300*time.Millisecond + grace, detail: "nothing ran"},
		"settlement":      {blockAt: 3, deadline: time.Hour, limit: grace, detail: "completed/loop_completed", calls: 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := &stuckRecords{MemoryRecords: storage.NewMemoryRecords(), blockAt: c.blockAt, release: make(chan struct{})}
			t.Cleanup(func() { close(store.release) })
			f := newFixture(t, []scripted.Step{end("done"), end("never")}, func(cfg *engine.Config) {
				cfg.Admissions, cfg.SettleTimeout = store, grace
			})
			now := time.Now()
			f.clock.now = now
			req := request()
			req.Budget.Deadline = now.Add(c.deadline)
			done := make(chan api.ExecutionResult, 1)
			go func() {
				res, _ := f.engine.Execute(context.Background(), req)
				done <- res
			}()
			var res api.ExecutionResult
			select {
			case res = <-done:
			case <-time.After(c.limit + 2*time.Second):
				t.Fatalf("Execute still running %s after the bound; a stuck admission store held it open", c.limit+2*time.Second)
			}
			want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
			if !strings.Contains(res.Termination.Detail, "did not return within") ||
				!strings.Contains(res.Termination.Detail, c.detail) || len(f.provider.Requests()) != c.calls {
				t.Fatalf("detail %q after %d provider calls", res.Termination.Detail, len(f.provider.Requests()))
			}
			if name == "settlement" {
				next := request()
				next.AttemptID, next.Budget.Deadline = "att-2", req.Budget.Deadline
				res, _ := f.engine.Execute(context.Background(), next)
				wantRefused(t, res, "unsettled")
			}
		})
	}
}
