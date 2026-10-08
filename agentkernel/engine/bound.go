package engine

import (
	"context"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// DefaultSettleTimeout is the settlement grace used when Config.SettleTimeout
// is zero: how long terminal recording may take after the loop ends, and how
// long a hand-off may still wait after the host cancels. It is the most the
// kernel ever runs past the host's deadline or cancellation.
const DefaultSettleTimeout = 5 * time.Second

// recordingBound is how long an event or context hand-off may wait:
//
//   - terminal events (refusal, settlement) until r.settleBy;
//   - in-loop hand-offs until the budget deadline, cut to the settlement
//     grace when the host cancels, including while the hand-off waits; once
//     the deadline has passed or the host has cancelled, for the grace.
//
// Every host port is reached through handoff.Exchange, so the bound is
// enforced without a kernel goroutine: when it passes, the kernel stops
// waiting and a stuck host worker strands only itself.
func (r *run) recordingBound(terminal bool) handoff.Bound {
	if terminal {
		return handoff.Bound{Until: r.settleBy}
	}
	now := time.Now()
	if r.parent.Err() != nil || !now.Before(r.req.Budget.Deadline) {
		return handoff.Bound{Until: now.Add(r.e.settleTimeout)}
	}
	return handoff.Bound{Until: r.req.Budget.Deadline, Shorten: r.parent.Done(), Grace: r.e.settleTimeout}
}

// recordingContext is the context an event or context hand-off carries. It
// ignores the execution's cancellation (the host must still see how a
// cancelled execution ended) and ends at the bound; the caller cancels it
// when it stops waiting.
func recordingContext(ctx context.Context, b handoff.Bound) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.WithoutCancel(ctx), b.Until)
}

// providerBound is how long a provider hand-off may wait: until the budget
// deadline, or the settlement grace after a host cancellation, so a
// provider honouring its context can still report what the call consumed.
func (r *run) providerBound() handoff.Bound {
	return handoff.Bound{Until: r.req.Budget.Deadline, Shorten: r.parent.Done(), Grace: r.e.settleTimeout}
}
