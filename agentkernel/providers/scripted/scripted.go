// Package scripted is a deterministic in-process Provider for offline tests
// and failure injection: it replays a fixed script of responses and errors
// and records every request it receives.
package scripted

import (
	"context"
	"errors"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Step is one scripted call outcome.
type Step struct {
	Response api.ProviderResponse
	// Err, when set, is returned instead of Response.
	Err error
	// Block makes the call wait until its context ends, then return the
	// matching cancelled/deadline ProviderError.
	Block bool
	// Entered, when set, is closed as the call starts, so a test can act while
	// the call is in flight without sleeping.
	Entered chan struct{}
	// Before, when set, runs just before the step's outcome is returned; a test
	// uses it to cancel at an exact point (after the provider has answered).
	Before func()
}

// Provider replays steps in order. It is safe for concurrent use; concurrent
// callers each consume the next step.
type Provider struct {
	mu       sync.Mutex
	steps    []Step
	next     int
	requests []api.ProviderRequest
}

// New returns a provider that answers with steps in order.
func New(steps ...Step) *Provider { return &Provider{steps: steps} }

// Complete consumes the next step. Calls beyond the script fail as rejected
// so an over-running loop is visible rather than silently answered.
func (p *Provider) Complete(ctx context.Context, req api.ProviderRequest) (api.ProviderResponse, error) {
	step, ok := p.take(req)
	if !ok {
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderRejected, Detail: "scripted provider: script exhausted"}
	}
	if step.Entered != nil {
		close(step.Entered)
	}
	if step.Block {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderDeadline, Detail: "deadline exceeded"}
		}
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderCancelled, Detail: "request cancelled"}
	}
	if step.Before != nil {
		step.Before()
	}
	if step.Err != nil {
		return api.ProviderResponse{}, step.Err
	}
	return step.Response, nil
}

func (p *Provider) take(req api.ProviderRequest) (Step, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.next >= len(p.steps) {
		return Step{}, false
	}
	step := p.steps[p.next]
	p.next++
	return step, true
}

// Requests returns a copy of every request received, in arrival order.
func (p *Provider) Requests() []api.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.ProviderRequest(nil), p.requests...)
}
