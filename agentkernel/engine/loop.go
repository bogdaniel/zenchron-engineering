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
	calls   chan<- api.ProviderCall
	binding api.ProviderBinding
	specs   []api.ToolSpec
}

// loop alternates provider turns and tool rounds until the provider stops
// proposing tools or a bound, a failure or a cancellation ends it.
func (r *run) loop(ctx context.Context, c call, messages []api.Message) api.Termination {
	for {
		if t, stop := r.interrupted(ctx); stop {
			return t
		}
		if _, err := r.ledger.reserve(amount{api.DimensionIterations, 1}); err != nil {
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
	// The wall-clock comparison closes a timer race: a hand-off that stopped
	// waiting at the deadline can return before ctx's own deadline timer
	// has fired, and the run must not take another step in that gap.
	wallPassed := !time.Now().Before(r.req.Budget.Deadline)
	if ctx.Err() != nil || wallPassed || !r.e.clock.Now().Before(r.req.Budget.Deadline) {
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
			if _, err := r.ledger.reserve(amount{api.DimensionRetries, 1}); err == nil {
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
	est := kcontext.ApproximateTokens(prompt)
	maxOut := min(c.binding.MaxOutputTokens, r.ledger.remaining(api.DimensionOutputTokens))
	if maxOut <= 0 {
		return fail(r.exhausted(api.DimensionOutputTokens, "no output tokens remain"))
	}
	// Fitting the context window is an estimate; the input budget is not.
	if est.Count > c.binding.ContextWindow-maxOut {
		return fail(r.exhausted(api.DimensionInputTokens, "transcript no longer fits the context window"))
	}
	in := inputReservation(prompt, len(messages)+len(c.specs))
	// Money is reserved at a true worst case: the provider cannot accept more
	// input than its context window. A local estimate is not a bound, and a
	// hard ceiling must not rest on one.
	money := worstCaseCost(c.binding.ContextWindow, maxOut, c.binding.Pricing)
	held := reservation{in: in, out: maxOut, money: money, estIn: est.Count}
	if dim, err := r.ledger.reserve(held.amounts()...); err != nil {
		if errors.Is(err, errNegativeAmount) {
			return fail(r.termination(api.OutcomeFailed, api.CauseProviderFailed,
				fmt.Sprintf("negative %s reservation refused", dim)))
		}
		return fail(r.exhausted(dim, "the next model turn cannot be reserved"))
	}
	ev := api.Event{Kind: api.EventProviderRequest, Detail: fmt.Sprintf("binding %s attempt %d max_output %d", c.binding.ID, n+1, maxOut)}
	if err := r.emit(ctx, ev); err != nil {
		return fail(r.recordingFailed(""))
	}
	start := r.e.clock.Now()
	resp, err := r.complete(ctx, c, api.ProviderRequest{Binding: c.binding, Messages: messages, Tools: c.specs, MaxOutputTokens: maxOut})
	latency := r.e.clock.Now().Sub(start)
	if err != nil {
		perr := r.callFailed(c, held, err, latency)
		if rerr := r.emit(ctx, api.Event{Kind: api.EventProviderError, Detail: perr.Error()}); rerr != nil {
			return fail(r.recordingFailed("provider error " + string(perr.Class)))
		}
		return api.ProviderResponse{}, perr, api.Termination{}, false
	}
	resp.Usage = plausibleUsage(resp.Usage)
	r.settleCall(c, resp, held, latency)
	if err := r.emit(ctx, api.Event{Kind: api.EventProviderResponse, Detail: "stop " + string(resp.Stop), Usage: &resp.Usage}); err != nil {
		return fail(r.recordingFailed("provider responded with stop " + string(resp.Stop)))
	}
	return resp, nil, api.Termination{}, true
}

// settleCall replaces the worst-case reservation with reported usage where it
// exists. Unreported input keeps its upper-bound reservation, unreported
// output its full reservation; unpriced or partly reported usage keeps the
// worst-case money charge. held.estIn is the observation-only estimate.
func (r *run) settleCall(c call, resp api.ProviderResponse, held reservation, latency time.Duration) {
	u := resp.Usage
	if u.Input != nil {
		r.ledger.settle(api.DimensionInputTokens, held.in, *u.Input)
	}
	if u.Output != nil {
		r.ledger.settle(api.DimensionOutputTokens, held.out, *u.Output)
	}
	if actual, known := actualCost(u, c.binding.Pricing); known {
		r.ledger.settle(api.DimensionMoney, held.money, actual)
	}
	// The output estimate is an observation only and never reaches the ledger.
	estOut := kcontext.ApproximateTokens(resp.Text + promptText([]api.Message{{ToolCalls: resp.ToolCalls}}, nil)).Count
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acct.recordCall(held.estIn, estOut, &u, c.binding.Pricing, latency)
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

// plausibleUsage drops implausible reported counts to unknown, so they can
// neither credit the ledger nor price a call below its worst case. A negative
// count is unknown. Cached and cache-write input are parts of input, so a
// negative input or part makes input and both parts unknown (an adapter may
// have folded the negative part into input already). When input is reported
// and a part exceeds it, or the known parts together do, the partition is
// impossible and input and both parts become unknown too.
func plausibleUsage(u api.TokenUsage) api.TokenUsage {
	if u.Output != nil && *u.Output < 0 {
		u.Output = nil
	}
	for _, c := range []*int64{u.Input, u.CachedInput, u.CacheWriteInput} {
		if c != nil && *c < 0 {
			u.Input, u.CachedInput, u.CacheWriteInput = nil, nil, nil
			return u
		}
	}
	if u.Input == nil {
		return u
	}
	cached, written := knownOrZero(u.CachedInput), knownOrZero(u.CacheWriteInput)
	// Compared by subtraction so huge counts cannot overflow past the check.
	if cached > *u.Input || written > *u.Input-cached {
		u.Input, u.CachedInput, u.CacheWriteInput = nil, nil, nil
	}
	return u
}

func knownOrZero(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
