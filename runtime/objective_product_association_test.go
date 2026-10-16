package runtime

// Product -> WorkGraph identity (#471/#479 Step 2): AssociateProductGraph is
// the one call site that threads the operator's intake-time product choice
// into the existing #476 association store. These tests prove its three
// required behaviors directly against the real SQLite store: no product
// chosen associates nothing, an unknown product fails closed, and the
// association survives a restart and never leaks across products that share
// a repository.

import (
	"testing"
	"time"
)

func TestAssociateProductGraphPersistsAcrossRestart(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	graph := adoptedGraph(t, first, "objective-graph", 101, now)

	service := PlanService{Store: first, Clock: &fakeClock{now: now}}
	if err := service.AssociateProductGraph(productA.ID, graph.ID); err != nil {
		t.Fatal(err)
	}

	// A second handle on the SAME file is a restart: the association survives.
	owner, found, err := second.AssociatedProduct(graph.ID)
	if err != nil || !found || owner != productA.ID {
		t.Fatalf("associated product after restart = %q (found=%t err=%v), want %q", owner, found, err, productA.ID)
	}
}

func TestAssociateProductGraphNoProductChosenAssociatesNothing(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	graph := adoptedGraph(t, first, "objective-graph", 101, now)

	service := PlanService{Store: first, Clock: &fakeClock{now: now}}
	if err := service.AssociateProductGraph("", graph.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := first.AssociatedProduct(graph.ID); err != nil || found {
		t.Fatalf("empty product id should associate nothing: found=%t err=%v", found, err)
	}
}

func TestAssociateProductGraphFailsClosedOnUnknownProduct(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	graph := adoptedGraph(t, first, "objective-graph", 101, now)

	service := PlanService{Store: first, Clock: &fakeClock{now: now}}
	if err := service.AssociateProductGraph("product-never-adopted", graph.ID); err == nil {
		t.Fatal("an unknown product should refuse the association rather than silently skip it")
	}
	if _, found, err := first.AssociatedProduct(graph.ID); err != nil || found {
		t.Fatalf("a refused association must not be partially recorded: found=%t err=%v", found, err)
	}
}

func TestAssociateProductGraphTwoProductsSharingRepositoryStayIsolated(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	productB := composeProduct(t, "checkout", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AdoptProductRevision(productB); err != nil {
		t.Fatal(err)
	}
	graphX := adoptedGraph(t, first, "objective-graph-x", 101, now)
	graphY := adoptedGraph(t, first, "objective-graph-y", 102, now)

	service := PlanService{Store: first, Clock: &fakeClock{now: now}}
	if err := service.AssociateProductGraph(productA.ID, graphX.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.AssociateProductGraph(productB.ID, graphY.ID); err != nil {
		t.Fatal(err)
	}

	graphsA, _, err := first.AssociatedGraphs(productA.ID, 0, 10)
	if err != nil || len(graphsA) != 1 || graphsA[0] != graphX.ID {
		t.Fatalf("product A associated graphs = %v (err=%v), want only %q", graphsA, err, graphX.ID)
	}
	graphsB, _, err := first.AssociatedGraphs(productB.ID, 0, 10)
	if err != nil || len(graphsB) != 1 || graphsB[0] != graphY.ID {
		t.Fatalf("product B associated graphs = %v (err=%v), want only %q", graphsB, err, graphY.ID)
	}

	// The same repository backs both products; a graph already claimed by one
	// is still refused to the other rather than silently reassigned.
	if err := service.AssociateProductGraph(productB.ID, graphX.ID); err == nil {
		t.Fatal("a graph already claimed by product A should refuse product B's claim")
	}
}

// TestBindObjectivePlanAssociatesItsProductWhenChosen proves the full call
// path objective.go's proposePlanAndGraph drives: BindObjectivePlan compiles
// the real objective WorkGraph, and AssociateProductGraph then claims that
// EXACT graph id for the product chosen at intake - not a synthetic one.
func TestBindObjectivePlanAssociatesItsProductWhenChosen(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	now := fixture.clock.Now()
	productA := composeProduct(t, "storefront", 1, []string{"acme/repo"}, now)
	if _, _, err := fixture.store.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}

	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.AssociateProductGraph(productA.ID, graphID); err != nil {
		t.Fatal(err)
	}

	owner, found, err := fixture.store.AssociatedProduct(graphID)
	if err != nil || !found || owner != productA.ID {
		t.Fatalf("associated product = %q (found=%t err=%v), want %q", owner, found, err, productA.ID)
	}
}
