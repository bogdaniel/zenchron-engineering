package engine

import (
	"errors"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ledger is the execution's resource account. Every operation reserves before
// it runs and settles after; reservations are atomic across dimensions, so
// concurrent reservers cannot overspend by passing independent prechecks.
// Nothing ever adds to a limit: retries spend from the same envelope, and a
// later attempt of the same execution starts from what earlier attempts
// consumed (admission.go).
type ledger struct {
	mu     sync.Mutex
	limits map[api.BudgetDimension]int64
	used   map[api.BudgetDimension]int64
}

type amount struct {
	dim api.BudgetDimension
	n   int64
}

func newLedger(b api.Budget) *ledger {
	limits := map[api.BudgetDimension]int64{
		api.DimensionIterations:    int64(b.MaxIterations),
		api.DimensionToolCalls:     int64(b.MaxToolCalls),
		api.DimensionInputTokens:   b.MaxInputTokens,
		api.DimensionOutputTokens:  b.MaxOutputTokens,
		api.DimensionArtifactBytes: b.MaxArtifactBytes,
		api.DimensionRetries:       int64(b.MaxProviderRetries),
	}
	if b.Money != nil {
		limits[api.DimensionMoney] = b.Money.MaxMicros
	}
	return &ledger{limits: limits, used: map[api.BudgetDimension]int64{}}
}

// errOverLimit reports a reservation that would overspend its dimension.
var errOverLimit = errors.New("over limit")

// errNegativeAmount reports a negative reservation. It is not exhaustion: a
// negative amount comes from a broken counter, and taking it would lower
// used and renew budget the host bounded.
var errNegativeAmount = errors.New("negative reservation refused")

// reserve takes every amount or none. On refusal it returns the offending
// dimension with errNegativeAmount or errOverLimit. A dimension without a
// limit (money when the host set no ceiling) is tracked but never refuses on
// size.
func (l *ledger) reserve(amounts ...amount) (api.BudgetDimension, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range amounts {
		if a.n < 0 {
			return a.dim, errNegativeAmount
		}
		limit, bounded := l.limits[a.dim]
		if bounded && l.used[a.dim]+a.n > limit {
			return a.dim, errOverLimit
		}
	}
	for _, a := range amounts {
		l.used[a.dim] += a.n
	}
	return "", nil
}

// consumed is a copy of every dimension's charge so far.
func (l *ledger) consumed() map[api.BudgetDimension]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[api.BudgetDimension]int64, len(l.used))
	for d, n := range l.used {
		out[d] = n
	}
	return out
}

// restore charges what earlier attempts of the same execution consumed. It
// runs once, at admission, before any reservation.
func (l *ledger) restore(prior map[api.BudgetDimension]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for d, n := range prior {
		l.used[d] += n
	}
}

// settle replaces a reservation with the actual charge. The actual may exceed
// the reservation (a provider reporting more than the local estimate); the
// overrun is kept, so the next reservation fails rather than the excess
// disappearing.
func (l *ledger) settle(dim api.BudgetDimension, reserved, actual int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used[dim] += actual - reserved
}

// remaining is the unreserved allowance of a bounded dimension, never negative.
func (l *ledger) remaining(dim api.BudgetDimension) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.limits[dim]-l.used[dim], 0)
}
