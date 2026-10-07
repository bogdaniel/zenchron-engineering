package engine

import (
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ledger is the execution's resource account. Every operation reserves before
// it runs and settles after; reservations are atomic across dimensions, so
// concurrent reservers cannot overspend by passing independent prechecks.
// Nothing ever adds to a limit: retries and re-entry spend from the same
// envelope.
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

// reserve takes every amount or none. It returns the first dimension that
// would overspend. A dimension without a limit (money when the host set no
// ceiling) is tracked but never refuses.
func (l *ledger) reserve(amounts ...amount) (api.BudgetDimension, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range amounts {
		limit, bounded := l.limits[a.dim]
		if bounded && l.used[a.dim]+a.n > limit {
			return a.dim, false
		}
	}
	for _, a := range amounts {
		l.used[a.dim] += a.n
	}
	return "", true
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
