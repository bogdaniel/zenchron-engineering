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
