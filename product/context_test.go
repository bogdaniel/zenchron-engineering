package product

import (
	"testing"
	"time"
)

// fakeStore is a Store with no database, used to prove CompileContext itself
// never asks for a product other than the one it was given, never applies an
// unrecognized scope, and never conflates "at or below this scope rank" with
// "visible to this caller". runtime/product_store_test.go and
// runtime/product_context_fixture_test.go prove the same thing end to end
// against the real SQLite store.
type fakeStore struct {
	products       map[string]Product
	configurations map[string]map[int]ProductConfiguration
	knowledge      map[string][]KnowledgeEntry
	queried        []string
}

func (f *fakeStore) CurrentProduct(productID string) (Product, bool, error) {
	f.queried = append(f.queried, productID)
	p, ok := f.products[productID]
	return p, ok, nil
}

func (f *fakeStore) CurrentConfiguration(productID string) (ProductConfiguration, bool, error) {
	f.queried = append(f.queried, productID)
	revisions := f.configurations[productID]
	best, ok := ProductConfiguration{}, false
	for _, c := range revisions {
		if !ok || c.Revision > best.Revision {
			best, ok = c, true
		}
	}
	return best, ok, nil
}

func (f *fakeStore) ConfigurationRevision(productID string, revision int) (ProductConfiguration, bool, error) {
	f.queried = append(f.queried, productID)
	c, ok := f.configurations[productID][revision]
	return c, ok, nil
}

func (f *fakeStore) OwnedKnowledge(productID string, scope Scope, ownerRef string) ([]KnowledgeEntry, error) {
	f.queried = append(f.queried, productID)
	var out []KnowledgeEntry
	for _, entry := range f.knowledge[productID] {
		if entry.Provenance.Type == ProvenancePromotion || entry.OwnerRef != ownerRef {
			continue
		}
		rank, err := compareScope(entry.Scope, scope)
		if err != nil {
			return nil, err
		}
		if rank <= 0 {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (f *fakeStore) PromotedKnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error) {
	f.queried = append(f.queried, productID)
	var out []KnowledgeEntry
	for _, entry := range f.knowledge[productID] {
		if entry.Provenance.Type != ProvenancePromotion {
			continue
		}
		rank, err := compareScope(entry.Scope, scope)
		if err != nil {
			return nil, err
		}
		if rank <= 0 {
			out = append(out, entry)
		}
	}
	return out, nil
}

func twoOverlappingProducts(t *testing.T) (*fakeStore, KnowledgeEntry, KnowledgeEntry) {
	t.Helper()
	at := time.Unix(1700000000, 0).UTC()
	productA := composedProduct(t, "storefront", 1, []string{"acme/shared-platform-lib", "acme/storefront-web"})
	productB := composedProduct(t, "checkout", 1, []string{"acme/shared-platform-lib", "acme/checkout-api"})

	discoveryA, err := NewKnowledgeEntry(productA.ID, ScopeWorkUnit, "run-storefront-1", "shared-platform-lib retry budget is 3",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-storefront-1"}, at)
	if err != nil {
		t.Fatal(err)
	}
	promotedA, _, err := Promote(discoveryA, ScopeProduct, "storefront-lead@example", at)
	if err != nil {
		t.Fatal(err)
	}

	discoveryB, err := NewKnowledgeEntry(productB.ID, ScopeWorkUnit, "run-checkout-1", "shared-platform-lib retry budget is 5",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-checkout-1"}, at)
	if err != nil {
		t.Fatal(err)
	}
	promotedB, _, err := Promote(discoveryB, ScopeProduct, "checkout-lead@example", at)
	if err != nil {
		t.Fatal(err)
	}

	store := &fakeStore{
		products: map[string]Product{productA.ID: productA, productB.ID: productB},
		knowledge: map[string][]KnowledgeEntry{
			productA.ID: {discoveryA, promotedA},
			productB.ID: {discoveryB, promotedB},
		},
	}
	return store, promotedA, promotedB
}

func TestCompileContextIsolatesOverlappingProducts(t *testing.T) {
	store, promotedA, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}

	context, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeProduct})
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Knowledge) != 1 || context.Knowledge[0].ID != promotedA.ID {
		t.Fatalf("expected exactly storefront's own promoted claim, got %+v", context.Knowledge)
	}
	for _, queried := range store.queried {
		if queried != productAID {
			t.Fatalf("CompileContext for storefront queried a different product: %q", queried)
		}
	}
}

func TestCompileContextRefusesMissingProduct(t *testing.T) {
	store := &fakeStore{products: map[string]Product{}}
	if _, err := CompileContext(store, ContextRequest{ProductID: "product-nonexistent", Scope: ScopeProduct}); err == nil {
		t.Fatal("compiling context for a product that does not exist should fail closed, not return an empty context")
	}
}

func TestCompileContextRefusesUnknownScope(t *testing.T) {
	store, _, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: "galaxy"}); err == nil {
		t.Fatal("an unrecognized scope should be refused")
	}
}

func TestCompileContextRequiresOwnerReferenceAtNarrowScope(t *testing.T) {
	store, _, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeWorkUnit}); err == nil {
		t.Fatal("a work_unit-scoped request with no owner reference should be refused")
	}
}

func TestCompileContextNeverLeaksUnpromotedKnowledgeToWiderScope(t *testing.T) {
	store, promotedA, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	// A product-scoped request sees the PROMOTED claim and nothing else - in
	// particular, not the raw work_unit discovery it was promoted from, which
	// would be the B3 leak (scope_rank <= requested with no promotion gate).
	context, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeProduct})
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Knowledge) != 1 || context.Knowledge[0].ID != promotedA.ID {
		t.Fatalf("product-scoped context should contain only the promoted entry, got %+v", context.Knowledge)
	}
}

func TestCompileContextIsolatesOwnersWithinTheSameProduct(t *testing.T) {
	store, _, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1700000000, 0).UTC()
	otherOwner, err := NewKnowledgeEntry(productAID, ScopeWorkUnit, "run-storefront-2", "a different unit's claim",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-storefront-2"}, at)
	if err != nil {
		t.Fatal(err)
	}
	store.knowledge[productAID] = append(store.knowledge[productAID], otherOwner)

	context, err := CompileContext(store, ContextRequest{
		ProductID: productAID, Scope: ScopeWorkUnit, OwnerRef: "run-storefront-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range context.Knowledge {
		if entry.ID == otherOwner.ID {
			t.Fatal("one work unit's context leaked another work unit's raw discovery within the same product")
		}
	}
}

func TestCompileContextPinsAnExactConfigurationRevision(t *testing.T) {
	store, _, _ := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1700000000, 0).UTC()
	revisionOne := ProductConfiguration{
		SchemaVersion: ConfigurationSchemaVersion, ProductID: productAID, Revision: 1,
		Settings: map[string]string{"tier": "standard"}, CreatedAt: at,
	}
	revisionTwo := ProductConfiguration{
		SchemaVersion: ConfigurationSchemaVersion, ProductID: productAID, Revision: 2,
		Settings: map[string]string{"tier": "premium"}, CreatedAt: at.Add(time.Hour),
	}
	store.configurations = map[string]map[int]ProductConfiguration{
		productAID: {1: revisionOne, 2: revisionTwo},
	}

	pinned := 1
	context, err := CompileContext(store, ContextRequest{
		ProductID: productAID, Scope: ScopeProduct, ConfigurationRevision: &pinned,
	})
	if err != nil {
		t.Fatal(err)
	}
	if context.Configuration == nil || context.Configuration.Settings["tier"] != "standard" {
		t.Fatalf("a bound caller pinning revision 1 should never see revision 2's settings, got %+v", context.Configuration)
	}

	current, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeProduct})
	if err != nil {
		t.Fatal(err)
	}
	if current.Configuration == nil || current.Configuration.Settings["tier"] != "premium" {
		t.Fatalf("an unpinned caller should see the current revision, got %+v", current.Configuration)
	}

	missing := 99
	if _, err := CompileContext(store, ContextRequest{
		ProductID: productAID, Scope: ScopeProduct, ConfigurationRevision: &missing,
	}); err == nil {
		t.Fatal("pinning a configuration revision that does not exist should fail closed")
	}
}
