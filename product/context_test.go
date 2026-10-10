package product

import (
	"testing"
	"time"
)

// fakeStore is a Store with no database, used to prove CompileContext itself
// never asks for a product other than the one it was given, and never applies
// an unrecognized scope. runtime/product_store_test.go proves the same thing
// end to end against the real SQLite store.
type fakeStore struct {
	products       map[string]Product
	configurations map[string]ProductConfiguration
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
	c, ok := f.configurations[productID]
	return c, ok, nil
}

func (f *fakeStore) KnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error) {
	f.queried = append(f.queried, productID)
	var out []KnowledgeEntry
	for _, entry := range f.knowledge[productID] {
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

func twoOverlappingProducts(t *testing.T) *fakeStore {
	t.Helper()
	at := time.Unix(1700000000, 0).UTC()
	productA := composedProduct(t, "storefront", 1, []string{"acme/shared-platform-lib", "acme/storefront-web"})
	productB := composedProduct(t, "checkout", 1, []string{"acme/shared-platform-lib", "acme/checkout-api"})

	discoveryA, err := NewKnowledgeEntry(productA.ID, ScopeWorkUnit, "shared-platform-lib retry budget is 3",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-storefront-1"}, at)
	if err != nil {
		t.Fatal(err)
	}
	promotedA, _, err := Promote(discoveryA, ScopeProduct, "storefront-lead@example", at)
	if err != nil {
		t.Fatal(err)
	}

	discoveryB, err := NewKnowledgeEntry(productB.ID, ScopeWorkUnit, "shared-platform-lib retry budget is 5",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-checkout-1"}, at)
	if err != nil {
		t.Fatal(err)
	}
	promotedB, _, err := Promote(discoveryB, ScopeProduct, "checkout-lead@example", at)
	if err != nil {
		t.Fatal(err)
	}

	return &fakeStore{
		products: map[string]Product{productA.ID: productA, productB.ID: productB},
		knowledge: map[string][]KnowledgeEntry{
			productA.ID: {promotedA},
			productB.ID: {promotedB},
		},
	}
}

func TestCompileContextIsolatesOverlappingProducts(t *testing.T) {
	store := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}

	context, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeProduct})
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Knowledge) != 1 {
		t.Fatalf("expected exactly one knowledge entry for storefront, got %d", len(context.Knowledge))
	}
	if context.Knowledge[0].Statement != "shared-platform-lib retry budget is 3" {
		t.Fatalf("storefront's context leaked checkout's claim: %q", context.Knowledge[0].Statement)
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
	store := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: "galaxy"}); err == nil {
		t.Fatal("an unrecognized scope should be refused")
	}
}

func TestCompileContextOmitsKnowledgeAboveRequestedScope(t *testing.T) {
	store := twoOverlappingProducts(t)
	productAID, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	context, err := CompileContext(store, ContextRequest{ProductID: productAID, Scope: ScopeWorkUnit})
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Knowledge) != 0 {
		t.Fatalf("a work_unit-scoped request should not see product-scoped knowledge, got %d entries", len(context.Knowledge))
	}
}
