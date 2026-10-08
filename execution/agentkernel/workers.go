package agentkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// workers owns every host-side goroutine of one Execute. The kernel hands
// calls over and never runs host code itself (execution spec §4.1); the
// adapter is that host side, so it starts its workers before the engine runs
// and stops and joins them before Execute returns. No goroutine outlives the
// call, which is why every injected worker must honour its context.
type workers struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newWorkers() *workers {
	ctx, cancel := context.WithCancel(context.Background())
	return &workers{ctx: ctx, cancel: cancel}
}

// stop ends every worker between calls and waits for all of them. A worker
// in a call returns when the call's context ends, which the kernel cancels
// once it stops waiting.
func (w *workers) stop() {
	w.cancel()
	w.wg.Wait()
}

// serve starts one owned worker answering calls one at a time, the shape of
// api.Serve but joined by stop.
func serve[Q, R any](w *workers, handle func(context.Context, Q) R) chan<- api.Call[Q, R] {
	calls := make(chan api.Call[Q, R])
	w.wg.Go(func() {
		for {
			select {
			case <-w.ctx.Done():
				return
			case c := <-calls:
				c.Reply <- handle(c.Context, c.Request)
			}
		}
	})
	return calls
}

// clock is the watchdog's time source: time.Now (monotonic) and time.After
// in production, controlled in tests.
type clock struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

var systemClock = clock{now: time.Now, after: time.After}

// supervise translates the host's cancellation into the kernel's, and
// enforces the host's inactivity bound, on one owned goroutine. The kernel
// context is cancelled with a cause that keeps the host's own cause (so the
// host owner is derived from it, never from the kernel) and carries the
// matching kernel provenance (so the kernel's record agrees).
//
// The inactivity timer is only a wake-up. When it fires the watchdog
// re-reads the last observed progress and cancels only if the silence since
// then really reached the limit; otherwise it re-arms for the remainder. A
// progress observation racing the expiry therefore wins whenever it was
// observed first, and time the host spends recording an event is never the
// provider's silence (attempt.inactive).
func supervise(w *workers, host context.Context, cancel context.CancelCauseFunc, limit time.Duration, s *attempt) {
	w.wg.Go(func() {
		var expired <-chan time.Time
		if limit > 0 {
			expired = s.clock.after(limit)
		}
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-host.Done():
				cancel(kernelCause(context.Cause(host)))
				return
			case <-expired:
				remaining, inactive := s.inactive(limit)
				if inactive {
					cancel(kernelCause(execution.ErrProviderInactive))
					return
				}
				expired = s.clock.after(remaining)
			}
		}
	})
}

// kernelCause joins the host cause with the kernel provenance it stands for.
// Inactivity has no kernel provenance and stays unknown there; the host
// cause still names it.
func kernelCause(cause error) error {
	provenance := api.CancelUnknown
	switch execution.OwnerOfCause(cause) {
	case execution.OwnerOperatorStop:
		provenance = api.CancelOperatorStop
	case execution.OwnerControllerShutdown:
		provenance = api.CancelControllerShutdown
	case execution.OwnerDeadline:
		provenance = api.CancelDeadline
	}
	return errors.Join(cause, api.Cancellation(provenance))
}

// attempt is what the workers of one invocation observe.
type attempt struct {
	ref        execution.AttemptRef
	transcript TranscriptWriter
	write      execution.ProgressWriter
	clock      clock

	mu sync.Mutex
	// lastProgress is when the last kernel event was observed (monotonic),
	// and recording counts events being written by the host right now.
	lastProgress time.Time
	recording    int
	lastKey      string
	events       int64
	calls        int
	responses    int
	lastErr      *api.ProviderError
}

func newAttempt(ref execution.AttemptRef, transcript TranscriptWriter, write execution.ProgressWriter, c clock) *attempt {
	return &attempt{ref: ref, transcript: transcript, write: write, clock: c, lastProgress: c.now()}
}

// progressWriter is the host's fallible progress writer. A host that supplied
// only the best-effort recorder gets it wrapped never to fail, which is all
// that recorder ever promised; one that supplied neither gets nil.
func progressWriter(ctx context.Context) execution.ProgressWriter {
	if write := execution.ProgressWriterFrom(ctx); write != nil {
		return write
	}
	if record := execution.ProgressRecorder(ctx); record != nil {
		return func(p execution.Progress) error { record(p); return nil }
	}
	return nil
}

// inactive reports whether the provider has been silent for the whole limit,
// or else how long until it would be. While the host is recording an event
// the kernel is waiting on the host, not on the provider, so that time is
// never silence; the clock restarts when the write ends.
func (s *attempt) inactive(limit time.Duration) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recording > 0 {
		return limit, false
	}
	silence := s.clock.now().Sub(s.lastProgress)
	if silence >= limit {
		return 0, true
	}
	return limit - silence, false
}

// observe marks progress the instant a kernel event is received, before the
// transcript write that may then fail or be slow on its own.
func (s *attempt) observe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProgress = s.clock.now()
	s.recording++
}

func (s *attempt) recorded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProgress = s.clock.now()
	s.recording--
}

// recordEvent is the event sink. Progress is observed on receipt; the
// transcript write and then the host progress write are the durability the
// kernel waits for, and either failure is returned, so the kernel settles
// recording_failed and takes no further side effect. Both writes are host
// time, never provider silence (attempt.inactive).
func (s *attempt) recordEvent(ctx context.Context, ev api.Event) error {
	s.observe()
	defer s.recorded()
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event %d: %w", ev.Seq, err)
	}
	if err := s.transcript.WriteEvent(ctx, s.ref, line); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	key := fmt.Sprintf("agentkernel:%d:%s", ev.Seq, ev.Kind)
	s.mu.Lock()
	s.lastKey = key
	s.events++
	s.mu.Unlock()
	if s.write == nil {
		return nil
	}
	if err := s.write(execution.Progress{Key: key, Age: max(time.Since(ev.ObservedAt), 0)}); err != nil {
		return fmt.Errorf("progress: %w", err)
	}
	return nil
}

// recordFinal is the recorder's closing write once the execution settled.
// Its error is the caller's: nothing is left to settle, so it can only make
// the host result incomplete (finalProgressFailed).
func (s *attempt) recordFinal() error {
	s.mu.Lock()
	key := s.lastKey
	s.mu.Unlock()
	if s.write == nil || key == "" {
		return nil
	}
	return s.write(execution.Progress{Key: key, Final: true})
}

// complete serves the provider and keeps the last typed failure, which is
// the class the kernel's provider termination stands for.
func (s *attempt) complete(p api.Provider) func(context.Context, api.ProviderRequest) api.Reply[api.ProviderResponse] {
	return func(ctx context.Context, req api.ProviderRequest) api.Reply[api.ProviderResponse] {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		resp, err := p.Complete(ctx, req)
		var perr *api.ProviderError
		switch {
		case err == nil:
		case errors.As(err, &perr):
		default:
			perr = &api.ProviderError{Class: api.ProviderTransport, Detail: "untyped provider error: " + err.Error()}
		}
		s.mu.Lock()
		s.lastErr = perr
		if err == nil {
			s.responses++
		}
		s.mu.Unlock()
		return api.Reply[api.ProviderResponse]{Value: resp, Err: err}
	}
}

// runCommand serves granted commands through the host's process boundary,
// bound to this invocation's identity and scratch.
func (s *attempt) runCommand(runner CommandRunner, scratch string) func(context.Context, api.CommandRequest) api.Reply[api.CommandResult] {
	return func(ctx context.Context, req api.CommandRequest) api.Reply[api.CommandResult] {
		out, err := runner.Run(ctx, Command{Attempt: s.ref, Argv: req.Argv, Dir: req.Dir, ScratchDir: scratch, Timeout: req.Timeout})
		return api.Reply[api.CommandResult]{
			Value: api.CommandResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr, Truncated: out.Truncated},
			Err:   err,
		}
	}
}

// observed is a consistent read of what the workers saw.
func (s *attempt) observed() (perr *api.ProviderError, reached, executed bool, events int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr, s.calls > 0, s.responses > 0, s.events
}
