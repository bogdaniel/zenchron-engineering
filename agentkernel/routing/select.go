// Package routing ranks providers deterministically inside the host-supplied
// eligible set. A score never makes a provider eligible.
package routing

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Observation is optional historical data about one provider. It may inform
// ranking among eligible bindings only.
type Observation struct {
	ProviderID    string
	MedianLatency time.Duration
	// CostMicrosPerCall is nil when unknown; unknown is never zero.
	CostMicrosPerCall *int64
	ObservedAt        time.Time
}

// Input is one routing decision.
type Input struct {
	Bindings                   []api.ProviderBinding
	RequireHostProvenIsolation bool
	RequiredFeatures           []string
	Observations               []Observation
	Now                        time.Time
	// StaleAfter marks observations older than this as stale.
	StaleAfter time.Duration
}

// ProviderFeatures are the required kernel features a provider binding must
// itself declare. The other kernel features (artifacts, cancellation
// provenance) are implemented by the kernel around any provider, so they do
// not filter bindings.
var ProviderFeatures = []string{api.FeatureTools}

// candidate is one binding's evaluation. Unknown cost and latency stay nil.
type candidate struct {
	binding   api.ProviderBinding
	permitted bool
	cost      *int64
	latency   *time.Duration
	reasons   []string
}

// Select returns the decision; Chosen is empty and Blocked set when no
// binding is permitted.
//
// Hard filters (host eligibility, isolation, provider features, pin) decide
// what may be chosen; ranking only orders what passed. Among permitted
// bindings: host Preference ascending, then known cost ascending with unknown
// after every known cost, then known latency ascending with unknown after,
// then ID. A pinned binding that fails a filter blocks the decision; there is
// no silent fallback to another provider.
func Select(in Input) api.RoutingDecision {
	pinned := pinnedID(in.Bindings)
	cands := make([]candidate, 0, len(in.Bindings))
	var permitted []api.ProviderBinding
	for _, b := range in.Bindings {
		c := candidate{binding: b}
		c.permitted = c.filter(in, pinned)
		if c.permitted {
			permitted = append(permitted, b)
		}
		cands = append(cands, c)
	}
	currency := commonCurrency(permitted)
	latest := latestObservations(in.Observations)
	for i := range cands {
		if cands[i].permitted {
			cands[i].rankingData(in, currency, latest)
		}
	}
	slices.SortFunc(cands, compareCandidates)
	decision := api.RoutingDecision{Candidates: make([]api.RoutingCandidate, 0, len(cands))}
	for i, c := range cands {
		rc := api.RoutingCandidate{ProviderID: c.binding.ID, Eligible: c.permitted, Reasons: c.reasons}
		if c.permitted {
			rc.Rank = i + 1
		}
		decision.Candidates = append(decision.Candidates, rc)
	}
	if len(cands) > 0 && cands[0].permitted {
		decision.Chosen = cands[0].binding.ID
		return decision
	}
	decision.Blocked = blockedText(pinned, cands)
	return decision
}

// filter applies every hard constraint and records each failure.
func (c *candidate) filter(in Input, pinned string) bool {
	b := c.binding
	if !b.Eligible {
		c.reasons = append(c.reasons, "excluded: host did not mark this binding eligible")
	}
	if pinned != "" && b.ID != pinned {
		c.reasons = append(c.reasons, fmt.Sprintf("excluded: binding %q is pinned", pinned))
	}
	if in.RequireHostProvenIsolation && b.Isolation != api.IsolationHostProven {
		c.reasons = append(c.reasons, fmt.Sprintf("excluded: isolation %q, host-proven isolation required", b.Isolation))
	}
	for _, f := range in.RequiredFeatures {
		if slices.Contains(ProviderFeatures, f) && !slices.Contains(b.Features, f) {
			c.reasons = append(c.reasons, fmt.Sprintf("excluded: lacks required feature %q", f))
		}
	}
	if len(c.reasons) > 0 {
		return false
	}
	c.reasons = append(c.reasons, fmt.Sprintf("permitted: preference %d", b.Preference))
	if b.ID == pinned {
		c.reasons = append(c.reasons, "pinned by host")
	}
	return true
}

// rankingData derives known cost and latency. A trusted rate card gives the
// worst-case price of one call at the binding's own token bounds; a fresh
// observation is used only when there is no comparable rate card. Stale
// observations are named stale and never used.
func (c *candidate) rankingData(in Input, currency string, latest map[string]Observation) {
	b := c.binding
	if b.Pricing != nil && currency == "" {
		c.reasons = append(c.reasons, "cost: rate cards use different currencies; not comparable")
	}
	if b.Pricing != nil && currency != "" {
		c.priceFromRateCard(currency)
	}
	obs, ok := latest[b.ID]
	if !ok {
		c.noteUnknown()
		return
	}
	age := in.Now.Sub(obs.ObservedAt)
	if in.StaleAfter <= 0 || age < 0 || age > in.StaleAfter {
		c.reasons = append(c.reasons, fmt.Sprintf("observation stale: observed %s, not used", obs.ObservedAt.UTC().Format(time.RFC3339)))
		c.noteUnknown()
		return
	}
	if c.cost == nil && obs.CostMicrosPerCall != nil {
		cost := *obs.CostMicrosPerCall
		c.cost = &cost
		c.reasons = append(c.reasons, fmt.Sprintf("cost: observed %d micros per call", cost))
	}
	if obs.MedianLatency > 0 {
		latency := obs.MedianLatency
		c.latency = &latency
		c.reasons = append(c.reasons, "latency: observed median "+latency.String())
	}
	c.noteUnknown()
}

// priceFromRateCard prices a full context window at the highest input-side
// rate plus maximum output. An unknown cached or cache-write rate leaves the
// cost unknown, because either may exceed the input rate. A price too large
// to represent stays unknown rather than wrapping to a small number.
func (c *candidate) priceFromRateCard(currency string) {
	b := c.binding
	inRate := b.Pricing.InputMicrosPerMillion
	for _, rate := range []*int64{b.Pricing.CachedInputMicrosPerMillion, b.Pricing.CacheWriteInputMicrosPerMillion} {
		if rate == nil {
			c.reasons = append(c.reasons, "cost: rate card lacks a cached or cache-write rate; worst case unknown")
			return
		}
		inRate = max(inRate, *rate)
	}
	worst := (float64(b.ContextWindow)*float64(inRate) +
		float64(b.MaxOutputTokens)*float64(b.Pricing.OutputMicrosPerMillion)) / 1e6
	if worst < 0 || worst >= math.MaxInt64 {
		c.reasons = append(c.reasons, "cost: rate card price out of range; not comparable")
		return
	}
	cost := int64(math.Ceil(worst))
	c.cost = &cost
	c.reasons = append(c.reasons, fmt.Sprintf("cost: at most %d %s micros per call (rate card %s %s)",
		cost, currency, b.Pricing.Source, b.Pricing.Version))
}

func (c *candidate) noteUnknown() {
	if c.cost == nil {
		c.reasons = append(c.reasons, "cost: unknown (ranked after known costs)")
	}
	if c.latency == nil {
		c.reasons = append(c.reasons, "latency: unknown (ranked after known latencies)")
	}
}

func compareCandidates(a, b candidate) int {
	if a.permitted != b.permitted {
		if a.permitted {
			return -1
		}
		return 1
	}
	if !a.permitted {
		return cmp.Compare(a.binding.ID, b.binding.ID)
	}
	return cmp.Or(
		cmp.Compare(a.binding.Preference, b.binding.Preference),
		compareKnown(a.cost, b.cost),
		compareKnown(a.latency, b.latency),
		cmp.Compare(a.binding.ID, b.binding.ID),
	)
}

// compareKnown orders known values ascending and every unknown after them.
func compareKnown[T cmp.Ordered](a, b *T) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*a, *b)
}

func pinnedID(bindings []api.ProviderBinding) string {
	for _, b := range bindings {
		if b.Pinned {
			return b.ID
		}
	}
	return ""
}

// commonCurrency is the single rate-card currency across bindings, or "" when
// rate cards disagree, because micros in different currencies do not compare.
func commonCurrency(bindings []api.ProviderBinding) string {
	currency := ""
	for _, b := range bindings {
		if b.Pricing == nil {
			continue
		}
		if currency != "" && b.Pricing.Currency != currency {
			return ""
		}
		currency = b.Pricing.Currency
	}
	return currency
}

func latestObservations(observations []Observation) map[string]Observation {
	latest := map[string]Observation{}
	for _, o := range observations {
		if prev, ok := latest[o.ProviderID]; !ok || o.ObservedAt.After(prev.ObservedAt) {
			latest[o.ProviderID] = o
		}
	}
	return latest
}

func blockedText(pinned string, cands []candidate) string {
	if len(cands) == 0 {
		return "no provider bindings supplied"
	}
	if pinned != "" {
		for _, c := range cands {
			if c.binding.ID == pinned {
				return fmt.Sprintf("pinned binding %q is not permitted: %s", pinned, strings.Join(c.reasons, "; "))
			}
		}
	}
	return "no binding passed the host's eligibility, isolation and feature constraints"
}
