package engine

import (
	"math"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// account accumulates what one execution used. Provider-reported counts and
// local estimates are kept apart; a reported total is stated only when every
// call reported that count.
type account struct {
	calls, toolCalls, iterations, retries int
	artifactBytes                         int64
	latency                               time.Duration

	reportedIn, reportedOut, reportedCached, reportedWrite int64
	missingIn, missingOut, missingCached, missingWrite     bool
	estimatedIn, estimatedOut                              int64

	costMicros int64
	costKnown  bool
	pricing    *api.Pricing
}

// recordCall adds one provider call. usage is nil for a failed call, whose
// usage is unknown.
func (a *account) recordCall(estIn, estOut int64, usage *api.TokenUsage, pricing *api.Pricing, latency time.Duration) {
	a.calls++
	a.latency += latency
	a.estimatedIn += estIn
	a.estimatedOut += estOut
	a.pricing = pricing
	var u api.TokenUsage
	if usage != nil {
		u = *usage
	}
	addReported(&a.reportedIn, &a.missingIn, u.Input)
	addReported(&a.reportedOut, &a.missingOut, u.Output)
	addReported(&a.reportedCached, &a.missingCached, u.CachedInput)
	addReported(&a.reportedWrite, &a.missingWrite, u.CacheWriteInput)
	micros, known := actualCost(u, pricing)
	a.costMicros += micros
	a.costKnown = a.costKnown && known
}

func addReported(sum *int64, missing *bool, n *int64) {
	if n == nil {
		*missing = true
		return
	}
	*sum += *n
}

func (a *account) usage() api.Usage {
	u := api.Usage{
		ProviderCalls: a.calls, ToolCalls: a.toolCalls, Iterations: a.iterations, Retries: a.retries,
		ArtifactBytes: a.artifactBytes, LatencyMillis: a.latency.Milliseconds(),
		Estimated: api.TokenUsage{Input: api.Count(a.estimatedIn), Output: api.Count(a.estimatedOut)},
		Cost:      api.Cost{Known: a.costKnown && a.calls > 0, Micros: a.costMicros},
	}
	u.Reported.Input = reported(a.reportedIn, a.missingIn, &u.Unknowns, "reported.input")
	u.Reported.Output = reported(a.reportedOut, a.missingOut, &u.Unknowns, "reported.output")
	u.Reported.CachedInput = reported(a.reportedCached, a.missingCached, &u.Unknowns, "reported.cached_input")
	u.Reported.CacheWriteInput = reported(a.reportedWrite, a.missingWrite, &u.Unknowns, "reported.cache_write_input")
	u.Unknowns = append(u.Unknowns, "estimated.cached_input", "estimated.cache_write_input")
	if a.pricing != nil {
		u.Cost.Currency, u.Cost.RateSource, u.Cost.RateVersion = a.pricing.Currency, a.pricing.Source, a.pricing.Version
	}
	if !u.Cost.Known {
		u.Unknowns = append(u.Unknowns, "cost")
	}
	return u
}

func reported(sum int64, missing bool, unknowns *[]string, name string) *int64 {
	if missing {
		*unknowns = append(*unknowns, name)
		return nil
	}
	return api.Count(sum)
}

// actualCost prices reported usage: input tokens are split into cache reads,
// cache writes and the rest, each at its own rate. It is known only with a
// trusted rate card and every count it needs. A missing cache count is not
// needed when its rate equals the input rate, because the split then cannot
// change the price. Otherwise the call contributes nothing, so the account's
// micros stay a lower bound.
func actualCost(u api.TokenUsage, p *api.Pricing) (int64, bool) {
	if p == nil || u.Input == nil || u.Output == nil {
		return 0, false
	}
	cached, okCached := part(u.CachedInput, p.CachedInputMicrosPerMillion == p.InputMicrosPerMillion)
	written, okWritten := part(u.CacheWriteInput, p.CacheWriteInputMicrosPerMillion == p.InputMicrosPerMillion)
	if !okCached || !okWritten {
		return 0, false
	}
	plain := max(*u.Input-cached-written, 0)
	return price(float64(plain)*float64(p.InputMicrosPerMillion) +
		float64(cached)*float64(p.CachedInputMicrosPerMillion) +
		float64(written)*float64(p.CacheWriteInputMicrosPerMillion) +
		float64(*u.Output)*float64(p.OutputMicrosPerMillion)), true
}

// part is a reported sub-count of input, or 0 when it is unknown but priced
// like plain input.
func part(n *int64, pricedAsInput bool) (int64, bool) {
	if n != nil {
		return *n, true
	}
	return 0, pricedAsInput
}

// worstCaseCost prices the most a call can cost: every input token at the
// highest input-side rate and the full output allowance. Unpriced bindings
// cost 0 here because validation admits a money ceiling only when every
// eligible binding is priced in its currency.
func worstCaseCost(inputTokens, outputTokens int64, p *api.Pricing) int64 {
	if p == nil {
		return 0
	}
	inRate := max(p.InputMicrosPerMillion, p.CachedInputMicrosPerMillion, p.CacheWriteInputMicrosPerMillion)
	return price(float64(inputTokens)*float64(inRate) + float64(outputTokens)*float64(p.OutputMicrosPerMillion))
}

// price converts token-micros-per-million to micros, rounding up so a bound
// is never understated.
// ponytail: float64 is exact below 2^53 token-micros; integer 128-bit math if
// rate cards ever approach that.
func price(tokenMicrosPerMillion float64) int64 {
	return int64(math.Ceil(tokenMicrosPerMillion / 1e6))
}
