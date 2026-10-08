package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// reservation is what one provider call holds of the ledger.
type reservation struct {
	in, out, money int64
	// estIn is the observation-only input estimate.
	estIn int64
}

func (h reservation) amounts() []amount {
	return []amount{{api.DimensionInputTokens, h.in}, {api.DimensionOutputTokens, h.out}, {api.DimensionMoney, h.money}}
}

// complete hands one model call to the binding's host worker and waits
// within providerBound. The call's context is the run's: it ends at the
// budget deadline or the host's cancellation.
func (r *run) complete(ctx context.Context, c call, req api.ProviderRequest) (api.ProviderResponse, error) {
	r.providerCalls++
	id := fmt.Sprintf("%s/%s/provider-%d", r.req.ExecutionID, r.req.AttemptID, r.providerCalls)
	call := api.ProviderCall{ID: id, Context: ctx, Request: req}
	reply, err := handoff.Exchange("provider "+c.binding.ID, c.calls, call, r.providerBound())
	if err != nil {
		return api.ProviderResponse{}, err
	}
	return reply.Value, reply.Err
}

// callFailed settles a call that produced no response and classifies it.
//
//   - Never taken: the provider never saw it, so every reservation is
//     released and no call is counted.
//   - Taken, not answered: it may have consumed anything it was allowed, so
//     every reservation stays charged.
//   - Failed: input stays charged at its reservation, money keeps the worst
//     case, output is released.
func (r *run) callFailed(c call, held reservation, err error, latency time.Duration) *api.ProviderError {
	switch {
	case errors.Is(err, handoff.ErrNotTaken):
		for _, a := range held.amounts() {
			r.ledger.settle(a.dim, a.n, 0)
		}
		return r.unanswered(err)
	case errors.Is(err, handoff.ErrNoAnswer):
		r.count(func(a *account) { a.recordCall(held.estIn, 0, nil, c.binding.Pricing, latency) })
		return r.unanswered(err)
	}
	r.ledger.settle(api.DimensionOutputTokens, held.out, 0)
	r.count(func(a *account) { a.recordCall(held.estIn, 0, nil, c.binding.Pricing, latency) })
	return asProviderError(err)
}

// unanswered types a hand-off that ended at the bound. The bound is the
// deadline or the grace after a host cancellation, so the turn then settles
// on whichever interrupted it.
func (r *run) unanswered(err error) *api.ProviderError {
	class := api.ProviderDeadline
	if r.parent.Err() != nil {
		class = api.ProviderCancelled
	}
	return &api.ProviderError{Class: class, Detail: err.Error()}
}
