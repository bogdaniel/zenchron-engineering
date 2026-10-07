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

// supervise translates the host's cancellation into the kernel's, and
// enforces the host's inactivity bound, on one owned goroutine. The kernel
// context is cancelled with a cause that keeps the host's own cause (so the
// host owner is derived from it, never from the kernel) and carries the
// matching kernel provenance (so the kernel's record agrees).
func supervise(w *workers, host context.Context, cancel context.CancelCauseFunc, limit time.Duration, progress <-chan struct{}) {
	w.wg.Go(func() {
		var expired <-chan time.Time
		var timer *time.Timer
		if limit > 0 {
			timer = time.NewTimer(limit)
			defer timer.Stop()
			expired = timer.C
		}
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-host.Done():
				cancel(kernelCause(context.Cause(host)))
				return
			case <-expired:
				cancel(kernelCause(execution.ErrProviderInactive))
				return
			case <-progress:
				if timer != nil {
					timer.Reset(limit)
				}
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
	record     func(execution.Progress)
	progress   chan struct{}

	mu        sync.Mutex
	lastKey   string
	events    int64
	calls     int
	responses int
	lastErr   *api.ProviderError
}

func newAttempt(ref execution.AttemptRef, transcript TranscriptWriter, record func(execution.Progress)) *attempt {
	return &attempt{ref: ref, transcript: transcript, record: record, progress: make(chan struct{}, 1)}
}

// recordEvent is the event sink. The transcript write is the durability the
// kernel waits for: its failure is returned, so the kernel settles
// recording_failed and takes no further side effect. Only then is progress
// recorded and the inactivity bound renewed.
func (s *attempt) recordEvent(ctx context.Context, ev api.Event) error {
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
	if s.record != nil {
		s.record(execution.Progress{Key: key, Age: max(time.Since(ev.ObservedAt), 0)})
	}
	select {
	case s.progress <- struct{}{}:
	default:
	}
	return nil
}

// recordFinal is the recorder's closing write once the execution settled.
func (s *attempt) recordFinal() {
	s.mu.Lock()
	key := s.lastKey
	s.mu.Unlock()
	if s.record != nil && key != "" {
		s.record(execution.Progress{Key: key, Final: true})
	}
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
