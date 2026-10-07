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

	reportedIn, reportedOut, reportedCached int64
	missingIn, missingOut, missingCached    bool
	estimatedIn, estimatedOut               int64

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
	u.Unknowns = append(u.Unknowns, "estimated.cached_input")
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

// actualCost prices reported usage. It is known only with a trusted rate card
// and every count reported; otherwise the call contributes nothing, so the
// account's micros stay a lower bound.
func actualCost(u api.TokenUsage, p *api.Pricing) (int64, bool) {
	if p == nil || u.Input == nil || u.Output == nil || u.CachedInput == nil {
		return 0, false
	}
	uncached := max(*u.Input-*u.CachedInput, 0)
	return price(float64(uncached)*float64(p.InputMicrosPerMillion) +
		float64(*u.CachedInput)*float64(p.CachedInputMicrosPerMillion) +
		float64(*u.Output)*float64(p.OutputMicrosPerMillion)), true
}

// worstCaseCost prices the most a call can cost: every input token at the
// uncached rate and the full output allowance. Unpriced bindings cost 0 here
// because validation admits a money ceiling only when every eligible binding
// is priced in its currency.
func worstCaseCost(inputTokens, outputTokens int64, p *api.Pricing) int64 {
	if p == nil {
		return 0
	}
	return price(float64(inputTokens)*float64(p.InputMicrosPerMillion) + float64(outputTokens)*float64(p.OutputMicrosPerMillion))
}

// price converts token-micros-per-million to micros, rounding up so a bound
// is never understated.
// ponytail: float64 is exact below 2^53 token-micros; integer 128-bit math if
// rate cards ever approach that.
func price(tokenMicrosPerMillion float64) int64 {
	return int64(math.Ceil(tokenMicrosPerMillion / 1e6))
}
