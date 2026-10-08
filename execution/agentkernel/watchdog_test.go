package agentkernel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

const testLimit = time.Minute

// bounded is how long a test waits for the watchdog before failing loudly.
const bounded = 5 * time.Second

// fakeClock is a clock the test moves by hand; every timer the watchdog arms
// is handed to the test, which fires it explicitly.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers chan fakeTimer
}

type fakeTimer struct {
	d    time.Duration
	fire chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_000_000, 0), timers: make(chan fakeTimer, 8)}
}

func (c *fakeClock) clock() clock {
	return clock{
		now: func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.t
		},
		after: func(d time.Duration) <-chan time.Time {
			fire := make(chan time.Time, 1)
			c.timers <- fakeTimer{d: d, fire: fire}
			return fire
		},
	}
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) armed(t *testing.T) fakeTimer {
	t.Helper()
	select {
	case timer := <-c.timers:
		return timer
	case <-time.After(bounded):
		t.Fatal("the watchdog armed no timer")
		return fakeTimer{}
	}
}

// watchdog starts supervise over a fresh attempt on a fake clock.
func watchdog(t *testing.T, w TranscriptWriter) (*fakeClock, *attempt, context.Context) {
	t.Helper()
	fc := newFakeClock()
	s := newAttempt(execution.AttemptRef{RunID: "r", OperationID: "o", Attempt: 1}, w, nil, fc.clock())
	workers := newWorkers()
	kctx, cancel := context.WithCancelCause(context.Background())
	supervise(workers, context.Background(), cancel, testLimit, s)
	t.Cleanup(func() { workers.stop(); cancel(nil) })
	return fc, s, kctx
}

func assertLive(t *testing.T, kctx context.Context, why string) {
	t.Helper()
	if kctx.Err() != nil {
		t.Fatalf("%s: the watchdog cancelled the execution (%v)", why, context.Cause(kctx))
	}
}

func assertInactive(t *testing.T, kctx context.Context) {
	t.Helper()
	select {
	case <-kctx.Done():
	case <-time.After(bounded):
		t.Fatal("genuine silence was not cancelled")
	}
	if !errors.Is(context.Cause(kctx), execution.ErrProviderInactive) {
		t.Fatalf("cancelled with %v, want ErrProviderInactive", context.Cause(kctx))
	}
}

// (a) Progress observed exactly at the inactivity edge, with the expiry
// already runnable, wins: the watchdog re-reads it and re-arms.
func TestProgressObservedAtTheEdgeWins(t *testing.T) {
	fc, s, kctx := watchdog(t, &transcript{})
	first := fc.armed(t)
	fc.advance(testLimit)
	if err := s.recordEvent(context.Background(), api.Event{Seq: 1, Kind: api.EventProviderResponse}); err != nil {
		t.Fatal(err)
	}
	first.fire <- time.Time{}
	rearmed := fc.armed(t)
	assertLive(t, kctx, "progress at the edge")
	if rearmed.d != testLimit {
		t.Errorf("re-armed for %v, want the full %v after progress at the edge", rearmed.d, testLimit)
	}
	fc.advance(testLimit)
	rearmed.fire <- time.Time{}
	assertInactive(t, kctx)
}

// blockingTranscript holds every write until released.
type blockingTranscript struct {
	entered, release chan struct{}
}

func (b blockingTranscript) WriteEvent(context.Context, execution.AttemptRef, []byte) error {
	close(b.entered)
	<-b.release
	return nil
}

// (b) A healthy transcript write slower than the limit is the host's time,
// never the provider's silence.
func TestSlowTranscriptWriteIsNotInactivity(t *testing.T) {
	slow := blockingTranscript{entered: make(chan struct{}), release: make(chan struct{})}
	fc, s, kctx := watchdog(t, slow)
	first := fc.armed(t)
	written := make(chan error, 1)
	go func() {
		written <- s.recordEvent(context.Background(), api.Event{Seq: 1, Kind: api.EventProviderRequest})
	}()
	<-slow.entered
	fc.advance(3 * testLimit)
	first.fire <- time.Time{}
	during := fc.armed(t)
	assertLive(t, kctx, "a write in flight for 3x the limit")
	close(slow.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	during.fire <- time.Time{}
	fc.armed(t)
	assertLive(t, kctx, "right after a slow write completed")
}

// (c) Genuine silence for the whole limit is inactivity.
func TestGenuineSilenceIsInactivity(t *testing.T) {
	fc, _, kctx := watchdog(t, &transcript{})
	first := fc.armed(t)
	fc.advance(testLimit)
	first.fire <- time.Time{}
	assertInactive(t, kctx)
}

// expiringProvider answers only after the watchdog has expired the attempt,
// so completion and expiry are concurrent.
type expiringProvider struct {
	t  *testing.T
	fc *fakeClock
}

func (p expiringProvider) Complete(ctx context.Context, _ api.ProviderRequest) (api.ProviderResponse, error) {
	p.fc.advance(2 * testLimit)
	p.fc.armed(p.t).fire <- time.Time{}
	select {
	case <-ctx.Done():
	case <-time.After(bounded):
		return api.ProviderResponse{}, errors.New("the watchdog never expired the attempt")
	}
	return api.ProviderResponse{Text: "finished", Stop: api.StopEnd}, nil
}

// (d) A completion the kernel observed is preserved even though the
// inactivity expiry fired concurrently: settlement happens once.
func TestCompletionConcurrentWithExpiryIsPreserved(t *testing.T) {
	fc := newFakeClock()
	a := newTestAdapter(t, testConfig(t, expiringProvider{t: t, fc: fc}))
	a.clock = fc.clock()
	req := testRequest(t)
	req.Budgets.InactivityLimit = testLimit
	res, err := a.Execute(t.Context(), req)
	if err != nil || res.Outcome != execution.Succeeded || res.Answer != "finished" ||
		res.Invocation.TerminationCause != string(execution.OwnerProviderExited) {
		t.Errorf("observed completion rewritten by the expiry: %+v, %v", res, err)
	}
}
