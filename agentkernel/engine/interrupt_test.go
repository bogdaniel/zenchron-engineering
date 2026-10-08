package engine

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// TestDeadlineHoldsBeforeItsTimerFires: a hand-off that stopped waiting at
// the budget deadline can return before the run context's deadline timer fires.
// With an injected clock that has not reached the deadline, the run must
// still stop on wall time rather than take one more step in that gap. (The
// race showed up under full-suite load as a completed run after a stuck
// context source; this pins the invariant deterministically.)
func TestDeadlineHoldsBeforeItsTimerFires(t *testing.T) {
	past := time.Now().Add(-time.Millisecond)
	r := &run{
		e:      &Engine{clock: api.NewManualClock(past.Add(-time.Hour))},
		req:    api.ExecutionRequest{Budget: api.Budget{Deadline: past}},
		parent: context.Background(),
	}
	// context.Background has no deadline and never ends: the timer "has not
	// fired yet".
	term, stop := r.interrupted(context.Background())
	if !stop || term.Dimension != api.DimensionDeadline {
		t.Fatalf("interrupted = %+v, %v; want exhausted on the deadline", term, stop)
	}
}
