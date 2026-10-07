package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	kcontext "github.com/bogdaniel/zenchron-engineering/agentkernel/context"
)

// call is the bound provider side of one execution.
type call struct {
	provider api.Provider
	binding  api.ProviderBinding
	specs    []api.ToolSpec
	estimate kcontext.Estimator
}

// loop alternates provider turns and tool rounds until the provider stops
// proposing tools or a bound, a failure or a cancellation ends it.
func (r *run) loop(ctx context.Context, c call, messages []api.Message) api.Termination {
	for {
		if t, stop := r.interrupted(ctx); stop {
			return t
		}
		if _, ok := r.ledger.reserve(amount{api.DimensionIterations, 1}); !ok {
			return r.exhausted(api.DimensionIterations, "no model turns remain")
		}
		r.count(func(a *account) { a.iterations++ })
		resp, t, ok := r.turn(ctx, c, messages)
		if !ok {
			return t
		}
		messages = append(messages, api.Message{Role: api.RoleAssistant, Content: resp.Text, ToolCalls: resp.ToolCalls, Replay: resp.Replay})
		switch {
		case resp.Stop == api.StopRefused:
			return r.termination(api.OutcomeFailed, api.CauseProviderRefused, "provider refused")
		case len(resp.ToolCalls) > 0:
			results, t, ok := r.toolRound(ctx, resp.ToolCalls)
			if !ok {
				return t
			}
			messages = append(messages, results...)
		case resp.Stop == api.StopMaxTokens:
			// The per-call output bound is min(binding limit, remaining
			// execution output budget); either way the host envelope ended
			// the output, so this is output-token exhaustion.
			return r.exhausted(api.DimensionOutputTokens, "provider stopped at the output-token bound")
		case resp.Stop != api.StopEnd:
			// An incomplete, paused or unrecognized stop is not a finished
			// loop; reporting it as completed would overstate what ran.
			return r.termination(api.OutcomeFailed, api.CauseProviderFailed,
				"provider stopped without completing: "+string(resp.Stop))
		default:
			r.finalText = resp.Text
			return r.termination(api.OutcomeCompleted, api.CauseLoopCompleted, "provider stopped proposing tools")
		}
	}
}

// interrupted checks, before each side effect, whether the execution must
// stop: a recording failure, a host cancellation or the budget deadline.
// The host's own cancellation is distinguished from the kernel's deadline.
func (r *run) interrupted(ctx context.Context) (api.Termination, bool) {
	if r.recordingFailure() != nil {
		return r.recordingFailed(""), true
	}
	if r.parent.Err() != nil {
		t := r.termination(api.OutcomeCancelled, api.CauseHostCancelled, "host cancelled the execution")
		t.Cancellation = api.CancellationOf(r.parent)
		return t, true
	}
	if ctx.Err() != nil || !r.e.clock.Now().Before(r.req.Budget.Deadline) {
		return r.exhausted(api.DimensionDeadline, "budget deadline reached"), true
	}
	return api.Termination{}, false
}

func (r *run) count(f func(*account)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.acct)
}

// turn makes one model turn, retrying a retryable failure only while the
// retry allowance lasts. Every attempt reserves afresh: a retry never renews
// the token, money or iteration budget.
func (r *run) turn(ctx context.Context, c call, messages []api.Message) (api.ProviderResponse, api.Termination, bool) {
	for attempt := 0; ; attempt++ {
		resp, perr, t, ok := r.attempt(ctx, c, messages, attempt)
		if perr == nil {
			return resp, t, ok
		}
		if perr.Class == api.ProviderCancelled || perr.Class == api.ProviderDeadline {
			if t, stop := r.interrupted(ctx); stop {
				return api.ProviderResponse{}, t, false
			}
		}
		if perr.Retryable {
			if _, ok := r.ledger.reserve(amount{api.DimensionRetries, 1}); ok {
				r.count(func(a *account) { a.retries++ })
				continue
			}
		}
		detail := fmt.Sprintf("%s after %d attempt(s): %s", perr.Class, attempt+1, perr.Detail)
		if perr.Class == api.ProviderRateLimited || perr.Class == api.ProviderUnavailable {
			// Returned to the host as a typed observation; the kernel never
			// waits or polls for capacity.
			return api.ProviderResponse{}, r.termination(api.OutcomeBlocked, api.CauseProviderUnavailable, detail), false
		}
		return api.ProviderResponse{}, r.termination(api.OutcomeFailed, api.CauseProviderFailed, detail), false
	}
}

// attempt is one provider call. It returns a provider error to classify, or
// a final (termination, ok) when the attempt itself ended the turn.
func (r *run) attempt(ctx context.Context, c call, messages []api.Message, n int) (api.ProviderResponse, *api.ProviderError, api.Termination, bool) {
	fail := func(t api.Termination) (api.ProviderResponse, *api.ProviderError, api.Termination, bool) {
		return api.ProviderResponse{}, nil, t, false
	}
	if t, stop := r.interrupted(ctx); stop {
		return fail(t)
	}
	prompt := promptText(messages, c.specs)
	estIn := c.estimate(prompt).Count
	maxOut := min(c.binding.MaxOutputTokens, r.ledger.remaining(api.DimensionOutputTokens))
	if maxOut <= 0 {
		return fail(r.exhausted(api.DimensionOutputTokens, "no output tokens remain"))
	}
	if estIn > c.binding.ContextWindow-maxOut {
		return fail(r.exhausted(api.DimensionInputTokens, "transcript no longer fits the context window"))
	}
	// Money is reserved at a true worst case: the provider cannot accept more
	// input than its context window. A local estimate is not a bound, and a
	// hard ceiling must not rest on one.
	money := worstCaseCost(c.binding.ContextWindow, maxOut, c.binding.Pricing)
	reservation := []amount{{api.DimensionInputTokens, estIn}, {api.DimensionOutputTokens, maxOut}, {api.DimensionMoney, money}}
	if dim, ok := r.ledger.reserve(reservation...); !ok {
		return fail(r.exhausted(dim, "the next model turn cannot be reserved"))
	}
	ev := api.Event{Kind: api.EventProviderRequest, Detail: fmt.Sprintf("binding %s attempt %d max_output %d", c.binding.ID, n+1, maxOut)}
	if err := r.emit(ctx, ev); err != nil {
		return fail(r.recordingFailed(""))
	}
	start := r.e.clock.Now()
	resp, err := c.provider.Complete(ctx, api.ProviderRequest{Binding: c.binding, Messages: messages, Tools: c.specs, MaxOutputTokens: maxOut})
	latency := r.e.clock.Now().Sub(start)
	if err != nil {
		// Usage of a failed call is unknown: input stays charged at the
		// estimate, money keeps the worst case, output is released.
		r.ledger.settle(api.DimensionOutputTokens, maxOut, 0)
		r.count(func(a *account) { a.recordCall(estIn, 0, nil, c.binding.Pricing, latency) })
		perr := asProviderError(err)
		if rerr := r.emit(ctx, api.Event{Kind: api.EventProviderError, Detail: perr.Error()}); rerr != nil {
			return fail(r.recordingFailed("provider error " + string(perr.Class)))
		}
		return api.ProviderResponse{}, perr, api.Termination{}, false
	}
	resp.Usage = plausibleUsage(resp.Usage)
	r.settleCall(c, resp, estIn, maxOut, money, latency)
	if err := r.emit(ctx, api.Event{Kind: api.EventProviderResponse, Detail: "stop " + string(resp.Stop), Usage: &resp.Usage}); err != nil {
		return fail(r.recordingFailed("provider responded with stop " + string(resp.Stop)))
	}
	return resp, nil, api.Termination{}, true
}

// settleCall replaces the worst-case reservation with reported usage where it
// exists. Unreported output keeps its full reservation; unpriced or partly
// reported usage keeps the worst-case money charge.
func (r *run) settleCall(c call, resp api.ProviderResponse, estIn, maxOut, money int64, latency time.Duration) {
	u := resp.Usage
	if u.Input != nil {
		r.ledger.settle(api.DimensionInputTokens, estIn, *u.Input)
	}
	if u.Output != nil {
		r.ledger.settle(api.DimensionOutputTokens, maxOut, *u.Output)
	}
	if actual, known := actualCost(u, c.binding.Pricing); known {
		r.ledger.settle(api.DimensionMoney, money, actual)
	}
	estOut := c.estimate(resp.Text + promptText([]api.Message{{ToolCalls: resp.ToolCalls}}, nil)).Count
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acct.recordCall(estIn, estOut, &u, c.binding.Pricing, latency)
	if resp.ModelVersionObserved != "" {
		r.provenance.ModelVersionObserved = resp.ModelVersionObserved
	}
	if resp.Session != nil {
		r.provenance.Sessions = append(r.provenance.Sessions, *resp.Session)
	}
}

// asProviderError keeps a typed error and wraps anything else as a
// non-retryable transport failure: an adapter that broke its contract is not
// retried on the guess that it might be transient.
func asProviderError(err error) *api.ProviderError {
	var perr *api.ProviderError
	if errors.As(err, &perr) {
		return perr
	}
	return &api.ProviderError{Class: api.ProviderTransport, Detail: "untyped provider error: " + err.Error()}
}

// plausibleUsage drops negative reported counts to unknown. Settling a
// negative count would credit the ledger and renew budget the host bounded.
func plausibleUsage(u api.TokenUsage) api.TokenUsage {
	for _, c := range []**int64{&u.Input, &u.Output, &u.CachedInput, &u.CacheWriteInput} {
		if *c != nil && **c < 0 {
			*c = nil
		}
	}
	return u
}
