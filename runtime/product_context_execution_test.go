package runtime

// #480: a product-associated WorkUnit executes against the product context it
// was ADMITTED with. These tests run real units through the real supervisor,
// SQLite store and EngineeringRun lifecycle; the only fake is the worker, and
// what it records is the ExecutionRequest the host built - rendered through the
// same providerPrompt every real provider uses.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/product"
)

const (
	productPolicyRef        = "policy-must-not-reach-the-worker"
	productContextPolicyRef = "context-policy-must-not-reach-the-worker"
	productWideClaim        = "retry budget is 3 across the platform"
)

type productUnitFixture struct {
	*fleetFixture
	supervisor *Supervisor
	at         time.Time
	nextIssue  int
}

func newProductUnitFixture(t *testing.T) *productUnitFixture {
	t.Helper()
	fleet := newFleetFixture(t, 4)
	return &productUnitFixture{fleetFixture: fleet, supervisor: fleet.supervisor(), at: time.Unix(1700000000, 0).UTC(), nextIssue: fleetFirstIssue + 20}
}

// seedProduct adopts a product over acme/repo whose configuration carries
// settings AND authority references, so a leak of either is observable.
func (f *productUnitFixture) seedProduct(name string, settings map[string]string) product.Product {
	f.t.Helper()
	p := composeProduct(f.t, name, 1, []string{"acme/repo"}, f.at)
	if _, _, err := f.store.AdoptProductRevision(p); err != nil {
		f.t.Fatal(err)
	}
	f.adoptConfiguration(p.ID, 1, settings)
	return p
}

func (f *productUnitFixture) adoptConfiguration(productID string, revision int, settings map[string]string) {
	f.t.Helper()
	configuration := product.ProductConfiguration{
		SchemaVersion: product.ConfigurationSchemaVersion, ProductID: productID, Revision: revision,
		Policy:        &domain.ObjectRevision{ID: productPolicyRef, Revision: "r1"},
		ContextPolicy: productContextPolicyRef, Settings: settings, CreatedAt: f.at,
	}
	if _, _, err := f.store.AdoptProductConfigurationRevision(configuration); err != nil {
		f.t.Fatal(err)
	}
}

// graphFor adopts a graph named name over fresh issues and associates it with
// productID when that is non-empty.
func (f *productUnitFixture) graphFor(name, productID string, units ...orchestration.WorkUnit) string {
	f.t.Helper()
	for i := range units {
		units[i].Issue = f.nextIssue
		f.nextIssue++
	}
	view, err := f.supervisor.AdoptWorkGraph(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: "claude", Operator: "operator@example",
		WorkGraph: &orchestration.WorkGraphProposal{Name: name, Revision: 1, Units: units},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if productID != "" {
		association, err := product.NewGraphAssociation(productID, view.GraphID, f.at)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.store.AssociateWorkGraph(association); err != nil {
			f.t.Fatal(err)
		}
	}
	return view.GraphID
}

// widelyPromote records a claim every unit of the product may read.
func (f *productUnitFixture) widelyPromote(productID, claim string) product.KnowledgeEntry {
	f.t.Helper()
	return f.promote(productID, product.ScopeWorkUnit, product.ScopeProduct, "seed/"+claim, "", claim)
}

// audiencedPromote records a claim only the named unit may read.
func (f *productUnitFixture) audiencedPromote(productID, graphID, unitID, claim string) product.KnowledgeEntry {
	f.t.Helper()
	return f.promote(productID, product.ScopeExecution, product.ScopeWorkUnit, "seed/"+claim, product.WorkUnitOwnerRef(graphID, unitID), claim)
}

func (f *productUnitFixture) promote(productID string, from, to product.Scope, owner, audience, claim string) product.KnowledgeEntry {
	f.t.Helper()
	raw, err := product.NewKnowledgeEntry(productID, from, owner, claim,
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: owner}, f.at)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.RecordKnowledgeEntry(raw); err != nil {
		f.t.Fatal(err)
	}
	promoted, promotion, err := product.Promote(raw, to, audience, "lead@example", f.at)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.RecordPromotion(promoted, promotion); err != nil {
		f.t.Fatal(err)
	}
	return promoted
}

// rawDiscovery records an unpromoted claim owned by exactly one unit.
func (f *productUnitFixture) rawDiscovery(productID, graphID, unitID, claim string) {
	f.t.Helper()
	owner := product.WorkUnitOwnerRef(graphID, unitID)
	raw, err := product.NewKnowledgeEntry(productID, product.ScopeWorkUnit, owner, claim,
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: owner}, f.at)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.RecordKnowledgeEntry(raw); err != nil {
		f.t.Fatal(err)
	}
}

// admit runs ONLY the activation step for one unit, so a test can change the
// world between admission and execution.
func (f *productUnitFixture) admit(graphID, unitID string) {
	f.t.Helper()
	view := f.graph(graphID)
	f.supervisor.orchestrationMu.Lock()
	defer f.supervisor.orchestrationMu.Unlock()
	if err := f.supervisor.activateWorkUnit(view, view.unit(f.t, unitID)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *productUnitFixture) runOf(graphID, unitID string) string {
	f.t.Helper()
	return f.activation(f.t, graphID, unitID).RunID
}

func (f *productUnitFixture) driveUnits(graphID string) WorkGraphView {
	f.t.Helper()
	return f.driveGraph(f.supervisor, graphID, nil)
}

func (f *productUnitFixture) requestOf(graphID, unitID string) ExecutionRequest {
	f.t.Helper()
	return f.worker.request(f.runOf(graphID, unitID))
}

func (f *productUnitFixture) engineeringInvocations(runID string) int {
	engineering, _ := f.worker.counts(runID)
	return engineering
}

func unitA() orchestration.WorkUnit {
	return orchestration.WorkUnit{ID: "a", Purpose: "do a", Role: domain.RoleImplementer}
}

func knowledgeStatements(context *execution.ProductContext) []string {
	var out []string
	for _, entry := range context.Knowledge {
		out = append(out, entry.Statement)
	}
	return out
}

func productOf(t *testing.T, request ExecutionRequest) *execution.ProductContext {
	t.Helper()
	if request.Product == nil {
		t.Fatal("the worker received no product context")
	}
	return request.Product
}

func requireAbsent(t *testing.T, prompt string, forbidden ...string) {
	t.Helper()
	for _, text := range forbidden {
		if strings.Contains(prompt, text) {
			t.Fatalf("the worker-visible prompt contains %q", text)
		}
	}
}

// TestProductAssociatedUnitExecutesWithItsProductContext is the end-to-end
// seam: association -> admitted WorkUnit -> ordinary EngineeringRun -> the
// ExecutionRequest and prompt the worker is actually given. It also proves what
// the worker must NOT see: a sibling product's state on the SAME repository,
// the unit's own unpromoted discovery, and the authority references a
// configuration carries.
func TestProductAssociatedUnitExecutesWithItsProductContext(t *testing.T) {
	f := newProductUnitFixture(t)
	storefront := f.seedProduct("storefront", map[string]string{"tier": "gold"})
	checkout := f.seedProduct("checkout", map[string]string{"tier": "SECRET-CHECKOUT-TIER"})
	graphID := f.graphFor("storefront-work", storefront.ID, unitA())
	f.widelyPromote(storefront.ID, productWideClaim)
	f.audiencedPromote(storefront.ID, graphID, "a", "audienced to unit a")
	f.rawDiscovery(storefront.ID, graphID, "a", "RAW-UNPROMOTED-DISCOVERY")
	f.widelyPromote(checkout.ID, "SECRET-CHECKOUT-CLAIM")

	settled := f.driveUnits(graphID)
	if settled.Counts.Activated.Completed != 1 {
		t.Fatalf("the unit did not complete: %+v", settled.Counts)
	}
	request := f.requestOf(graphID, "a")
	view := productOf(t, request)
	if view.ProductID != storefront.ID || view.ConfigurationRevision != 1 || view.Settings["tier"] != "gold" {
		t.Fatalf("the worker received the wrong product context: %+v", view)
	}
	if got := strings.Join(knowledgeStatements(view), "|"); !strings.Contains(got, productWideClaim) || !strings.Contains(got, "audienced to unit a") {
		t.Fatalf("the worker is missing its authorized knowledge: %s", got)
	}

	prompt := providerPrompt(request)
	for _, want := range []string{productFrameMarker, "setting tier: gold", productWideClaim, "audienced to unit a"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the worker-visible prompt lacks %q", want)
		}
	}
	requireAbsent(t, prompt, "SECRET-CHECKOUT", "RAW-UNPROMOTED-DISCOVERY", productPolicyRef, productContextPolicyRef)
}

// TestOverlappingProductsStayIsolatedAtExecution: two products list the SAME
// repository, each with its own graph. Neither unit's worker sees the other's
// configuration or knowledge.
func TestOverlappingProductsStayIsolatedAtExecution(t *testing.T) {
	f := newProductUnitFixture(t)
	one := f.seedProduct("product-one", map[string]string{"owner": "ONE-ONLY"})
	two := f.seedProduct("product-two", map[string]string{"owner": "TWO-ONLY"})
	graphOne := f.graphFor("work-one", one.ID, unitA())
	graphTwo := f.graphFor("work-two", two.ID, unitA())
	f.widelyPromote(one.ID, "CLAIM-ONE-ONLY")
	f.widelyPromote(two.ID, "CLAIM-TWO-ONLY")
	f.audiencedPromote(two.ID, graphTwo, "a", "AUDIENCED-TWO-ONLY")

	f.driveUnits(graphOne)
	f.driveUnits(graphTwo)
	promptOne := providerPrompt(f.requestOf(graphOne, "a"))
	promptTwo := providerPrompt(f.requestOf(graphTwo, "a"))
	if !strings.Contains(promptOne, "ONE-ONLY") || !strings.Contains(promptTwo, "TWO-ONLY") {
		t.Fatal("each worker should see its own product's context")
	}
	requireAbsent(t, promptOne, "TWO-ONLY")
	requireAbsent(t, promptTwo, "ONE-ONLY")
}

// TestAnAdmittedUnitKeepsItsContextAcrossNewerConfigurationRestartAndReplay is
// the freeze: configuration revision 2 and a newer promotion arrive AFTER the
// unit was admitted. Replaying the admission, then a real store reopen, then
// executing, still delivers revision 1 and exactly the admitted knowledge - and
// never a second batch, activation or run.
func TestAnAdmittedUnitKeepsItsContextAcrossNewerConfigurationRestartAndReplay(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
	graphID := f.graphFor("storefront-work", p.ID, unitA())
	f.widelyPromote(p.ID, productWideClaim)
	f.admit(graphID, "a")
	runID, batchID := f.runOf(graphID, "a"), f.activation(t, graphID, "a").BatchID
	admitted, _, _ := f.store.OrchestrationBatch(batchID)
	if admitted.Product == nil || admitted.Product.ConfigurationRevision != 1 {
		t.Fatalf("admission did not pin revision 1: %+v", admitted.Product)
	}

	f.adoptConfiguration(p.ID, 2, map[string]string{"tier": "NEWER-REVISION"})
	f.widelyPromote(p.ID, "PROMOTED-AFTER-ADMISSION")
	f.admit(graphID, "a") // replay

	reopen(t, f.phase8Fixture)
	f.supervisor = f.fleetFixture.supervisor()
	replayed, _, _ := f.store.OrchestrationBatch(batchID)
	if replayed.Product == nil || replayed.Product.ContextDigest != admitted.Product.ContextDigest {
		t.Fatalf("replay or restart rewrote the frozen identity: %+v", replayed.Product)
	}
	if got := f.runOf(graphID, "a"); got != runID {
		t.Fatalf("replay created a second run: %s then %s", runID, got)
	}

	f.driveUnits(graphID)
	request := f.requestOf(graphID, "a")
	view := productOf(t, request)
	if view.ConfigurationRevision != 1 || view.Settings["tier"] != "standard" {
		t.Fatalf("the unit drifted from its admitted configuration: %+v", view)
	}
	requireAbsent(t, providerPrompt(request), "NEWER-REVISION", "PROMOTED-AFTER-ADMISSION")
	if !strings.Contains(providerPrompt(request), productWideClaim) {
		t.Fatal("the admitted knowledge must still be delivered")
	}
	if count := f.engineeringInvocations(runID); count != 1 {
		t.Fatalf("the unit executed %d times", count)
	}
}

// TestUnitsReceiveOnlyKnowledgeAudiencedToThemselves: a request selector cannot
// be changed from outside, because the owner is derived from the batch's own
// graph and unit ids. Two units of one graph each get only what was promoted to
// them, and neither gets a raw discovery.
func TestUnitsReceiveOnlyKnowledgeAudiencedToThemselves(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
	graphID := f.graphFor("storefront-work", p.ID, unitA(),
		orchestration.WorkUnit{ID: "b", Purpose: "do b", Role: domain.RoleImplementer})
	f.audiencedPromote(p.ID, graphID, "a", "FOR-A-ONLY")
	f.audiencedPromote(p.ID, graphID, "b", "FOR-B-ONLY")
	f.rawDiscovery(p.ID, graphID, "a", "RAW-OF-A")
	f.rawDiscovery(p.ID, graphID, "b", "RAW-OF-B")

	f.driveUnits(graphID)
	promptA, promptB := providerPrompt(f.requestOf(graphID, "a")), providerPrompt(f.requestOf(graphID, "b"))
	if !strings.Contains(promptA, "FOR-A-ONLY") || !strings.Contains(promptB, "FOR-B-ONLY") {
		t.Fatal("each unit should receive the knowledge audienced to it")
	}
	requireAbsent(t, promptA, "FOR-B-ONLY", "RAW-OF-A", "RAW-OF-B")
	requireAbsent(t, promptB, "FOR-A-ONLY", "RAW-OF-A", "RAW-OF-B")
}

// TestAReviewerUnitInheritsNothingPrivateFromItsProducer: the reviewer depends
// on the producer, so it receives the producer's ADMITTED handoff as upstream -
// and none of the producer's private knowledge, which stays with its owner.
func TestAReviewerUnitInheritsNothingPrivateFromItsProducer(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
	graphID := f.graphFor("storefront-work", p.ID, unitA(),
		orchestration.WorkUnit{ID: "review", Purpose: "review a", Role: domain.RoleReviewer, DependsOn: []string{"a"}})
	f.audiencedPromote(p.ID, graphID, "a", "PRODUCER-PRIVATE-PROMOTION")
	f.rawDiscovery(p.ID, graphID, "a", "PRODUCER-RAW-DISCOVERY")
	f.widelyPromote(p.ID, productWideClaim)

	if settled := f.driveUnits(graphID); settled.Counts.Activated.Completed != 2 {
		t.Fatalf("producer and reviewer should both complete: %+v", settled.Counts)
	}
	reviewer := f.requestOf(graphID, "review")
	if len(reviewer.Upstream) != 1 || reviewer.Upstream[0].Handoff == nil {
		t.Fatalf("the reviewer should receive the producer's admitted handoff: %+v", reviewer.Upstream)
	}
	prompt := providerPrompt(reviewer)
	if !strings.Contains(prompt, productWideClaim) {
		t.Fatal("the reviewer should still receive the product-wide context it is authorized for")
	}
	requireAbsent(t, prompt, "PRODUCER-PRIVATE-PROMOTION", "PRODUCER-RAW-DISCOVERY")

	// The class a reviewer could be given its producer's reasoning through is
	// refused outright, whatever an operator policy asks.
	_, err := planning.RoleContextClasses(domain.RoleReviewer, &domain.ContextPolicy{ID: "greedy", Include: []domain.ContextClass{domain.ContextProducerReasoning}})
	if err == nil {
		t.Fatal("a policy requesting the producer's reasoning must be refused")
	}
}

// TestContextPolicyDecidesWhetherAUnitReceivesProductContext: product context
// travels as project_model_facts, so the operator's ContextPolicy can narrow it
// away - and can never narrow away the governance classes.
func TestContextPolicyDecidesWhetherAUnitReceivesProductContext(t *testing.T) {
	graph := orchestration.WorkGraph{ID: "graph-1", Units: []orchestration.WorkUnit{unitA()}}
	included := &domain.AgentAssignment{Context: domain.ContextPack{Included: []domain.ContextClass{domain.ContextObjective, domain.ContextProjectFacts}}}
	excluded := &domain.AgentAssignment{Context: domain.ContextPack{Included: []domain.ContextClass{domain.ContextObjective}}}

	for name, test := range map[string]struct {
		assignment *domain.AgentAssignment
		want       bool
	}{
		"no assignment: the role's own selection": {nil, true},
		"policy left project facts in":            {included, true},
		"policy excluded project facts":           {excluded, false},
	} {
		got, err := receivesProductContext(graph, "a", test.assignment)
		if err != nil || got != test.want {
			t.Fatalf("%s: got %t, %v", name, got, err)
		}
	}
	if _, err := receivesProductContext(graph, "missing", nil); err == nil {
		t.Fatal("a unit the graph does not contain must be refused")
	}
	if _, err := planning.RoleContextClasses(domain.RoleImplementer,
		&domain.ContextPolicy{ID: "strip", Exclude: []domain.ContextClass{domain.ContextObligations}}); err == nil {
		t.Fatal("a ContextPolicy must not be able to exclude a required governance class")
	}
	classes, err := planning.RoleContextClasses(domain.RoleImplementer,
		&domain.ContextPolicy{ID: "narrow", Exclude: []domain.ContextClass{domain.ContextProjectFacts}})
	if err != nil || classes[domain.ContextProjectFacts] || !classes[domain.ContextObligations] {
		t.Fatalf("narrowing must drop only the optional class: %v %v", classes, err)
	}
}

// TestProductConfigurationNeverChangesWhatTheWorkerMayDo: the configuration
// carries a policy reference, a context-policy reference and settings that read
// like grants. A unit of a product-associated graph is given exactly the same
// authority-bearing request fields as its twin in an unassociated graph.
func TestProductConfigurationNeverChangesWhatTheWorkerMayDo(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{
		"permission_bypass": "true", "budget": "999999", "trust": "operator_trusted", "tools": "*"})
	withProduct := f.graphFor("with-product", p.ID, unitA())
	without := f.graphFor("without-product", "", unitA())
	f.driveUnits(withProduct)
	f.driveUnits(without)

	associated, plain := f.requestOf(withProduct, "a"), f.requestOf(without, "a")
	if associated.Product == nil || plain.Product != nil {
		t.Fatalf("only the associated graph's unit carries product context: %v / %v", associated.Product, plain.Product)
	}
	for name, pair := range map[string][2]any{
		"DenyPermissionBypass":  {associated.DenyPermissionBypass, plain.DenyPermissionBypass},
		"ModelPreference":       {associated.ModelPreference, plain.ModelPreference},
		"RequiredTools":         {associated.RequiredTools, plain.RequiredTools},
		"Instructions":          {associated.Instructions, plain.Instructions},
		"Purpose":               {associated.Purpose, plain.Purpose},
		"AcceptanceObligations": {len(associated.AcceptanceObligations), len(plain.AcceptanceObligations)},
	} {
		if fmt.Sprint(pair[0]) != fmt.Sprint(pair[1]) {
			t.Fatalf("product configuration changed the authority-bearing request field %s: %v vs %v", name, pair[0], pair[1])
		}
	}
	// And the legacy twin's worker-visible prompt carries no product block.
	requireAbsent(t, providerPrompt(plain), productFrameMarker)
}

// TestMissingOrInconsistentProductContextRefusesBeforeAnyModelInvocation: every
// case leaves the worker never invoked and never falls back to another product,
// the current configuration or an empty context.
func TestMissingOrInconsistentProductContextRefusesBeforeAnyModelInvocation(t *testing.T) {
	setup := func(t *testing.T) (*productUnitFixture, product.Product, string, string) {
		f := newProductUnitFixture(t)
		p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
		graphID := f.graphFor("storefront-work", p.ID, unitA())
		f.admit(graphID, "a")
		return f, p, graphID, f.runOf(graphID, "a")
	}
	corrupt := func(t *testing.T, f *productUnitFixture, statement string, args ...any) {
		t.Helper()
		if _, err := f.store.db.Exec(statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, f *productUnitFixture, p product.Product, graphID string){
		"the pinned configuration revision is gone": func(t *testing.T, f *productUnitFixture, p product.Product, _ string) {
			corrupt(t, f, `DELETE FROM product_configuration_revisions WHERE product_id = ?`, p.ID)
		},
		"the association was removed after admission": func(t *testing.T, f *productUnitFixture, _ product.Product, graphID string) {
			corrupt(t, f, `DELETE FROM product_graph_associations WHERE graph_id = ?`, graphID)
		},
		"the graph was re-associated to another product": func(t *testing.T, f *productUnitFixture, _ product.Product, graphID string) {
			other := f.seedProduct("other", nil)
			corrupt(t, f, `UPDATE product_graph_associations SET product_id = ? WHERE graph_id = ?`, other.ID, graphID)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, p, graphID, runID := setup(t)
			mutate(t, f, p, graphID)
			for range 6 {
				if _, err := f.supervisor.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				f.clock.advance(61 * time.Second)
			}
			if got := f.engineeringInvocations(runID); got != 0 {
				t.Fatalf("a model was invoked %d time(s) with inconsistent product context", got)
			}
			if view := f.graph(graphID); view.unit(t, "a").State == orchestration.UnitState(orchestration.ItemCompleted) {
				t.Fatal("the unit completed without its product context")
			}
		})
	}
}

// TestUnitProductContextRefusesEveryInconsistencyDirectly pins the individual
// cross-checks unitProductContext makes, without a supervisor in the way.
func TestUnitProductContextRefusesEveryInconsistencyDirectly(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
	graphID := f.graphFor("storefront-work", p.ID, unitA())
	f.admit(graphID, "a")
	batch, _, err := f.store.OrchestrationBatch(f.activation(t, graphID, "a").BatchID)
	if err != nil {
		t.Fatal(err)
	}
	run := f.fleetFixture.runtime
	state := &runState{}

	if resolved, err := run.unitProductContext(state, batch, nil); err != nil || resolved == nil || resolved.ProductID != p.ID {
		t.Fatalf("the consistent case must resolve: %v %v", resolved, err)
	}
	unpinned := batch
	unpinned.Product = nil
	if _, err := run.unitProductContext(state, unpinned, nil); err == nil {
		t.Fatal("an associated graph's unit admitted without a pin must be refused")
	}
	foreign := batch
	pin := *batch.Product
	pin.ProductID = "product-someone-else"
	foreign.Product = &pin
	if _, err := run.unitProductContext(state, foreign, nil); err == nil {
		t.Fatal("a pin for another product must be refused")
	}
	tampered := batch
	pin = *batch.Product
	pin.ContextDigest = strings.Repeat("0", len(pin.ContextDigest))
	tampered.Product = &pin
	if _, err := run.unitProductContext(state, tampered, nil); err == nil {
		t.Fatal("a pin whose identity was corrupted must be refused")
	}
	mismatched := &runState{run: EngineeringRun{Plan: &RunPlanBinding{PlanID: "plan-x", Revision: 1, PlanDigest: "d"}}}
	if _, err := run.unitProductContext(mismatched, batch, nil); err == nil {
		t.Fatal("a run bound to a plan the graph does not bind must be refused")
	}
	// A product that never named the graph's repository: the association row is
	// forced in underneath AssociateWorkGraph's own check, as corruption would.
	elsewhere := composeProduct(t, "elsewhere", 1, []string{"acme/not-this-repo"}, f.at)
	if _, _, err := f.store.AdoptProductRevision(elsewhere); err != nil {
		t.Fatal(err)
	}
	compiled, err := product.CompileContext(f.store, unitContextRequest(elsewhere.ID, *batch.Origin))
	if err != nil {
		t.Fatal(err)
	}
	elsewherePin, err := product.NewContextPin(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE product_graph_associations SET product_id = ? WHERE graph_id = ?`, elsewhere.ID, graphID); err != nil {
		t.Fatal(err)
	}
	outsider := batch
	outsider.Product = &elsewherePin
	if _, err := run.unitProductContext(state, outsider, nil); err == nil {
		t.Fatal("a product that does not list the graph's repository must be refused")
	}
	stray := batch
	stray.Origin = &orchestration.BatchOrigin{GraphID: "graph-without-association", UnitID: "a"}
	if _, err := run.unitProductContext(state, stray, nil); err == nil {
		t.Fatal("a pin on a unit whose graph has no association must be refused")
	}
}

// TestLegacyUnassociatedGraphExecutesWithoutProductContext: no association, no
// product block, nothing invented - and the unit still completes.
func TestLegacyUnassociatedGraphExecutesWithoutProductContext(t *testing.T) {
	f := newProductUnitFixture(t)
	f.seedProduct("storefront", map[string]string{"tier": "standard"}) // exists, owns nothing here
	graphID := f.graphFor("legacy-work", "", unitA())
	if settled := f.driveUnits(graphID); settled.Counts.Activated.Completed != 1 {
		t.Fatalf("the legacy unit did not complete: %+v", settled.Counts)
	}
	request := f.requestOf(graphID, "a")
	if request.Product != nil {
		t.Fatalf("a legacy unit was given product context: %+v", request.Product)
	}
	requireAbsent(t, providerPrompt(request), productFrameMarker)
	batch, _, _ := f.store.OrchestrationBatch(f.activation(t, graphID, "a").BatchID)
	if batch.Product != nil {
		t.Fatalf("a legacy batch was pinned: %+v", batch.Product)
	}
}

// TestAssociationAfterAUnitWasAdmittedIsRefused: a unit admitted before its
// graph named a product froze no context, so the association is refused rather
// than leaving that unit to execute with context nothing pinned. Replaying the
// SAME association stays idempotent.
func TestAssociationAfterAUnitWasAdmittedIsRefused(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", nil)
	graphID := f.graphFor("late-association", "", unitA())
	f.admit(graphID, "a")
	association, err := product.NewGraphAssociation(p.ID, graphID, f.at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AssociateWorkGraph(association); err == nil {
		t.Fatal("associating a graph with an admitted unit must be refused")
	}
	if _, found, _ := f.store.AssociatedProduct(graphID); found {
		t.Fatal("the refused association was stored")
	}

	early := f.graphFor("early-association", p.ID, unitA())
	f.admit(early, "a")
	earlier, err := product.NewGraphAssociation(p.ID, early, f.at)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := f.store.AssociateWorkGraph(earlier); err != nil || created {
		t.Fatalf("replaying an existing association must stay idempotent: created=%t err=%v", created, err)
	}
}

// TestARemediationEpochInheritsItsUnitsFrozenContext: a unit keeps ONE context
// identity for its whole life, even when a newer configuration exists by the
// time a governed remediation epoch is admitted.
func TestARemediationEpochInheritsItsUnitsFrozenContext(t *testing.T) {
	f := newProductUnitFixture(t)
	p := f.seedProduct("storefront", map[string]string{"tier": "standard"})
	graphID := f.graphFor("storefront-work", p.ID, unitA())
	f.admit(graphID, "a")
	first, _, _ := f.store.OrchestrationBatch(f.activation(t, graphID, "a").BatchID)
	f.adoptConfiguration(p.ID, 2, map[string]string{"tier": "NEWER-REVISION"})

	epoch := first
	origin := *first.Origin
	origin.Epoch = 2
	epoch.Origin, epoch.Product = &origin, nil
	pinned, err := f.store.pinProductContext(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Product == nil || pinned.Product.ContextDigest != first.Product.ContextDigest || pinned.Product.ConfigurationRevision != 1 {
		t.Fatalf("the epoch did not inherit the unit's frozen context: %+v", pinned.Product)
	}

	// A fresh pin for the same unit WOULD have moved to revision 2: this is the
	// difference inheritance makes.
	origin.Epoch = 0
	fresh, err := f.store.pinProductContext(orchestration.Batch{Origin: &origin})
	if err != nil || fresh.Product == nil || fresh.Product.ConfigurationRevision != 2 {
		t.Fatalf("a non-epoch pin reads the current revision: %v %+v", err, fresh.Product)
	}

	orphan := f.graphFor("orphan-epoch", p.ID, unitA())
	orphanOrigin := orchestration.BatchOrigin{GraphID: orphan, UnitID: "a", Epoch: 2}
	if _, err := f.store.pinProductContext(orchestration.Batch{Origin: &orphanOrigin}); err == nil {
		t.Fatal("an epoch for a unit that was never admitted cannot inherit a context")
	}
}

func TestGraphPlanBindingMustMatchTheRunsPlan(t *testing.T) {
	bound := orchestration.WorkGraph{ID: "graph-1", ObjectivePlan: &domain.PlanRef{ID: "plan-1", Revision: 2, Digest: "d2"}}
	same := &RunPlanBinding{PlanID: "plan-1", Revision: 2, PlanDigest: "d2"}
	if err := requireGraphPlanBinding(bound, same); err != nil {
		t.Fatalf("a matching binding must pass: %v", err)
	}
	if err := requireGraphPlanBinding(bound, nil); err != nil {
		t.Fatalf("a run with no plan binding is not a plan stage: %v", err)
	}
	for name, binding := range map[string]*RunPlanBinding{
		"another plan":     {PlanID: "plan-2", Revision: 2, PlanDigest: "d2"},
		"another revision": {PlanID: "plan-1", Revision: 3, PlanDigest: "d2"},
		"another digest":   {PlanID: "plan-1", Revision: 2, PlanDigest: "other"},
	} {
		if err := requireGraphPlanBinding(bound, binding); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if err := requireGraphPlanBinding(orchestration.WorkGraph{ID: "flat"}, same); err == nil {
		t.Fatal("a plan-bound run executing a graph that binds no plan must be refused")
	}
}
