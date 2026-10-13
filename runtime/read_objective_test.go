package runtime

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestObjectiveGraphReadsExplicitBindingAcrossReopenAndRefusesAmbiguity(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	makeGraph := func(name string) orchestration.WorkGraph {
		graph, err := (orchestration.WorkGraphProposal{Name: name, Revision: 1, Units: graphDiamond()}).Compose("acme/repo", "claude", "operator", time.Unix(1700000000, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		graph.ObjectivePlan = &domain.PlanRef{ID: "objective-plan", Revision: 1, Digest: "exact-plan-digest"}
		if _, _, err := writer.AdoptWorkGraphRevision(graph); err != nil {
			t.Fatal(err)
		}
		return graph
	}
	graph := makeGraph("first-objective")
	for range 2 {
		reader, err := OpenReadStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		got, found, err := reader.ObjectiveGraph("objective-plan")
		if err != nil || !found || got.ID != graph.ID || *got.ObjectivePlan != *graph.ObjectivePlan {
			t.Fatalf("binding after reopen: graph=%+v found=%t err=%v", got, found, err)
		}
		view, err := reader.WorkGraphStatus(graph.ID, time.Now().UTC())
		if err != nil || view.ObjectivePlan == nil || *view.ObjectivePlan != *graph.ObjectivePlan {
			t.Fatalf("projected graph lost its own exact plan binding: %+v err=%v", view.ObjectivePlan, err)
		}
		if _, found, err := reader.ObjectiveGraph("missing"); err != nil || found {
			t.Fatalf("missing binding: found=%t err=%v", found, err)
		}
		reader.Close()
	}
	makeGraph("second-objective")
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, found, err := reader.ObjectiveGraph("objective-plan"); err == nil || found {
		t.Fatalf("ambiguous objective binding accepted: found=%t err=%v", found, err)
	}
}
