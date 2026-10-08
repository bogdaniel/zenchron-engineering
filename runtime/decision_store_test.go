package runtime

// Durable, authorized DecisionResolution persistence (#508), proved across
// two independent handles on one database exactly as the WorkGraph store is:
// no in-process mutex is what makes a hold or a resolution write-once.

import (
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

var decisionStoreNow = time.Unix(1700000100, 0).UTC()

func testHoldAuthority(actor string) orchestration.DecisionResolutionAuthority {
	return orchestration.DecisionResolutionAuthority{Actor: actor, AuthorityKind: orchestration.AuthorityKindOperator, Provenance: "local_control_endpoint"}
}

func testHold(t *testing.T, graphID, unitID, purpose string, at time.Time) orchestration.WorkUnitHold {
	t.Helper()
	id, err := orchestration.WorkUnitHoldID(graphID, unitID)
	if err != nil {
		t.Fatal(err)
	}
	return orchestration.WorkUnitHold{
		SchemaVersion: orchestration.WorkUnitHoldSchemaVersion, ID: id, GraphID: graphID, UnitID: unitID,
		Purpose: purpose, RequestedBy: testHoldAuthority("operator-1"), RequestedAt: at,
	}
}

func testResolution(t *testing.T, requestID, scope string, outcome orchestration.DecisionOutcome, reason string, at time.Time) orchestration.DecisionResolution {
	t.Helper()
	id, err := orchestration.DecisionResolutionID(requestID)
	if err != nil {
		t.Fatal(err)
	}
	return orchestration.DecisionResolution{
		SchemaVersion: orchestration.DecisionResolutionSchemaVersion, ID: id, RequestID: requestID, Scope: scope,
		Outcome: outcome, Reason: reason, Authority: testHoldAuthority("operator-1"), ResolvedAt: at,
	}
}

func allowOutcomeForStore() orchestration.DecisionOutcome {
	return orchestration.DecisionOutcome{Kind: orchestration.DecisionAllowDeny, Value: orchestration.DecisionAllow}
}

// TestPlaceWorkUnitHoldIsWriteOnceAcrossHandles proves a hold is placed at
// most once, ever: a resubmission across a SEPARATE store handle finds the
// hold already there, and a disagreeing second placement is refused rather
// than silently overwriting it.
func TestPlaceWorkUnitHoldIsWriteOnceAcrossHandles(t *testing.T) {
	_, first, second := openPair(t)
	hold := testHold(t, "graph-1", "deploy", "human sign-off before production deploy", decisionStoreNow)
	stored, placed, err := first.PlaceWorkUnitHold(hold)
	if err != nil || !placed || stored.ID != hold.ID {
		t.Fatalf("place: placed=%t err=%v", placed, err)
	}
	// A lost reply's retry, read through a DIFFERENT handle, finds the hold
	// already placed rather than a conflict or a second row.
	again, placed, err := second.PlaceWorkUnitHold(hold)
	if err != nil || placed || again.ID != hold.ID {
		t.Fatalf("resubmit: placed=%t err=%v", placed, err)
	}
	// A disagreeing SECOND placement on the same unit is refused.
	disagreeing := testHold(t, "graph-1", "deploy", "a completely different purpose", decisionStoreNow)
	if _, _, err := second.PlaceWorkUnitHold(disagreeing); err == nil || !strings.Contains(err.Error(), "placed at most once") {
		t.Fatalf("expected a disagreeing second placement to be refused, got %v", err)
	}
}

// TestWorkGraphHoldsExcludesResolvedUnits proves the exact seam
// SupervisorDependencies.WorkUnitHolds consumes: an unresolved hold is
// reported, and a resolved one disappears from the map the very next read -
// WorkGraph frontier recomputation, not this store, decides what that means
// for the unit.
func TestWorkGraphHoldsExcludesResolvedUnits(t *testing.T) {
	_, store, _ := openPair(t)
	held := testHold(t, "graph-1", "deploy", "sign-off", decisionStoreNow)
	free := testHold(t, "graph-1", "build", "sign-off", decisionStoreNow)
	for _, hold := range []orchestration.WorkUnitHold{held, free} {
		if _, _, err := store.PlaceWorkUnitHold(hold); err != nil {
			t.Fatal(err)
		}
	}
	waits, err := store.WorkGraphHolds("graph-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits["deploy"].Reference != held.ID || waits["build"].Reference != free.ID {
		t.Fatalf("expected both units held, got %+v", waits)
	}
	resolution := testResolution(t, free.ID, free.GraphID+":"+free.UnitID, allowOutcomeForStore(), "go ahead", decisionStoreNow)
	if _, inserted, err := store.InsertDecisionResolution(resolution); err != nil || !inserted {
		t.Fatalf("insert resolution: inserted=%t err=%v", inserted, err)
	}
	waits, err = store.WorkGraphHolds("graph-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 {
		t.Fatalf("expected exactly one unit still held after resolving the other, got %+v", waits)
	}
	if _, stillHeld := waits["build"]; stillHeld {
		t.Fatal("a resolved unit's hold was still reported")
	}
	if _, stillHeld := waits["deploy"]; !stillHeld {
		t.Fatal("an unrelated unit's hold disappeared along with the resolved one")
	}
}

// TestInsertDecisionResolutionIsWriteOnceAcrossHandles proves acceptance #8
// and #9 at the durable layer, across two independent handles: an identical
// retry after a lost reply finds the SAME row (idempotent, not a second
// write), and a conflicting second answer finds a row to disagree with rather
// than a free slot.
func TestInsertDecisionResolutionIsWriteOnceAcrossHandles(t *testing.T) {
	_, first, second := openPair(t)
	resolution := testResolution(t, "message-1", "batch-1", allowOutcomeForStore(), "ship it", decisionStoreNow)
	stored, inserted, err := first.InsertDecisionResolution(resolution)
	if err != nil || !inserted || stored.ID != resolution.ID {
		t.Fatalf("insert: inserted=%t err=%v", inserted, err)
	}
	// The identical retry, read through a DIFFERENT handle - simulating a
	// restarted process - finds the same row rather than writing a second one.
	again, inserted, err := second.InsertDecisionResolution(resolution)
	if err != nil || inserted || again != stored {
		t.Fatalf("retry: inserted=%t err=%v stored=%+v again=%+v", inserted, err, stored, again)
	}
	// A conflicting second resolution of the SAME request is detectable: the
	// row returned does not match what the second writer proposed, which is
	// exactly what lets the caller (Supervisor.ResolveDecision) refuse it
	// through orchestration.ResolveDecision instead of silently keeping
	// whichever write happened to land first.
	conflicting := testResolution(t, "message-1", "batch-1",
		orchestration.DecisionOutcome{Kind: orchestration.DecisionAllowDeny, Value: orchestration.DecisionDeny}, "changed my mind", decisionStoreNow)
	rowAfterConflict, insertedConflict, err := second.InsertDecisionResolution(conflicting)
	if err != nil || insertedConflict {
		t.Fatalf("conflicting insert: inserted=%t err=%v", insertedConflict, err)
	}
	if rowAfterConflict.Outcome != resolution.Outcome {
		t.Fatalf("the durable resolution changed under a conflicting write: %+v", rowAfterConflict)
	}
}

// TestDecisionResolutionSurvivesRestart proves acceptance #12: closing and
// reopening the store (a fresh handle on the same file, as a restarted
// `serve` would open) reproduces the same open/resolved projection.
func TestDecisionResolutionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	hold := testHold(t, "graph-1", "deploy", "sign-off", decisionStoreNow)
	if _, _, err := store.PlaceWorkUnitHold(hold); err != nil {
		t.Fatal(err)
	}
	resolution := testResolution(t, hold.ID, hold.GraphID+":"+hold.UnitID, allowOutcomeForStore(), "go ahead", decisionStoreNow)
	if _, inserted, err := store.InsertDecisionResolution(resolution); err != nil || !inserted {
		t.Fatalf("insert: inserted=%t err=%v", inserted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	waits, err := reopened.WorkGraphHolds("graph-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("a resolved hold reappeared after restart: %+v", waits)
	}
	stored, found, err := reopened.DecisionResolutionByRequestID(hold.ID)
	if err != nil || !found || stored.Outcome != resolution.Outcome {
		t.Fatalf("resolution did not survive restart: found=%t err=%v stored=%+v", found, err, stored)
	}
}
