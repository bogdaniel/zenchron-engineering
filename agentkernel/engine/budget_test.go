package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// TestConcurrentReservationsCannotOverspend is A05: independent reservers
// racing on one ledger never take more than the limit, in any dimension of a
// multi-dimension reservation. Run with -race.
func TestConcurrentReservationsCannotOverspend(t *testing.T) {
	l := newLedger(api.Budget{MaxToolCalls: 100, MaxOutputTokens: 1000, MaxIterations: 1, MaxInputTokens: 1, MaxArtifactBytes: 1})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.reserve(amount{api.DimensionToolCalls, 1}, amount{api.DimensionOutputTokens, 7}); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// 1000/7 = 142 output reservations fit, but only 100 tool calls do.
	if granted != 100 || l.remaining(api.DimensionToolCalls) != 0 || l.remaining(api.DimensionOutputTokens) != 300 {
		t.Fatalf("granted %d, tool calls left %d, output left %d", granted,
			l.remaining(api.DimensionToolCalls), l.remaining(api.DimensionOutputTokens))
	}
}

func TestSettleKeepsOverrun(t *testing.T) {
	l := newLedger(api.Budget{MaxInputTokens: 100})
	if _, err := l.reserve(amount{api.DimensionInputTokens, 50}); err != nil {
		t.Fatal("reservation refused")
	}
	l.settle(api.DimensionInputTokens, 50, 120) // provider reported more than estimated
	if dim, err := l.reserve(amount{api.DimensionInputTokens, 0}); err == nil || dim != api.DimensionInputTokens {
		t.Fatal("overrun forgotten: reservation admitted past the limit")
	}
}

// TestNegativeReservationCannotRenewBudget: a negative amount (a broken token
// estimator, say) is refused at the root with a distinct error, takes none of
// a multi-dimension reservation, and can never lower what was used.
func TestNegativeReservationCannotRenewBudget(t *testing.T) {
	l := newLedger(api.Budget{MaxInputTokens: 100, MaxOutputTokens: 100})
	if _, err := l.reserve(amount{api.DimensionInputTokens, 100}); err != nil {
		t.Fatal(err)
	}
	dim, err := l.reserve(amount{api.DimensionOutputTokens, 10}, amount{api.DimensionInputTokens, -50})
	if !errors.Is(err, errNegativeAmount) || dim != api.DimensionInputTokens {
		t.Fatalf("negative reservation: dim %q err %v, want input_tokens errNegativeAmount", dim, err)
	}
	if l.remaining(api.DimensionInputTokens) != 0 || l.remaining(api.DimensionOutputTokens) != 100 {
		t.Fatalf("negative reservation changed the ledger: input left %d, output left %d",
			l.remaining(api.DimensionInputTokens), l.remaining(api.DimensionOutputTokens))
	}
}

// TestLedgerSaturatesInsteadOfWrapping: a settle or restore whose sum passes
// MaxInt64 saturates there, so the dimension stays exhausted; neither may
// wrap negative and admit a later reservation.
func TestLedgerSaturatesInsteadOfWrapping(t *testing.T) {
	l := newLedger(api.Budget{MaxInputTokens: 100, MaxOutputTokens: 100, MaxToolCalls: 100})
	if _, err := l.reserve(amount{api.DimensionInputTokens, 50}); err != nil {
		t.Fatal(err)
	}
	l.settle(api.DimensionInputTokens, 50, math.MaxInt64)
	l.settle(api.DimensionInputTokens, 0, math.MaxInt64)
	l.restore(map[api.BudgetDimension]int64{api.DimensionOutputTokens: math.MaxInt64})
	l.restore(map[api.BudgetDimension]int64{api.DimensionOutputTokens: math.MaxInt64})
	// A negative prior is a corrupt record: unknown, never a credit.
	l.restore(map[api.BudgetDimension]int64{api.DimensionToolCalls: -5})
	for _, dim := range []api.BudgetDimension{api.DimensionInputTokens, api.DimensionOutputTokens, api.DimensionToolCalls} {
		if got := l.consumed()[dim]; got != math.MaxInt64 {
			t.Fatalf("%s consumed %d, want saturation at MaxInt64", dim, got)
		}
		if _, err := l.reserve(amount{dim, 1}); !errors.Is(err, errOverLimit) {
			t.Fatalf("%s: reservation after saturation returned %v, want errOverLimit", dim, err)
		}
	}
}

// TestOutOfRangePriceIsUnknown: usage whose price does not fit in int64 has
// an unknown cost (the worst-case money charge stands), and a worst case that
// does not fit saturates; neither converts to a wrapped or negative charge.
func TestOutOfRangePriceIsUnknown(t *testing.T) {
	one := int64(1)
	p := &api.Pricing{Currency: "USD", InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000,
		CachedInputMicrosPerMillion: &one, CacheWriteInputMicrosPerMillion: &one}
	u := api.TokenUsage{Input: api.Count(math.MaxInt64), Output: api.Count(math.MaxInt64), CachedInput: api.Count(0),
		CacheWriteInput: api.Count(0)}
	if micros, known := actualCost(u, p); known {
		t.Fatalf("cost of %d tokens reported known as %d micros", int64(math.MaxInt64), micros)
	}
	if got := worstCaseCost(math.MaxInt64, math.MaxInt64, p); got != math.MaxInt64 {
		t.Fatalf("worst case %d, want saturation at MaxInt64", got)
	}
	// Float-to-int conversion out of range is implementation-defined (amd64
	// yields MinInt64, arm64 maps NaN to 0); price must saturate everywhere.
	for _, v := range []float64{1e30, math.Inf(1), math.NaN()} {
		if got := price(v); got != math.MaxInt64 {
			t.Fatalf("price(%v) = %d, want saturation at MaxInt64", v, got)
		}
	}
}

// TestUnansweredProviderCallKeepsReservations: a call the provider took but
// never answered may have consumed everything it was allowed, so every
// reservation stays charged; one never taken releases them all.
func TestUnansweredProviderCallKeepsReservations(t *testing.T) {
	held := reservation{in: 10, out: 20, money: 30}
	for name, c := range map[string]struct {
		err  error
		want int64
	}{"no_answer": {handoff.ErrNoAnswer, 1}, "not_taken": {handoff.ErrNotTaken, 0}} {
		t.Run(name, func(t *testing.T) {
			r := newRun(&Engine{}, context.Background(), api.ExecutionRequest{Budget: api.Budget{
				MaxInputTokens: 100, MaxOutputTokens: 100, Money: &api.MoneyCeiling{Currency: "USD", MaxMicros: 100}}})
			if _, err := r.ledger.reserve(held.amounts()...); err != nil {
				t.Fatal(err)
			}
			r.callFailed(call{}, held, fmt.Errorf("provider: %w", c.err), 0)
			got := r.ledger.consumed()
			for _, a := range held.amounts() {
				if got[a.dim] != a.n*c.want {
					t.Fatalf("%s charged %d after %v, want %d", a.dim, got[a.dim], c.err, a.n*c.want)
				}
			}
		})
	}
}
