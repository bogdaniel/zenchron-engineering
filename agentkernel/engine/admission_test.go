package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

func attempt(id string) api.ExecutionRequest {
	req := request()
	req.AttemptID = id
	return req
}

// next runs another execution on the same engine; the sink is cleared first
// so each result's events are checked on their own.
func (f *fixture) next(t *testing.T, req api.ExecutionRequest) api.ExecutionResult {
	t.Helper()
	f.sink.mu.Lock()
	f.sink.events, f.sink.calls = nil, 0
	f.sink.mu.Unlock()
	return f.run(t, context.Background(), req)
}

func wantRefused(t *testing.T, res api.ExecutionResult, reason string) {
	t.Helper()
	want(t, res, api.OutcomeBlocked, api.CauseInvalidRequest)
	if !strings.Contains(res.Termination.Detail, reason) {
		t.Fatalf("detail %q, want it to say %q", res.Termination.Detail, reason)
	}
}

// TestReentryCannotRenewBudget is A05: the same attempt cannot run twice, a
// later attempt cannot widen the envelope, and it starts from what earlier
// attempts consumed.
func TestReentryCannotRenewBudget(t *testing.T) {
	t.Run("same_attempt_refused", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("one"), end("two")})
		want(t, f.next(t, request()), api.OutcomeCompleted, api.CauseLoopCompleted)
		wantRefused(t, f.next(t, request()), "already admitted")
		if len(f.provider.Requests()) != 1 {
			t.Fatalf("re-entered attempt reached the provider: %d calls", len(f.provider.Requests()))
		}
	})
	t.Run("later_attempt_starts_from_consumed", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{toolUse(readCall("a", "a.txt")), end("one"), toolUse(readCall("b", "a.txt")), end("never")})
		first := attempt("att-1")
		first.Budget.MaxIterations = 3
		want(t, f.next(t, first), api.OutcomeCompleted, api.CauseLoopCompleted)
		second := attempt("att-2")
		second.Budget.MaxIterations = 3
		res := f.next(t, second)
		want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
		if res.Termination.Dimension != api.DimensionIterations || len(f.provider.Requests()) != 3 {
			t.Fatalf("dimension %q after %d calls: the second attempt renewed iterations",
				res.Termination.Dimension, len(f.provider.Requests()))
		}
	})
	widen := map[string]func(*api.Budget){
		"deadline":       func(b *api.Budget) { b.Deadline = b.Deadline.Add(time.Second) },
		"max_tool_calls": func(b *api.Budget) { b.MaxToolCalls++ },
		"max_input":      func(b *api.Budget) { b.MaxInputTokens++ },
		"retries":        func(b *api.Budget) { b.MaxProviderRetries++ },
		"money_added":    func(b *api.Budget) { b.Money = &api.MoneyCeiling{Currency: "USD", MaxMicros: 1} },
	}
	for name, mutate := range widen {
		t.Run("widened_"+name, func(t *testing.T) {
			f := newFixture(t, []scripted.Step{end("one"), end("two")})
			f.next(t, attempt("att-1"))
			second := attempt("att-2")
			mutate(&second.Budget)
			if name == "money_added" {
				one := int64(1)
				second.Providers[0].Pricing = &api.Pricing{Currency: "USD", InputMicrosPerMillion: 1, OutputMicrosPerMillion: 1,
					CachedInputMicrosPerMillion: &one, CacheWriteInputMicrosPerMillion: &one, Source: "card", Version: "1"}
			}
			wantRefused(t, f.next(t, second), "widens")
			if len(f.provider.Requests()) != 1 {
				t.Fatal("widening attempt reached the provider")
			}
		})
	}
	t.Run("narrower_attempt_admitted", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("one"), end("two")})
		f.next(t, attempt("att-1"))
		second := attempt("att-2")
		second.Budget.MaxToolCalls--
		want(t, f.next(t, second), api.OutcomeCompleted, api.CauseLoopCompleted)
	})
	t.Run("validation_refusal_is_not_admitted", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("ok")})
		bad := request()
		bad.Objective = ""
		want(t, f.next(t, bad), api.OutcomeBlocked, api.CauseInvalidRequest)
		want(t, f.next(t, request()), api.OutcomeCompleted, api.CauseLoopCompleted)
	})
}

// flakyRecords fails Put calls from the failFrom-th (1-based) on.
type flakyRecords struct {
	*storage.MemoryRecords
	mu       sync.Mutex
	puts     int
	failFrom int
}

func (f *flakyRecords) Put(ctx context.Context, partition, key string, value []byte) error {
	f.mu.Lock()
	f.puts++
	fail := f.failFrom > 0 && f.puts >= f.failFrom
	f.mu.Unlock()
	if fail {
		return errors.New("records unavailable")
	}
	return f.MemoryRecords.Put(ctx, partition, key, value)
}

func withAdmissions(r storage.Records) option {
	return func(cfg *engine.Config) { cfg.Admissions = r }
}

// TestUnsettledAttemptBlocksTheNext: an attempt whose consumption was never
// recorded (a crash, or a failed settlement write) leaves the execution's
// consumption unknown, so every later attempt is refused.
func TestUnsettledAttemptBlocksTheNext(t *testing.T) {
	records := &flakyRecords{MemoryRecords: storage.NewMemoryRecords(), failFrom: 2}
	f := newFixture(t, []scripted.Step{end("one"), end("two")}, withAdmissions(records))
	res := f.next(t, attempt("att-1"))
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if !strings.Contains(res.Termination.Detail, "completed/loop_completed") {
		t.Fatalf("observed outcome lost: %q", res.Termination.Detail)
	}
	wantRefused(t, f.next(t, attempt("att-2")), "unsettled")
	if len(f.provider.Requests()) != 1 {
		t.Fatal("attempt after an unsettled one reached the provider")
	}
}

// TestAdmissionWriteFailureRefusesBeforeSideEffects: if the in-flight marker
// cannot be persisted, nothing runs.
func TestAdmissionWriteFailureRefusesBeforeSideEffects(t *testing.T) {
	records := &flakyRecords{MemoryRecords: storage.NewMemoryRecords(), failFrom: 1}
	f := newFixture(t, []scripted.Step{end("never")}, withAdmissions(records))
	res := f.next(t, request())
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if len(f.provider.Requests()) != 0 || !strings.Contains(res.Termination.Detail, "nothing ran") {
		t.Fatalf("%d provider calls, detail %q", len(f.provider.Requests()), res.Termination.Detail)
	}
}

// gate is a provider whose calls wait for release.
type gate struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (g *gate) Complete(ctx context.Context, _ api.ProviderRequest) (api.ProviderResponse, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	select {
	case <-g.release:
		return api.ProviderResponse{Text: "ok", Stop: api.StopEnd, Usage: api.TokenUsage{Input: api.Count(1), Output: api.Count(1)}}, nil
	case <-ctx.Done():
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderCancelled}
	}
}

// TestConcurrentAttemptsAdmitExactlyOne: simultaneous Execute calls for one
// execution_id, re-entered and new attempts alike, admit exactly one; every
// other is refused before reaching the provider. Run with -race.
func TestConcurrentAttemptsAdmitExactlyOne(t *testing.T) {
	g := &gate{release: make(chan struct{})}
	f := newFixture(t, nil, withProvider(g))
	const n = 8
	results := make(chan api.ExecutionResult, n)
	for i := range n {
		go func() {
			res, _ := f.engine.Execute(context.Background(), attempt(fmt.Sprintf("att-%d", i%3)))
			results <- res
		}()
	}
	for i := range n - 1 {
		select {
		case res := <-results:
			want(t, res, api.OutcomeBlocked, api.CauseInvalidRequest)
		case <-time.After(5 * time.Second):
			close(g.release)
			t.Fatalf("only %d of %d concurrent attempts were refused; more than one was admitted", i, n-1)
		}
	}
	close(g.release)
	select {
	case res := <-results:
		want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted attempt never settled")
	}
	if g.calls != 1 {
		t.Fatalf("provider called %d times, want 1", g.calls)
	}
}
