package engine_test

import (
	"context"
	"math"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// hugeInput is a turn whose provider reports an input count near MaxInt64:
// non-negative and a consistent partition, so plausibleUsage keeps it.
func hugeInput() scripted.Step {
	return scripted.Step{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{readCall("c1", "a.txt")}, Stop: api.StopToolUse,
		Usage: api.TokenUsage{Input: api.Count(math.MaxInt64), Output: api.Count(10), CachedInput: api.Count(0)}}}
}

// TestHugeReportedInputStopsFurtherCalls: settling a reported input near
// MaxInt64 saturates the input dimension; the next reservation must not wrap
// past the limit check and reach the provider.
func TestHugeReportedInputStopsFurtherCalls(t *testing.T) {
	f := newFixture(t, []scripted.Step{hugeInput(), end("done")})
	res := f.run(t, context.Background(), request())
	if n := len(f.provider.Requests()); n != 1 {
		t.Fatalf("%d provider calls: a second call was admitted after a reported input of %d consumed max_input_tokens",
			n, int64(math.MaxInt64))
	}
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionInputTokens {
		t.Fatalf("exhausted %q, want input_tokens", res.Termination.Dimension)
	}
}

// TestOverflowCannotRenewBudgetAcrossAttempts: the saturated consumption is
// what the admission record carries, so a later attempt of the same execution
// starts with no input budget rather than a wrapped (renewed) one.
func TestOverflowCannotRenewBudgetAcrossAttempts(t *testing.T) {
	f := newFixture(t, []scripted.Step{hugeInput(), end("done"), end("again")})
	f.next(t, attempt("att-1"))
	res := f.next(t, attempt("att-2"))
	if res.Termination.Outcome == api.OutcomeCompleted || len(f.provider.Requests()) != 1 {
		t.Fatalf("attempt 2 %s after %d provider calls: attempt 1 consumed far more than max_input_tokens",
			describeTermination(res), len(f.provider.Requests()))
	}
}

// TestNegativeUsagePartMakesInputUnknown: a negative cache part makes input
// and both cache parts unknown, so a provider cannot settle input below what
// it reported by subtracting a negative part, and the reservation stands.
func TestNegativeUsagePartMakesInputUnknown(t *testing.T) {
	for name, u := range map[string]api.TokenUsage{
		"cached":      {Input: api.Count(10), Output: api.Count(1), CachedInput: api.Count(-5)},
		"cache_write": {Input: api.Count(10), Output: api.Count(1), CachedInput: api.Count(0), CacheWriteInput: api.Count(-5)},
	} {
		t.Run(name, func(t *testing.T) {
			step := scripted.Step{Response: api.ProviderResponse{Text: "ok", Stop: api.StopEnd, Usage: u}}
			f := newFixture(t, []scripted.Step{step})
			res := f.run(t, context.Background(), request())
			want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
			if r := res.Usage.Reported; r.Input != nil || r.CachedInput != nil || r.CacheWriteInput != nil {
				t.Fatalf("reported input %v cached %v write %v; a negative part must make all three unknown",
					deref(r.Input), deref(r.CachedInput), deref(r.CacheWriteInput))
			}
		})
	}
}

func deref(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func describeTermination(res api.ExecutionResult) string {
	t := res.Termination
	return string(t.Outcome) + "/" + string(t.Cause) + "/" + string(t.Dimension) + " " + t.Detail
}
