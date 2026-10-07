package engine_test

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// TestImpossibleCachePartitionGrantsNoRefund: a cache part larger than the
// reported input, or cached + cache-write together larger, is impossible, so
// input and both parts are unknown and the worst-case money reservation is
// not refunded.
func TestImpossibleCachePartitionGrantsNoRefund(t *testing.T) {
	cases := map[string]api.TokenUsage{
		"parts_sum_exceeds_input": {Input: api.Count(100), Output: api.Count(10), CachedInput: api.Count(100), CacheWriteInput: api.Count(100)},
		"one_part_exceeds_input":  {Input: api.Count(100), Output: api.Count(10), CachedInput: api.Count(150), CacheWriteInput: api.Count(0)},
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			step := scripted.Step{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{readCall("a", "a.txt")},
				Stop: api.StopToolUse, Usage: usage}}
			f := newFixture(t, []scripted.Step{step, end("never")})
			req := request()
			one := int64(1_000_000)
			req.Providers[0].Pricing = &api.Pricing{Currency: "USD", InputMicrosPerMillion: one, OutputMicrosPerMillion: 2 * one,
				CachedInputMicrosPerMillion: &one, CacheWriteInputMicrosPerMillion: &one, Source: "card", Version: "1"}
			// One worst case (50000 input + 1000 output at 2) is 52000 micros;
			// a second fits only if the first were refunded.
			req.Budget.Money = &api.MoneyCeiling{Currency: "USD", MaxMicros: 60000}
			res := f.run(t, context.Background(), req)
			want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
			if res.Termination.Dimension != api.DimensionMoney || len(f.provider.Requests()) != 1 {
				t.Fatalf("dimension %q after %d calls: impossible usage refunded money", res.Termination.Dimension,
					len(f.provider.Requests()))
			}
			u := res.Usage
			if u.Reported.Input != nil || u.Reported.CachedInput != nil || u.Cost.Known {
				t.Fatalf("impossible partition presented as known: %+v", u)
			}
		})
	}
}

// TestInputBudgetIsAHardBound is review item 5: each call reserves an upper
// bound of its input (bytes sent plus framing), computed by the kernel, so
// max_input_tokens holds even when the provider never reports input.
func TestInputBudgetIsAHardBound(t *testing.T) {
	// Measure the opening prompt with an unconstrained run.
	probe := newFixture(t, []scripted.Step{end("ok")})
	probe.run(t, context.Background(), request())
	first := probe.provider.Requests()[0]
	var bytes int64
	for _, m := range first.Messages {
		bytes += int64(len(m.Role) + len(m.Content))
	}
	for _, s := range first.Tools {
		bytes += int64(len(s.Name) + len(s.Description) + len(s.InputSchema))
	}

	t.Run("estimate_fits_bound_does_not", func(t *testing.T) {
		f := newFixture(t, []scripted.Step{end("never")})
		req := request()
		req.Budget.MaxInputTokens = bytes // the ~bytes/4 estimate fits, bytes + framing does not
		res := f.run(t, context.Background(), req)
		want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
		if res.Termination.Dimension != api.DimensionInputTokens || len(f.provider.Requests()) != 0 {
			t.Fatalf("dimension %q after %d calls", res.Termination.Dimension, len(f.provider.Requests()))
		}
	})
	unreported := scripted.Step{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{readCall("a", "a.txt")},
		Stop: api.StopToolUse, Usage: api.TokenUsage{Output: api.Count(1)}}}
	reported := unreported
	reported.Response.Usage.Input = api.Count(10)
	for name, c := range map[string]struct {
		step  scripted.Step
		calls int
	}{"unreported_input_keeps_the_bound": {unreported, 1}, "reported_input_settles": {reported, 2}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, []scripted.Step{c.step, end("done")})
			req := request()
			// Two upper bounds do not fit (the transcript grows); one bound
			// plus a reported 10 does.
			req.Budget.MaxInputTokens = 2 * bytes
			res := f.run(t, context.Background(), req)
			if len(f.provider.Requests()) != c.calls {
				t.Fatalf("%d provider calls, want %d (termination %+v)", len(f.provider.Requests()), c.calls, res.Termination)
			}
			if c.calls == 1 && res.Termination.Dimension != api.DimensionInputTokens {
				t.Fatalf("termination %+v, want input_tokens exhaustion", res.Termination)
			}
		})
	}
}
