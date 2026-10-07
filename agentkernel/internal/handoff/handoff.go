// Package handoff is the kernel side of every host port: it hands one
// api.Call to a host-owned worker and waits for the reply, never longer than
// a bound and never on a goroutine of its own.
package handoff

import (
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ErrNotTaken reports a call no worker received before the bound: the host
// never saw it, so it had no effect.
var ErrNotTaken = errors.New("handoff: not taken")

// ErrNoAnswer reports a call a worker received but did not answer before the
// bound: whatever it did is unknown.
var ErrNoAnswer = errors.New("handoff: not answered")

// Bound is how long the kernel waits on one call.
type Bound struct {
	// Until is the latest time to wait; zero means no fixed limit.
	Until time.Time
	// Shorten, when it fires, cuts the wait to at most Grace from then (the
	// host cancelled; Grace 0 stops at once). Nil never fires.
	Shorten <-chan struct{}
	Grace   time.Duration
}

// Exchange hands call to a worker on calls and returns its reply. It sets
// call.Reply to a fresh channel of capacity 1, so a late reply never blocks
// the worker and is discarded. It starts no goroutine: when the bound
// passes it returns ErrNotTaken or ErrNoAnswer and nothing of the kernel's
// is left waiting.
func Exchange[Q, R any](what string, calls chan<- api.Call[Q, R], call api.Call[Q, R], b Bound) (R, error) {
	start := time.Now()
	reply := make(chan R, 1)
	call.Reply = reply
	w := newWait(b)
	defer w.stop()
	var zero R
	for {
		select {
		case calls <- call:
			calls = nil // taken: from here on only the reply or the bound
		case v := <-reply:
			return v, nil
		case <-w.expired:
			elapsed := time.Since(start).Round(time.Millisecond)
			if calls != nil {
				return zero, fmt.Errorf("%w: %s did not take call %s within %s", ErrNotTaken, what, call.ID, elapsed)
			}
			return zero, fmt.Errorf("%w: %s did not answer call %s within %s", ErrNoAnswer, what, call.ID, elapsed)
		case <-w.shorten:
			w.cut()
		}
	}
}

// wait is the timer side of one Exchange.
type wait struct {
	timer   *time.Timer
	expired <-chan time.Time
	shorten <-chan struct{}
	until   time.Time
	grace   time.Duration
}

func newWait(b Bound) *wait {
	w := &wait{shorten: b.Shorten, until: b.Until, grace: b.Grace}
	if !b.Until.IsZero() {
		w.timer = time.NewTimer(time.Until(b.Until))
		w.expired = w.timer.C
	}
	return w
}

// cut applies the grace once; a grace ending after Until changes nothing.
func (w *wait) cut() {
	w.shorten = nil
	end := time.Now().Add(w.grace)
	if !w.until.IsZero() && !end.Before(w.until) {
		return
	}
	w.stop()
	w.timer = time.NewTimer(w.grace)
	w.expired = w.timer.C
}

func (w *wait) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}
