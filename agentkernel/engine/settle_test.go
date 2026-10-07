package engine_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// TestObservedOutcomeSurvivesLateCancel is A06: the provider answers, then
// the host cancels before settlement; the observed outcome stands and no
// goroutine is left behind.
func TestObservedOutcomeSurvivesLateCancel(t *testing.T) {
	cases := map[string]struct {
		step    scripted.Step
		outcome api.Outcome
		cause   api.Cause
	}{
		"completion": {end("done"), api.OutcomeCompleted, api.CauseLoopCompleted},
		"failure": {scripted.Step{Err: &api.ProviderError{Class: api.ProviderRejected, Status: 400, Detail: "bad"}},
			api.OutcomeFailed, api.CauseProviderFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			c.step.Before = func() { cancel(api.Cancellation(api.CancelOperatorStop)) }
			f := newFixture(t, []scripted.Step{c.step})
			res := f.run(t, ctx, request())
			want(t, res, c.outcome, c.cause)
			if res.Termination.Cancellation != "" {
				t.Fatalf("late cancellation rewrote the termination: %+v", res.Termination)
			}
			if after := runtime.NumGoroutine(); after > before {
				t.Fatalf("goroutines %d -> %d", before, after)
			}
		})
	}
}

func TestCancellationProvenance(t *testing.T) {
	for name, cause := range map[string]error{
		"operator": api.Cancellation(api.CancelOperatorStop),
		"bare":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			f := newFixture(t, []scripted.Step{{Block: true, Entered: entered}})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan api.ExecutionResult, 1)
			go func() { done <- f.run(t, ctx, request()) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider call never started")
			}
			cancel(cause)
			var res api.ExecutionResult
			select {
			case res = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("execution did not settle after cancellation")
			}
			want(t, res, api.OutcomeCancelled, api.CauseHostCancelled)
			wantProv := api.CancelOperatorStop
			if cause == nil {
				wantProv = api.CancelUnknown
			}
			if res.Termination.Cancellation != wantProv {
				t.Fatalf("cancellation provenance %q, want %q", res.Termination.Cancellation, wantProv)
			}
		})
	}
}

func TestDeadlineExhaustion(t *testing.T) {
	var f *fixture
	step := toolUse(readCall("c1", "a.txt"))
	step.Before = func() { f.clock.advance(budgetSpan) }
	f = newFixture(t, []scripted.Step{step, end("never")})
	res := f.run(t, context.Background(), request())
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionDeadline || len(f.provider.Requests()) != 1 {
		t.Fatalf("dimension %q, %d calls", res.Termination.Dimension, len(f.provider.Requests()))
	}
	if countKind(f.sink.kinds(), api.EventToolProposed) != 0 {
		t.Fatal("tool ran after the deadline")
	}
}

func TestBudgetDimensions(t *testing.T) {
	cases := map[string]struct {
		steps  []scripted.Step
		mutate func(*api.ExecutionRequest)
		dim    api.BudgetDimension
		calls  int
	}{
		"iterations": {
			steps:  []scripted.Step{toolUse(readCall("a", "a.txt")), toolUse(readCall("b", "a.txt")), end("never")},
			mutate: func(r *api.ExecutionRequest) { r.Budget.MaxIterations = 2 }, dim: api.DimensionIterations, calls: 2,
		},
		"tool_calls": {
			steps:  []scripted.Step{toolUse(readCall("a", "a.txt"), readCall("b", "a.txt")), end("never")},
			mutate: func(r *api.ExecutionRequest) { r.Budget.MaxToolCalls = 1 }, dim: api.DimensionToolCalls, calls: 1,
		},
		"input_tokens_reported_over_estimate": {
			steps: []scripted.Step{{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{readCall("a", "a.txt")},
				Stop: api.StopToolUse, Usage: api.TokenUsage{Input: api.Count(9000), Output: api.Count(1)}}}, end("never")},
			mutate: func(r *api.ExecutionRequest) { r.Budget.MaxInputTokens = 9000 }, dim: api.DimensionInputTokens, calls: 1,
		},
		"artifact_bytes": {
			steps:  []scripted.Step{toolUse(readCall("a", "a.txt")), end("never")},
			mutate: func(r *api.ExecutionRequest) { r.Budget.MaxArtifactBytes = 8 }, dim: api.DimensionArtifactBytes, calls: 1,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c.steps, func(cfg *engine.Config) { cfg.OutputLimit = 16 })
			req := request()
			c.mutate(&req)
			res := f.run(t, context.Background(), req)
			want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
			if res.Termination.Dimension != c.dim || len(f.provider.Requests()) != c.calls {
				t.Fatalf("dimension %q after %d calls, want %q after %d", res.Termination.Dimension,
					len(f.provider.Requests()), c.dim, c.calls)
			}
		})
	}
}

// TestRetriesAreBoundedAndNeverRenew: retries spend the one allowance; an
// exhausted allowance ends the execution with the typed observation.
func TestRetriesAreBoundedAndNeverRenew(t *testing.T) {
	perr := func(class api.ProviderErrorClass, retryable bool) scripted.Step {
		return scripted.Step{Err: &api.ProviderError{Class: class, Retryable: retryable, Status: 503}}
	}
	cases := map[string]struct {
		steps   []scripted.Step
		outcome api.Outcome
		cause   api.Cause
		calls   int
	}{
		"server_exhausts":    {[]scripted.Step{perr(api.ProviderServer, true), perr(api.ProviderServer, true), perr(api.ProviderServer, true), end("x")}, api.OutcomeFailed, api.CauseProviderFailed, 3},
		"unavailable_blocks": {[]scripted.Step{perr(api.ProviderUnavailable, true), perr(api.ProviderRateLimited, true), perr(api.ProviderRateLimited, true), end("x")}, api.OutcomeBlocked, api.CauseProviderUnavailable, 3},
		"recovers":           {[]scripted.Step{perr(api.ProviderTransport, true), end("ok")}, api.OutcomeCompleted, api.CauseLoopCompleted, 2},
		"not_retryable":      {[]scripted.Step{perr(api.ProviderAuth, false), end("x")}, api.OutcomeFailed, api.CauseProviderFailed, 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, c.steps)
			res := f.run(t, context.Background(), request())
			want(t, res, c.outcome, c.cause)
			if len(f.provider.Requests()) != c.calls || res.Usage.Retries > request().Budget.MaxProviderRetries {
				t.Fatalf("%d calls, %d retries", len(f.provider.Requests()), res.Usage.Retries)
			}
			if res.Usage.Iterations != 1 {
				t.Fatalf("retries consumed %d iterations", res.Usage.Iterations)
			}
		})
	}
}

func TestRecordingFailureStopsSideEffects(t *testing.T) {
	// Events: started, routing, context, provider.requested, provider.responded(5), ...
	f := newFixture(t, []scripted.Step{toolUse(readCall("a", "a.txt")), end("never")})
	f.sink.failAt = 5
	res := f.run(t, context.Background(), request())
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if len(f.provider.Requests()) != 1 || countKind(f.sink.kinds(), api.EventToolProposed) != 0 {
		t.Fatal("side effects continued after a recording failure")
	}
	if res.EventCount != 4 {
		t.Fatalf("evidence so far not returned: %d events", res.EventCount)
	}
}

func TestSettlementRecordingFailureKeepsObservedOutcome(t *testing.T) {
	f := newFixture(t, []scripted.Step{end("done")})
	f.sink.failAt = 6 // started, routing, context, requested, responded, settled
	res := f.run(t, context.Background(), request())
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if !strings.Contains(res.Termination.Detail, "completed/loop_completed") {
		t.Fatalf("observed outcome lost: %q", res.Termination.Detail)
	}
}

func TestMoneyCeiling(t *testing.T) {
	pricing := &api.Pricing{Currency: "USD", InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		CachedInputMicrosPerMillion: 100_000, CacheWriteInputMicrosPerMillion: 1_000_000, Source: "test-card", Version: "1"}
	t.Run("worst_case_refused_before_call", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("never")})
		req := request()
		req.Providers[0].Pricing = pricing
		req.Budget.Money = &api.MoneyCeiling{Currency: "USD", MaxMicros: 500} // < 1000 output tokens at 2 micros
		res := f.run(t, context.Background(), req)
		want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
		if res.Termination.Dimension != api.DimensionMoney || len(f.provider.Requests()) != 0 {
			t.Fatalf("dimension %q, %d calls", res.Termination.Dimension, len(f.provider.Requests()))
		}
	})
	t.Run("reported_usage_priced", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("ok")})
		req := request()
		req.Providers[0].Pricing = pricing
		res := f.run(t, context.Background(), req)
		c := res.Usage.Cost
		// 100 input (0 cached, writes unreported but priced as input) at 1
		// micro + 10 output at 2 micros = 120.
		if !c.Known || c.Micros != 120 || c.Currency != "USD" || c.RateSource != "test-card" {
			t.Fatalf("cost %+v", c)
		}
	})
	t.Run("cache_writes_priced_exactly", func(t *testing.T) {
		usage := api.TokenUsage{Input: api.Count(1000), Output: api.Count(10), CachedInput: api.Count(200), CacheWriteInput: api.Count(300)}
		f := newFixture(t, []scripted.Step{{Response: api.ProviderResponse{Text: "ok", Stop: api.StopEnd, Usage: usage}}})
		req := request()
		card := *pricing
		card.CacheWriteInputMicrosPerMillion = 1_250_000
		req.Providers[0].Pricing = &card
		res := f.run(t, context.Background(), req)
		// 500 plain at 1 + 200 cached at 0.1 + 300 written at 1.25 + 10 output at 2 = 915.
		if c := res.Usage.Cost; !c.Known || c.Micros != 915 {
			t.Fatalf("cost %+v, want exactly 915 micros", c)
		}
		// Unreported writes at a distinct rate cannot be priced.
		usage.CacheWriteInput = nil
		f = newFixture(t, []scripted.Step{{Response: api.ProviderResponse{Text: "ok", Stop: api.StopEnd, Usage: usage}}})
		if c := f.run(t, context.Background(), req).Usage.Cost; c.Known {
			t.Fatalf("cost %+v presented as known without the write count", c)
		}
	})
	t.Run("unreported_usage_unknown", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{{Response: api.ProviderResponse{Text: "ok", Stop: api.StopEnd}}})
		req := request()
		req.Providers[0].Pricing = pricing
		res := f.run(t, context.Background(), req)
		u := res.Usage
		if u.Cost.Known || u.Reported.Input != nil || u.Estimated.Input == nil || !contains(u.Unknowns, "reported.input", "cost") {
			t.Fatalf("unknown usage presented as known: %+v", u)
		}
	})
}

func contains(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}
