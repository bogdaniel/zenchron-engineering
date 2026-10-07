package runtime

// The durable half of the WorkGraph (#472), proved on its own: a revision is
// immutable, an activation is written once, and the current revision is the
// highest one adopted - across two independent handles on one database, so no
// process-local mutex can be what makes these pass.

import (
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestTheGraphStoreIsAppendOnlyAcrossProcesses proves the durable half on its
// own, across two independent handles on one database: a revision is immutable,
// a unit is activated at most once, and the current revision is the highest one.
func TestTheGraphStoreIsAppendOnlyAcrossProcesses(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	revision := func(store *SQLiteOperationStore, number int, units []orchestration.WorkUnit) (orchestration.WorkGraph, bool, error) {
		graph, err := orchestration.WorkGraphProposal{Name: "m2-o1", Revision: number, Units: units}.
			Compose("acme/repo", "claude", "operator@example", now)
		if err != nil {
			t.Fatal(err)
		}
		return store.AdoptWorkGraphRevision(graph)
	}
	adopted, created, err := revision(first, 1, graphDiamond())
	if err != nil || !created {
		t.Fatalf("adopt: created=%t err=%v", created, err)
	}
	// The SAME revision resubmitted through ANOTHER handle is found, not
	// rewritten, even though it was composed at a different moment.
	again, created, err := revision(second, 1, graphDiamond())
	if err != nil || created || again.ID != adopted.ID {
		t.Fatalf("resubmission: created=%t err=%v", created, err)
	}
	// A DIFFERENT revision 1 is a conflict rather than an overwrite.
	altered := graphDiamond()
	altered[3].DependsOn = []string{"b"}
	if _, _, err := revision(second, 1, altered); err == nil || !strings.Contains(err.Error(), "a revision is immutable") {
		t.Fatalf("err = %v, want a refusal to rewrite an adopted revision", err)
	}
	// The current revision is the highest adopted one.
	if _, _, err := revision(second, 2, altered); err != nil {
		t.Fatal(err)
	}
	current, found, err := first.WorkGraph(adopted.ID)
	if err != nil || !found || current.Revision != 2 {
		t.Fatalf("current revision = %d (found=%t err=%v)", current.Revision, found, err)
	}
	if _, found, err := second.WorkGraphRevision(adopted.ID, 1); err != nil || !found {
		t.Fatalf("revision 1 was lost: found=%t err=%v", found, err)
	}

	// An activation needs a durable batch, and is written at most once.
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion,
		Repository: "acme/repo", AgentID: "claude", CreatedAt: now,
		Items: []orchestration.BatchItem{{Issue: graphUnitA, RunID: "run-a"}}}
	if batch.ID, err = orchestration.BatchID("acme/repo", "claude", []int{graphUnitA}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.CreateOrchestrationBatch(batch); err != nil {
		t.Fatal(err)
	}
	activation := WorkUnitActivation{GraphID: adopted.ID, UnitID: "a", BatchID: batch.ID,
		RunID: "run-a", InputsDigest: "digest-a", ActivatedAt: now}
	claimed, err := first.ActivateWorkUnit(activation)
	if err != nil || !claimed {
		t.Fatalf("activate: claimed=%t err=%v", claimed, err)
	}
	// A SECOND writer replaying the same unit claims nothing, and cannot
	// change where the first activation pointed.
	racing := activation
	racing.RunID, racing.InputsDigest = "run-second", "digest-second"
	claimed, err = second.ActivateWorkUnit(racing)
	if err != nil || claimed {
		t.Fatalf("a replayed activation claimed the unit again: claimed=%t err=%v", claimed, err)
	}
	activations, err := second.WorkUnitActivations(adopted.ID)
	if err != nil || len(activations) != 1 || activations["a"].RunID != "run-a" || activations["a"].InputsDigest != "digest-a" {
		t.Fatalf("activations = %+v (%v)", activations, err)
	}
	// An activation naming no batch, run or digest is refused outright.
	for name, broken := range map[string]WorkUnitActivation{
		"no unit":   {GraphID: adopted.ID, BatchID: batch.ID, RunID: "r", InputsDigest: "d", ActivatedAt: now},
		"no run":    {GraphID: adopted.ID, UnitID: "b", BatchID: batch.ID, InputsDigest: "d", ActivatedAt: now},
		"no digest": {GraphID: adopted.ID, UnitID: "b", BatchID: batch.ID, RunID: "r", ActivatedAt: now},
		"no time":   {GraphID: adopted.ID, UnitID: "b", BatchID: batch.ID, RunID: "r", InputsDigest: "d"},
	} {
		if _, err := first.ActivateWorkUnit(broken); err == nil {
			t.Fatalf("an activation with %s was written", name)
		}
	}
}
