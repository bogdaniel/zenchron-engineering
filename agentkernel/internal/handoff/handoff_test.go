package handoff

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

type call = api.Call[string, int]

// waitForGoroutines fails unless the goroutine count returns to base: no
// goroutine of Exchange's may outlive it.
func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines remain beyond the %d running before Exchange", runtime.NumGoroutine()-base, base)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// holder takes calls and answers each only once release is closed. taken
// receives each call as it is taken.
func holder(t *testing.T, answer int) (chan<- call, <-chan call, chan<- struct{}) {
	calls, taken, release := make(chan call), make(chan call, 4), make(chan struct{})
	ctx := t.Context()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-calls:
				taken <- c
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
				c.Reply <- answer // capacity 1: never blocks, even when late
			}
		}
	}()
	return calls, taken, release
}

func TestExchangeAnswered(t *testing.T) {
	calls := api.Serve(t.Context(), func(_ context.Context, q string) int { return len(q) })
	got, err := Exchange("worker", calls, call{ID: "c1", Request: "four"}, Bound{Until: time.Now().Add(time.Second)})
	if err != nil || got != 4 {
		t.Fatalf("got %d, %v", got, err)
	}
}

// TestExchangeNeverTaken: nobody reads the channel. Exchange returns at the
// bound with ErrNotTaken and leaves nothing behind.
func TestExchangeNeverTaken(t *testing.T) {
	base := runtime.NumGoroutine()
	start := time.Now()
	_, err := Exchange("worker", make(chan call), call{ID: "c1"}, Bound{Until: time.Now().Add(50 * time.Millisecond)})
	if !errors.Is(err, ErrNotTaken) || time.Since(start) > time.Second {
		t.Fatalf("err %v after %s, want ErrNotTaken at the bound", err, time.Since(start))
	}
	waitForGoroutines(t, base)
}

// TestExchangeTakenNeverAnswered: the worker takes the call and hangs.
// Exchange returns at the bound with ErrNoAnswer; the stuck goroutine is the
// worker's, counted in base, and Exchange adds none.
func TestExchangeTakenNeverAnswered(t *testing.T) {
	calls, _, _ := holder(t, 1)
	base := runtime.NumGoroutine()
	_, err := Exchange("worker", calls, call{ID: "c1"}, Bound{Until: time.Now().Add(50 * time.Millisecond)})
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrNoAnswer", err)
	}
	waitForGoroutines(t, base)
}

// TestExchangeLateAnswerIsDiscarded: a reply after the bound neither blocks
// the worker nor reaches a later exchange, which gets its own answer.
func TestExchangeLateAnswerIsDiscarded(t *testing.T) {
	calls, taken, release := holder(t, 7)
	_, err := Exchange("worker", calls, call{ID: "late"}, Bound{Until: time.Now().Add(50 * time.Millisecond)})
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrNoAnswer", err)
	}
	<-taken
	close(release) // the late answer is sent now, into the abandoned reply channel
	got, err := Exchange("worker", calls, call{ID: "next"}, Bound{Until: time.Now().Add(time.Second)})
	if err != nil || got != 7 {
		t.Fatalf("next exchange got %d, %v: the late answer blocked the worker or leaked across calls", got, err)
	}
	if c := <-taken; c.ID != "next" {
		t.Fatalf("worker took %q", c.ID)
	}
}

// TestExchangeShortenedByCancellation: a cancellation cuts a long bound to
// the grace, and Grace 0 stops at once.
func TestExchangeShortenedByCancellation(t *testing.T) {
	for _, grace := range []time.Duration{0, 50 * time.Millisecond} {
		calls, taken, _ := holder(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-taken; cancel() }()
		start := time.Now()
		_, err := Exchange("worker", calls, call{ID: "c1"}, Bound{Until: time.Now().Add(time.Hour), Shorten: ctx.Done(), Grace: grace})
		if !errors.Is(err, ErrNoAnswer) || time.Since(start) > grace+time.Second {
			t.Fatalf("grace %s: err %v after %s", grace, err, time.Since(start))
		}
	}
}

// TestExpiredBoundHandsNothingOver: a bound already over when Exchange starts
// (its time passed, or Shorten closed with no grace) hands nothing over, even
// to a ready worker; select alone would pick the send at random.
func TestExpiredBoundHandsNothingOver(t *testing.T) {
	calls := make(chan api.Call[int, int])
	var taken atomic.Int64
	go func() {
		for c := range calls {
			taken.Add(1)
			c.Reply <- 1
		}
	}()
	cancelled := make(chan struct{})
	close(cancelled)
	for i := range 200 {
		b := Bound{Shorten: cancelled}
		if i%2 == 1 {
			b = Bound{Until: time.Now().Add(-time.Millisecond)}
		}
		if _, err := Exchange("x", calls, api.Call[int, int]{ID: "id", Context: context.Background()}, b); !errors.Is(err, ErrNotTaken) {
			t.Fatalf("call %d: err %v, want ErrNotTaken", i, err)
		}
	}
	close(calls)
	if n := taken.Load(); n > 0 {
		t.Fatalf("%d of 200 calls were handed to the host after the bound had already ended", n)
	}
}

// TestClosedReplyIsNoAnswer: a worker that closes Reply instead of answering
// gave no answer; the zero value a closed channel yields must not be read as
// one (for an event it would mean "durable").
func TestClosedReplyIsNoAnswer(t *testing.T) {
	calls := make(chan api.Call[int, error])
	go func() {
		c := <-calls
		close(c.Reply)
	}()
	v, err := Exchange("x", calls, api.Call[int, error]{ID: "id", Context: context.Background()}, Bound{Until: time.Now().Add(5 * time.Second)})
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("closed reply read as answer %v (err %v), want ErrNoAnswer", v, err)
	}
}
