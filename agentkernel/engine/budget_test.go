package engine

import (
	"errors"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
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
