package runtime

// WorkGraph-to-product association persistence (#476), proved against the
// real SQLite store: a graph belongs to at most one product, and both sides
// of an association must actually exist before it is accepted.

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
)

func adoptedGraph(t *testing.T, store *SQLiteOperationStore, name string, issue int, at time.Time) orchestration.WorkGraph {
	t.Helper()
	proposal := orchestration.WorkGraphProposal{Name: name, Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "a", Purpose: "do the work", Role: "implementer", Issue: issue},
	}}
	graph, err := proposal.Compose("acme/repo", "claude", "operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	adopted, _, err := store.AdoptWorkGraphRevision(graph)
	if err != nil {
		t.Fatal(err)
	}
	return adopted
}

func TestAssociateWorkGraphOneProductPerGraph(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	productB := composeProduct(t, "checkout", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AdoptProductRevision(productB); err != nil {
		t.Fatal(err)
	}
	graph := adoptedGraph(t, first, "launch-graph", 101, now)

	association, err := product.NewGraphAssociation(productA.ID, graph.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := first.AssociateWorkGraph(association); err != nil || !created {
		t.Fatalf("associate: created=%t err=%v", created, err)
	}
	// The SAME association, replayed through another handle, is a no-op.
	if created, err := second.AssociateWorkGraph(association); err != nil || created {
		t.Fatalf("idempotent re-association: created=%t err=%v", created, err)
	}
	// A DIFFERENT product claiming the same graph is refused, never a silent
	// takeover of which product's context the graph's units now resolve.
	conflicting, err := product.NewGraphAssociation(productB.ID, graph.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.AssociateWorkGraph(conflicting); err == nil {
		t.Fatal("associating an already-claimed graph with a different product should be refused")
	}
	owner, found, err := first.AssociatedProduct(graph.ID)
	if err != nil || !found || owner != productA.ID {
		t.Fatalf("associated product = %q (found=%t err=%v), want %q", owner, found, err, productA.ID)
	}
}

// TestAssociateWorkGraphRequiresBothSidesToExist is B5: a nonexistent graph
// or a nonexistent product is refused rather than stored as a disconnected
// identity nothing else can ever resolve.
func TestAssociateWorkGraphRequiresBothSidesToExist(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}

	missingGraph, err := product.NewGraphAssociation(productA.ID, "graph-never-adopted", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AssociateWorkGraph(missingGraph); err == nil {
		t.Fatal("associating a work graph that was never adopted should be refused")
	}

	graph := adoptedGraph(t, first, "orphan-graph", 102, now)
	missingProduct, err := product.NewGraphAssociation("product-never-adopted", graph.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AssociateWorkGraph(missingProduct); err == nil {
		t.Fatal("associating a product that was never adopted should be refused")
	}
}

// TestAssociatedGraphsPaginatesWithoutSilentTruncation proves a product with
// MORE associations than one page never loses one: paging through
// AssociatedGraphs with a small limit recovers every association exactly
// once, hasMore is true for every page but the last, and the LIMIT/OFFSET
// bound is a database-level query, not a result silently trimmed after a
// full read - the one thing the earlier maxAssociatedGraphs cap got wrong.
func TestAssociatedGraphsPaginatesWithoutSilentTruncation(t *testing.T) {
	_, store, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := store.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	const total = 201
	want := map[string]bool{}
	for i := range total {
		graph := adoptedGraph(t, store, "graph-"+string(rune('a'+i%26))+string(rune('0'+i/26)), 2000+i, now)
		association, err := product.NewGraphAssociation(productA.ID, graph.ID, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AssociateWorkGraph(association); err != nil {
			t.Fatal(err)
		}
		want[graph.ID] = true
	}

	const pageSize = 100
	got := map[string]bool{}
	var pages int
	for offset := 0; ; offset += pageSize {
		page, hasMore, err := store.AssociatedGraphs(productA.ID, offset, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, id := range page {
			if got[id] {
				t.Fatalf("graph %s returned on more than one page", id)
			}
			got[id] = true
		}
		if !hasMore {
			if len(page) == 0 || len(page) > pageSize {
				t.Fatalf("final page has %d entries, want 1..%d", len(page), pageSize)
			}
			break
		}
		if len(page) != pageSize {
			t.Fatalf("a page reporting hasMore must be full: got %d, want %d", len(page), pageSize)
		}
		if pages > total/pageSize+2 {
			t.Fatal("hasMore never became false - pagination never terminates")
		}
	}
	if pages != 3 {
		t.Fatalf("201 associations at page size 100 must take exactly 3 pages, took %d", pages)
	}
	if len(got) != total {
		t.Fatalf("paging recovered %d of %d associations - some were silently dropped", len(got), total)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("association %s was never returned by any page", id)
		}
	}
}
