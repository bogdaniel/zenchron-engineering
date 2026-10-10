package runtime

// The #476 acceptance fixture: two real products, "storefront" and "checkout",
// that share a repository concept (both depend on acme/shared-platform-lib)
// and both discover a claim about it independently. Proves zero cross-product
// knowledge leakage, zero cross-owner leakage WITHIN one product, that a
// WorkUnit's context compilation loads only its own minimum relevant state,
// and that all of it survives a restart - end to end through the real SQLite
// store, not a fake.

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/product"
)

type productFixture struct {
	product       product.Product
	graph         string
	ownerRef      string
	configuration product.ProductConfiguration
	discovered    product.KnowledgeEntry
	promoted      product.KnowledgeEntry
	promotion     product.KnowledgePromotion
}

func seedProductFixture(t *testing.T, store *SQLiteOperationStore, name string, issue int, repository, claim, authorizer string, at time.Time) productFixture {
	t.Helper()
	p := composeProduct(t, name, 1, []string{"acme/shared-platform-lib", repository}, at)
	if _, _, err := store.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	configuration := baseConfigurationRevision(t, p.ID, 1, at)
	if _, _, err := store.AdoptProductConfigurationRevision(configuration); err != nil {
		t.Fatal(err)
	}
	graph := adoptedGraph(t, store, name+"-graph", issue, at)
	association, err := product.NewGraphAssociation(p.ID, graph.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssociateWorkGraph(association); err != nil {
		t.Fatal(err)
	}
	ownerRef := graph.ID
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, ownerRef, claim,
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: ownerRef}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordKnowledgeEntry(discovered); err != nil {
		t.Fatal(err)
	}
	promoted, promotion, err := product.Promote(discovered, product.ScopeProduct, authorizer, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordPromotion(promoted, promotion); err != nil {
		t.Fatal(err)
	}
	return productFixture{
		product: p, graph: graph.ID, ownerRef: ownerRef, configuration: configuration,
		discovered: discovered, promoted: promoted, promotion: promotion,
	}
}

func TestTwoOverlappingProductsNeverLeakKnowledgeAcrossARestart(t *testing.T) {
	dir, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()

	storefront := seedProductFixture(t, first, "storefront", 201,
		"acme/storefront-web", "shared-platform-lib retry budget is 3", "storefront-lead@example", now)
	checkout := seedProductFixture(t, first, "checkout", 202,
		"acme/checkout-api", "shared-platform-lib retry budget is 5", "checkout-lead@example", now)

	// A THIRD handle on the same file is a full process restart.
	restarted, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })

	for _, store := range []*SQLiteOperationStore{first, second, restarted} {
		storefrontContext, err := product.CompileContext(store, product.ContextRequest{
			ProductID: storefront.product.ID, Scope: product.ScopeProduct,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !containsKnowledgeID(storefrontContext.Knowledge, storefront.promoted.ID) {
			t.Fatalf("storefront's context should contain its own promoted claim, got %+v", storefrontContext.Knowledge)
		}
		if containsKnowledgeID(storefrontContext.Knowledge, checkout.promoted.ID) {
			t.Fatal("storefront's context leaked checkout's promoted claim")
		}
		for _, entry := range storefrontContext.Knowledge {
			if entry.ProductID != storefront.product.ID {
				t.Fatalf("storefront's context leaked an entry owned by %q", entry.ProductID)
			}
		}

		checkoutContext, err := product.CompileContext(store, product.ContextRequest{
			ProductID: checkout.product.ID, Scope: product.ScopeProduct,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !containsKnowledgeID(checkoutContext.Knowledge, checkout.promoted.ID) {
			t.Fatalf("checkout's context should contain its own promoted claim, got %+v", checkoutContext.Knowledge)
		}
		if containsKnowledgeID(checkoutContext.Knowledge, storefront.promoted.ID) {
			t.Fatal("checkout's context leaked storefront's promoted claim")
		}

		// Both products share the repository concept, by construction, yet
		// neither context's knowledge or configuration crosses over.
		if storefrontContext.Configuration == nil || storefrontContext.Configuration.ProductID != storefront.product.ID {
			t.Fatal("storefront's context should carry its own configuration")
		}
		if checkoutContext.Configuration == nil || checkoutContext.Configuration.ProductID != checkout.product.ID {
			t.Fatal("checkout's context should carry its own configuration")
		}
	}

	// A WorkUnit scoped to work_unit, not product, sees its OWN raw discovery
	// and nothing else - the minimum relevant context, not everything the
	// product ever learned.
	narrow, err := product.CompileContext(second, product.ContextRequest{
		ProductID: storefront.product.ID, Scope: product.ScopeWorkUnit, OwnerRef: storefront.ownerRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Knowledge) != 1 || narrow.Knowledge[0].ID != storefront.discovered.ID {
		t.Fatalf("work_unit scope should see exactly its own directly authored entry, got %+v", narrow.Knowledge)
	}
	if narrow.Knowledge[0].Provenance.Type == product.ProvenancePromotion {
		t.Fatal("a work_unit-scoped read should not surface the product-scoped promoted copy")
	}

	// The graph association survives the restart too, naming the right product.
	owner, found, err := restarted.AssociatedProduct(storefront.graph)
	if err != nil || !found || owner != storefront.product.ID {
		t.Fatalf("graph association after restart: owner=%q found=%t err=%v", owner, found, err)
	}
}

// TestContextIsolatesTwoWorkUnitsWithinOneProduct is the real-store half of
// B3's "two units under same product remain isolated": a raw discovery is
// visible to its own owner alone, never to another owner compiling context
// for the SAME product and the SAME scope.
func TestContextIsolatesTwoWorkUnitsWithinOneProduct(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	storefront := seedProductFixture(t, first, "storefront", 301,
		"acme/storefront-web", "shared-platform-lib retry budget is 3", "storefront-lead@example", now)

	otherGraph := adoptedGraph(t, first, "storefront-second-graph", 302, now)
	otherAssociation, err := product.NewGraphAssociation(storefront.product.ID, otherGraph.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AssociateWorkGraph(otherAssociation); err != nil {
		t.Fatal(err)
	}
	otherDiscovery, err := product.NewKnowledgeEntry(storefront.product.ID, product.ScopeWorkUnit, otherGraph.ID,
		"an unrelated claim", product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: otherGraph.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(otherDiscovery); err != nil {
		t.Fatal(err)
	}

	context, err := product.CompileContext(first, product.ContextRequest{
		ProductID: storefront.product.ID, Scope: product.ScopeWorkUnit, OwnerRef: storefront.ownerRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsKnowledgeID(context.Knowledge, otherDiscovery.ID) {
		t.Fatal("one work unit's context leaked another work unit's raw discovery within the same product")
	}
	if !containsKnowledgeID(context.Knowledge, storefront.discovered.ID) {
		t.Fatal("a work unit's context should still contain its own raw discovery")
	}
}

// TestContextPinsAnExactConfigurationRevisionThroughTheRealStore is B4's
// real-store half: a caller that already knows its frozen revision number
// keeps seeing that exact revision after a newer one becomes current.
func TestContextPinsAnExactConfigurationRevisionThroughTheRealStore(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	storefront := seedProductFixture(t, first, "storefront", 401,
		"acme/storefront-web", "shared-platform-lib retry budget is 3", "storefront-lead@example", now)

	boundRevision := storefront.configuration.Revision
	next := baseConfigurationRevision(t, storefront.product.ID, boundRevision+1, now.Add(time.Hour))
	next.Settings = map[string]string{"tier": "premium"}
	if _, _, err := first.AdoptProductConfigurationRevision(next); err != nil {
		t.Fatal(err)
	}

	pinned, err := product.CompileContext(first, product.ContextRequest{
		ProductID: storefront.product.ID, Scope: product.ScopeProduct, ConfigurationRevision: &boundRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Configuration == nil || pinned.Configuration.Settings["tier"] != "standard" {
		t.Fatalf("a caller bound to revision %d should still see it after a newer one is adopted, got %+v",
			boundRevision, pinned.Configuration)
	}

	current, err := product.CompileContext(first, product.ContextRequest{
		ProductID: storefront.product.ID, Scope: product.ScopeProduct,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.Configuration == nil || current.Configuration.Settings["tier"] != "premium" {
		t.Fatalf("an unpinned caller should see the current revision, got %+v", current.Configuration)
	}
}

func containsKnowledgeID(entries []product.KnowledgeEntry, id string) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}
