package routing_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/routing"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func binding(id string, preference int) api.ProviderBinding {
	return api.ProviderBinding{
		ID: id, Kind: "scripted", Model: "m", ModelVersion: "1", ConfigFingerprint: "f",
		Eligible: true, Preference: preference, Isolation: api.IsolationUnproven,
		ContextWindow: 1_000_000, MaxOutputTokens: 1_000_000, Features: []string{api.FeatureTools},
	}
}

func priced(b api.ProviderBinding, inputMicros int64) api.ProviderBinding {
	b.Pricing = &api.Pricing{Currency: "USD", InputMicrosPerMillion: inputMicros, Source: "host", Version: "1",
		CachedInputMicrosPerMillion: cost(inputMicros), CacheWriteInputMicrosPerMillion: cost(inputMicros)}
	return b
}

func cost(v int64) *int64 { return &v }

func candidateFor(t *testing.T, d api.RoutingDecision, id string) api.RoutingCandidate {
	t.Helper()
	for _, c := range d.Candidates {
		if c.ProviderID == id {
			return c
		}
	}
	t.Fatalf("no candidate %q in %+v", id, d.Candidates)
	return api.RoutingCandidate{}
}

func TestCheaperIneligibleNeverChosen(t *testing.T) {
	cheap := priced(binding("cheap", 0), 1)
	cheap.Eligible = false
	costly := priced(binding("costly", 5), 1000)
	d := routing.Select(routing.Input{Bindings: []api.ProviderBinding{cheap, costly}, Now: now})
	if d.Chosen != "costly" {
		t.Fatalf("chosen %q, want the only eligible binding", d.Chosen)
	}
	c := candidateFor(t, d, "cheap")
	if c.Eligible || c.Rank != 0 || !strings.Contains(strings.Join(c.Reasons, ";"), "eligible") {
		t.Fatalf("ineligible candidate = %+v", c)
	}
}

func TestUnknownPriceIsNeverZero(t *testing.T) {
	unknown := binding("unknown", 0)
	known := priced(binding("known", 0), 50)
	d := routing.Select(routing.Input{Bindings: []api.ProviderBinding{unknown, known}, Now: now})
	if d.Chosen != "known" {
		t.Fatalf("chosen %q: unknown price ranked as if it were cheaper than a known price", d.Chosen)
	}
	if r := strings.Join(candidateFor(t, d, "unknown").Reasons, ";"); !strings.Contains(r, "cost: unknown") {
		t.Fatalf("unknown cost not stated: %s", r)
	}
	// A fresh observation with nil cost is unknown too, not zero.
	d = routing.Select(routing.Input{
		Bindings:     []api.ProviderBinding{binding("a", 0), binding("b", 0)},
		Observations: []routing.Observation{{ProviderID: "a", ObservedAt: now}, {ProviderID: "b", CostMicrosPerCall: cost(900), ObservedAt: now}},
		Now:          now, StaleAfter: time.Hour,
	})
	if d.Chosen != "b" {
		t.Fatalf("chosen %q, want the binding with a known observed cost", d.Chosen)
	}
}

func TestStaleObservationVisibleAndUnused(t *testing.T) {
	d := routing.Select(routing.Input{
		Bindings: []api.ProviderBinding{binding("a", 0), binding("b", 0)},
		Observations: []routing.Observation{
			{ProviderID: "a", CostMicrosPerCall: cost(1), ObservedAt: now.Add(-48 * time.Hour)},
			{ProviderID: "b", CostMicrosPerCall: cost(500), ObservedAt: now.Add(-time.Minute)},
		},
		Now: now, StaleAfter: time.Hour,
	})
	if d.Chosen != "b" {
		t.Fatalf("chosen %q: stale cheap observation was used as known", d.Chosen)
	}
	if r := strings.Join(candidateFor(t, d, "a").Reasons, ";"); !strings.Contains(r, "stale") {
		t.Fatalf("stale observation not marked: %s", r)
	}
}

func TestPinnedPreservedAndNeverFallsBack(t *testing.T) {
	pinned := binding("operator-choice", 9)
	pinned.Pinned = true
	cheaper := priced(binding("cheaper", 0), 1)
	d := routing.Select(routing.Input{Bindings: []api.ProviderBinding{cheaper, pinned}, Now: now})
	if d.Chosen != "operator-choice" {
		t.Fatalf("chosen %q, want the pinned binding", d.Chosen)
	}
	d = routing.Select(routing.Input{
		Bindings: []api.ProviderBinding{cheaper, pinned}, RequireHostProvenIsolation: true, Now: now,
	})
	if d.Chosen != "" || !strings.Contains(d.Blocked, "operator-choice") {
		t.Fatalf("pinned binding failing a filter must block, got chosen %q blocked %q", d.Chosen, d.Blocked)
	}
}

func TestProtectedIncompatibleExcluded(t *testing.T) {
	proven := binding("proven", 5)
	proven.Isolation = api.IsolationHostProven
	unproven := priced(binding("unproven", 0), 1)
	noTools := binding("no-tools", 0)
	noTools.Isolation = api.IsolationHostProven
	noTools.Features = nil
	d := routing.Select(routing.Input{
		Bindings:                   []api.ProviderBinding{unproven, noTools, proven},
		RequireHostProvenIsolation: true,
		RequiredFeatures:           []string{api.FeatureTools, api.FeatureArtifacts},
		Now:                        now,
	})
	if d.Chosen != "proven" {
		t.Fatalf("chosen %q, want the only host-proven binding with tools", d.Chosen)
	}
	if candidateFor(t, d, "unproven").Eligible || candidateFor(t, d, "no-tools").Eligible {
		t.Fatalf("protected-incompatible bindings marked eligible: %+v", d.Candidates)
	}
	d = routing.Select(routing.Input{Bindings: []api.ProviderBinding{unproven}, RequireHostProvenIsolation: true, Now: now})
	if d.Chosen != "" || d.Blocked == "" {
		t.Fatalf("no permitted binding must block, got %+v", d)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	bs := []api.ProviderBinding{binding("zeta", 1), binding("alpha", 1), binding("mid", 1)}
	first := routing.Select(routing.Input{Bindings: bs, Now: now})
	reversed := routing.Select(routing.Input{Bindings: []api.ProviderBinding{bs[2], bs[1], bs[0]}, Now: now})
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(reversed)
	if first.Chosen != "alpha" || string(a) != string(b) {
		t.Fatalf("tie-break not deterministic:\n%s\n%s", a, b)
	}
	for i, c := range first.Candidates {
		if c.Rank != i+1 {
			t.Fatalf("ranks not sequential: %+v", first.Candidates)
		}
	}
}

func TestReportIsDeterministicSummary(t *testing.T) {
	d1 := routing.Select(routing.Input{Bindings: []api.ProviderBinding{binding("b", 0), binding("a", 1)}, Now: now})
	d2 := api.RoutingDecision{Blocked: "none", Candidates: []api.RoutingCandidate{{ProviderID: "a"}}}
	r := routing.Report([]api.RoutingDecision{d1, d2}, []routing.Observation{{ProviderID: "a"}, {ProviderID: "a", CostMicrosPerCall: cost(3)}})
	if r.Decisions != 2 || r.Blocked != 1 || len(r.Providers) != 2 || r.Providers[0].ProviderID != "a" {
		t.Fatalf("report = %+v", r)
	}
	if a := r.Providers[0]; a.Considered != 2 || a.Permitted != 1 || a.Chosen != 0 || a.KnownCostSamples != 1 || a.UnknownCostSamples != 1 {
		t.Fatalf("row a = %+v", a)
	}
	if b := r.Providers[1]; b.Chosen != 1 {
		t.Fatalf("row b = %+v", b)
	}
}

// TestUnknownCacheRateLeavesCostUnknown: a cache-write rate may exceed the
// input rate, so a card without one cannot bound a call's price, and a known
// higher write rate sets the worst case.
func TestUnknownCacheRateLeavesCostUnknown(t *testing.T) {
	partial := priced(binding("partial", 1), 1)
	partial.Pricing.CacheWriteInputMicrosPerMillion = nil
	writes := priced(binding("writes", 1), 1)
	writes.Pricing.CacheWriteInputMicrosPerMillion = cost(1_000_000)
	full := priced(binding("full", 1), 1_000)
	d := routing.Select(routing.Input{Bindings: []api.ProviderBinding{partial, writes, full}, Now: now})
	if d.Chosen != "full" {
		t.Fatalf("chose %q; the only card with a bounded cheap worst case is full", d.Chosen)
	}
	reasons := strings.Join(candidateFor(t, d, "partial").Reasons, "; ")
	if !strings.Contains(reasons, "worst case unknown") || !strings.Contains(reasons, "cost: unknown") {
		t.Fatalf("partial card reasons %q", reasons)
	}
}
