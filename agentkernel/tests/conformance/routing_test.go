package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

func priced(b api.ProviderBinding, inputMicros int64) api.ProviderBinding {
	b.Pricing = &api.Pricing{Currency: "USD", InputMicrosPerMillion: inputMicros, OutputMicrosPerMillion: inputMicros,
		CachedInputMicrosPerMillion: api.Count(inputMicros), CacheWriteInputMicrosPerMillion: api.Count(inputMicros),
		Source: "host-rate-card", Version: "1", ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	return b
}

// TestA13BindingSurvivesRoutingThroughEngine: through the engine, a pinned
// binding is used even when an alternative ranks better, a pinned binding
// that fails a constraint blocks instead of falling back, and an ineligible
// or isolation-incompatible binding is never invoked however cheap.
func TestA13BindingSurvivesRoutingThroughEngine(t *testing.T) {
	cases := map[string]struct {
		bindings func() []api.ProviderBinding
		require  func(*api.ExecutionRequest)
		chosen   string // "" means blocked
	}{
		"pinned beats cheaper preferred": {
			bindings: func() []api.ProviderBinding {
				p1, p2 := priced(binding("p1"), 9000), priced(binding("p2"), 1)
				p1.Pinned, p1.Preference, p2.Preference = true, 5, 0
				return []api.ProviderBinding{p1, p2}
			},
			chosen: "p1",
		},
		"pinned failing a constraint blocks without fallback": {
			bindings: func() []api.ProviderBinding {
				p1, p2 := binding("p1"), binding("p2")
				p1.Pinned, p2.Isolation = true, api.IsolationHostProven
				return []api.ProviderBinding{p1, p2}
			},
			require: func(r *api.ExecutionRequest) { r.Constraints.RequireHostProvenIsolation = true },
		},
		"ineligible cheaper never chosen": {
			bindings: func() []api.ProviderBinding {
				p1, p2 := priced(binding("p1"), 9000), priced(binding("p2"), 1)
				p2.Eligible, p2.Preference, p1.Preference = false, 0, 9
				return []api.ProviderBinding{p1, p2}
			},
			chosen: "p1",
		},
		"isolation-incompatible preferred never chosen": {
			bindings: func() []api.ProviderBinding {
				p1, p2 := binding("p1"), binding("p2")
				p1.Preference, p2.Preference, p2.Isolation = 0, 9, api.IsolationHostProven
				return []api.ProviderBinding{p1, p2}
			},
			require: func(r *api.ExecutionRequest) { r.Constraints.RequireHostProvenIsolation = true },
			chosen:  "p2",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p1, p2 := scripted.New(end("from p1")), scripted.New(end("from p2"))
			k := newKernel(t, config{providers: providers(p1, p2)})
			req := request("a13")
			req.Providers = c.bindings()
			if c.require != nil {
				c.require(&req)
			}
			res := k.run(t, context.Background(), req)
			calls := map[string]int{"p1": len(p1.Requests()), "p2": len(p2.Requests())}
			if c.chosen == "" {
				want(t, res, api.OutcomeBlocked, api.CauseNoEligibleProvider)
				if calls["p1"]+calls["p2"] != 0 {
					t.Fatalf("blocked routing still invoked a provider: %v", calls)
				}
				return
			}
			want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
			if res.Provenance.ProviderID != c.chosen || res.Routing.Chosen != c.chosen || calls[c.chosen] != 1 {
				t.Fatalf("chosen %q (routing %q), calls %v, want %s", res.Provenance.ProviderID, res.Routing.Chosen, calls, c.chosen)
			}
			if calls["p1"]+calls["p2"] != 1 {
				t.Fatalf("more than one provider invoked: %v", calls)
			}
		})
	}
}
