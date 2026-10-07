package engine_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
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
// later attempt cannot widen a cumulative bound, and it starts from what
// earlier attempts consumed. Since v0.2 the deadline is attempt-scoped: a new
// attempt may carry a later one.
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
	t.Run("later_deadline_new_attempt_admitted_from_consumed", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{toolUse(readCall("a", "a.txt")), end("one"), toolUse(readCall("b", "a.txt")), end("never")})
		first := attempt("att-1")
		first.Budget.MaxIterations = 3
		want(t, f.next(t, first), api.OutcomeCompleted, api.CauseLoopCompleted)
		second := attempt("att-2")
		second.Budget.MaxIterations = 3
		second.Budget.Deadline = first.Budget.Deadline.Add(time.Hour)
		res := f.next(t, second)
		want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
		if res.Termination.Dimension != api.DimensionIterations || len(f.provider.Requests()) != 3 {
			t.Fatalf("dimension %q after %d calls: a later deadline must admit the attempt and keep consumption",
				res.Termination.Dimension, len(f.provider.Requests()))
		}
	})
	t.Run("same_attempt_later_deadline_refused", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("one"), end("two")})
		want(t, f.next(t, request()), api.OutcomeCompleted, api.CauseLoopCompleted)
		again := request()
		again.Budget.Deadline = again.Budget.Deadline.Add(time.Hour)
		wantRefused(t, f.next(t, again), "already admitted")
		if len(f.provider.Requests()) != 1 {
			t.Fatal("re-entered attempt with a later deadline reached the provider")
		}
	})
	widen := map[string]func(*api.Budget){
		"max_tool_calls": func(b *api.Budget) { b.MaxToolCalls++ },
		"max_input":      func(b *api.Budget) { b.MaxInputTokens++ },
		"retries":        func(b *api.Budget) { b.MaxProviderRetries++ },
		"max_iterations": func(b *api.Budget) { b.MaxIterations++ },
		"max_output":     func(b *api.Budget) { b.MaxOutputTokens++ },
		"max_artifact":   func(b *api.Budget) { b.MaxArtifactBytes++ },
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
	t.Run("widened_money_currency", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("one"), end("two")})
		first, second := priced(attempt("att-1"), "USD"), priced(attempt("att-2"), "EUR")
		want(t, f.next(t, first), api.OutcomeCompleted, api.CauseLoopCompleted)
		wantRefused(t, f.next(t, second), "widens money")
		if len(f.provider.Requests()) != 1 {
			t.Fatal("an attempt in another currency reached the provider")
		}
	})
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
	f := newFixture(t, nil, withProvider(t, g))
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

// priced puts req under a money ceiling in currency, with a full rate card.
func priced(req api.ExecutionRequest, currency string) api.ExecutionRequest {
	one := int64(1)
	req.Budget.Money = &api.MoneyCeiling{Currency: currency, MaxMicros: 1_000_000}
	req.Providers[0].Pricing = &api.Pricing{Currency: currency, InputMicrosPerMillion: 1, OutputMicrosPerMillion: 1,
		CachedInputMicrosPerMillion: &one, CacheWriteInputMicrosPerMillion: &one, Source: "card", Version: "1"}
	return req
}

// TestPriorConsumptionIsCountedOnce: each attempt's record carries what the
// execution consumed through that attempt; admission must not add those
// cumulative records together. Five one-iteration attempts fit a five-
// iteration envelope exactly; the sixth finds no iteration left.
func TestPriorConsumptionIsCountedOnce(t *testing.T) {
	f := newFixture(t, []scripted.Step{end("1"), end("2"), end("3"), end("4"), end("5"), end("never")})
	for i := 1; i <= 5; i++ {
		res := f.next(t, attempt(fmt.Sprintf("att-%d", i)))
		if res.Termination.Outcome != api.OutcomeCompleted {
			t.Fatalf("attempt %d %s after only %d of 5 iterations were used", i, describeTermination(res), i-1)
		}
	}
	res := f.next(t, attempt("att-6"))
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionIterations || len(f.provider.Requests()) != 5 {
		t.Fatalf("attempt 6 exhausted %q after %d calls, want iterations after 5", res.Termination.Dimension, len(f.provider.Requests()))
	}
}
