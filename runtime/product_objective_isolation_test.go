package runtime

// #480 acceptance: two products list the SAME repository, each governs its own
// approved objective plan, and the two plans run through the real planner
// reconciler, supervisor, SQLite store and EngineeringRun lifecycle - across a
// configuration update and a store restart that both happen AFTER the stages
// were admitted. The only fake is the worker, and what it records is the
// ExecutionRequest the host built.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func isolationProducerStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Implement the backend half.",
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
			InvocationMode:       domain.InvocationModeMutating},
		{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Implement the frontend half.",
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
			InvocationMode:       domain.InvocationModeMutating},
	}
}

func TestTwoProductsSharingARepositoryStayIsolatedThroughTheObjectiveJourney(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	worker := &objectivePlanRemediationWorker{}
	fleet.deps.Provider = worker
	fleet.runtime = fleet.newRuntime(fleet.deps)
	f := &productUnitFixture{fleetFixture: fleet, at: time.Unix(1700000000, 0).UTC()}
	one := f.seedProduct("storefront", map[string]string{"owner": "ONE-ONLY"})
	two := f.seedProduct("checkout", map[string]string{"owner": "TWO-ONLY"})
	f.widelyPromote(one.ID, "CLAIM-ONE-ONLY")
	f.widelyPromote(two.ID, "CLAIM-TWO-ONLY")

	type governed struct {
		plan    *planRunFixture
		product string
		graph   string
		other   string
	}
	var plans []*governed
	for _, spec := range []struct{ id, product, other string }{{"plan-one", one.ID, "TWO-ONLY"}, {"plan-two", two.ID, "ONE-ONLY"}} {
		plan := newNamedPlanRunFixture(t, fleet.phase8Fixture, spec.id, isolationProducerStages(), fleet.deps.ConfigDigest, defaultPlanFixtureEnvelope)
		graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.service.AssociateProductGraph(spec.product, graphID); err != nil {
			t.Fatal(err)
		}
		plan.approve(t)
		plans = append(plans, &governed{plan: plan, product: spec.product, graph: graphID, other: spec.other})
	}

	supervisor := fleet.supervisor()
	ctx := context.Background()
	tick := func() {
		t.Helper()
		for _, g := range plans {
			g.plan.reconcile(t)
		}
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
	}
	admitted := func() bool {
		for _, g := range plans {
			for _, unit := range []string{"backend", "frontend"} {
				if fleet.graph(g.graph).unit(t, unit).BatchID == "" {
					return false
				}
			}
		}
		return true
	}
	// Admission only: the plan reconciler starts each stage, which writes its
	// batch with the product pin and creates its run. Nothing has executed yet.
	for range 5 {
		for _, g := range plans {
			g.plan.reconcile(t)
		}
		if admitted() {
			break
		}
	}
	if !admitted() {
		for _, g := range plans {
			t.Logf("%s: %+v", g.plan.plan.ID, g.plan.reconcile(t))
			for _, u := range fleet.graph(g.graph).Units {
				t.Logf("  unit %s state=%s batch=%q run=%q reason=%s", u.UnitID, u.State, u.BatchID, u.RunID, u.Reason)
			}
		}
		t.Fatal("the plans never admitted their stages")
	}

	if n := worker.invocations(); n != 0 {
		t.Fatalf("%d stages had already executed before the world changed", n)
	}

	// The world changes AFTER admission: newer configuration and newer knowledge
	// for both products, then a restart of the process's view of the store.
	for _, g := range plans {
		f.adoptConfiguration(g.product, 2, map[string]string{"owner": "NEWER-REVISION"})
		f.widelyPromote(g.product, "PROMOTED-AFTER-ADMISSION")
	}
	store, _ := reopen(t, fleet.phase8Fixture)
	for _, g := range plans {
		g.plan.service.Store, g.plan.reconciler.Store = store, store
		g.plan.reconciler.Service = g.plan.service
	}
	supervisor = fleet.supervisor()

	finished := func() bool {
		for _, g := range plans {
			view := fleet.graph(g.graph)
			if view.unit(t, "backend").Output == nil || view.unit(t, "frontend").Output == nil {
				return false
			}
		}
		return true
	}
	for range 60 {
		tick()
		if finished() {
			break
		}
	}
	if !finished() {
		t.Fatal("the plans never completed their stages")
	}

	invoked := 0
	for _, g := range plans {
		view := fleet.graph(g.graph)
		for _, unitID := range []string{"backend", "frontend"} {
			unit := view.unit(t, unitID)
			worker.mu.Lock()
			request, attempts := worker.requests[unit.RunID], worker.attempts[unit.RunID]
			worker.mu.Unlock()
			prompt := providerPrompt(request)
			delivered := productOf(t, request)
			if delivered.ProductID != g.product || delivered.ConfigurationRevision != 1 {
				t.Fatalf("%s/%s ran as product %s revision %d, want %s revision 1", g.plan.plan.ID, unitID, delivered.ProductID, delivered.ConfigurationRevision, g.product)
			}
			own := "ONE-ONLY"
			claim := "CLAIM-ONE-ONLY"
			if g.other == "ONE-ONLY" {
				own, claim = "TWO-ONLY", "CLAIM-TWO-ONLY"
			}
			if !strings.Contains(prompt, own) || !strings.Contains(prompt, claim) {
				t.Fatalf("%s/%s did not receive its own product's admitted context", g.plan.plan.ID, unitID)
			}
			requireAbsent(t, prompt, g.other, "NEWER-REVISION", "PROMOTED-AFTER-ADMISSION", "CLAIM-"+strings.TrimSuffix(g.other, "-ONLY")+"-ONLY")
			batch, found, err := fleet.store.OrchestrationBatch(unit.BatchID)
			if err != nil || !found || batch.Product == nil || batch.Product.ProductID != g.product || batch.Product.ConfigurationRevision != 1 {
				t.Fatalf("%s/%s has no durable product pin for %s: %+v (%v)", g.plan.plan.ID, unitID, g.product, batch.Product, err)
			}
			if run, _ := storedRun(t, fleet.phase8Fixture, unit.RunID); run.Plan == nil || run.Plan.PlanID != g.plan.plan.ID {
				t.Fatalf("run %s is not the %s stage run", unit.RunID, g.plan.plan.ID)
			}
			if attempts != 1 {
				t.Fatalf("%s/%s executed %d times across the restart", g.plan.plan.ID, unitID, attempts)
			}
			invoked++
		}
	}
	if invoked != 4 {
		t.Fatalf("expected four stage runs, saw %d", invoked)
	}
}
