package engine

import (
	"context"
	"fmt"
	"time"
)

// DefaultSettleTimeout is the settlement grace used when Config.SettleTimeout
// is zero: how long terminal recording may take after the loop ends, and how
// long an in-flight recording may continue after the host cancels. It is the
// most the kernel ever runs past the host's deadline or cancellation.
const DefaultSettleTimeout = 5 * time.Second

// bounded runs f, a call into a recording port (EventSink, ContextSource, the
// Admissions store),
// under a context the caller's cancellation does not end, and waits for it
// at most until its bound:
//
//   - terminal calls (refusal, settlement and the admission settlement
//     write) until r.settleBy;
//   - in-loop calls until the budget deadline, or for the settlement grace
//     when the deadline has passed or the host has cancelled, including a
//     cancellation that arrives while f is running.
//
// f's context carries that bound as its deadline and is cancelled when the
// kernel stops waiting. A call still running then is abandoned and reported
// as an error, so the caller stops side effects.
//
// Only recording ports are abandoned. Providers, command runners and tools
// are never abandoned, because abandoning a mutating call would hide its
// effect; they must honour their context.
func (r *run) bounded(ctx context.Context, terminal bool, what string, f func(context.Context) error) error {
	start := time.Now()
	until, watchHost := r.recordingBound(terminal, start)
	callCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), until)
	defer cancel()
	done := make(chan error, 1)
	// ponytail: a callee that ignores its context keeps this goroutine until
	// it returns; the kernel never waits for it past the bound. Bound the
	// number of abandoned calls per process if a host ever hits that.
	go func() { done <- f(callCtx) }()
	var hostDone <-chan struct{}
	if watchHost {
		hostDone = r.parent.Done()
	}
	for {
		select {
		case err := <-done:
			return err
		case <-callCtx.Done():
			return fmt.Errorf("%s did not return within %s", what, time.Since(start).Round(time.Millisecond))
		case <-hostDone:
			hostDone = nil
			grace := time.AfterFunc(r.e.settleTimeout, cancel)
			defer grace.Stop()
		}
	}
}

// recordingBound is the latest time a recording call may run, and whether a
// host cancellation should shorten it to the settlement grace.
func (r *run) recordingBound(terminal bool, now time.Time) (time.Time, bool) {
	if terminal {
		return r.settleBy, false
	}
	if r.parent.Err() != nil || !now.Before(r.req.Budget.Deadline) {
		return now.Add(r.e.settleTimeout), false
	}
	return r.req.Budget.Deadline, true
}
