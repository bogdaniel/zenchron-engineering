package runtime

// #475 review P1: AdmitDeliveredFinding must enforce the SAME #473
// per-scope message bound (orchestration.MaxScopeMessages) ordinary
// orchestration.AdmitMessages enforces for a worker's own report, atomically
// against the destination scope it is actually writing into - and an
// idempotent repeat of a delivery already present must never be refused
// merely because the scope has since filled up around it.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

var deliveryTestNow = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// fillerMessage is a minimally valid, distinct state_update - the cheapest
// kind the #473 schema accepts, with no further required members - used only
// to occupy a scope's message count. runID is a REAL run (orchestration_messages.run_id
// is foreign-keyed to runs(id)); it never has to be the run the SCOPE itself
// belongs to, since scope carries no such constraint.
func fillerMessage(scope, runID string, index int) orchestration.EngineeringMessage {
	return orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion,
		ID:            fmt.Sprintf("message-filler-%s-%d", scope, index),
		Scope:         scope,
		Kind:          orchestration.KindStateUpdate,
		Source: orchestration.MessageSource{
			Unit: "issue-1", RunID: runID, OperationID: "op-filler", Attempt: 1, Index: index, System: true,
		},
		Route:          orchestration.MessageRoute{Audience: orchestration.AudienceScope},
		Body:           "filler",
		DocumentSHA256: strings.Repeat("a", 64),
		AdmittedAt:     deliveryTestNow.Add(time.Duration(index)),
	}
}

func deliveredFixture(scope, runID string) orchestration.EngineeringMessage {
	return orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion,
		ID:            "message-delivered-fixture-" + scope,
		Scope:         scope,
		Kind:          orchestration.KindFinding,
		Source: orchestration.MessageSource{
			Unit: "c", RunID: runID, OperationID: "op-integrator", Attempt: 1, Index: 0, System: true,
		},
		Route:          orchestration.MessageRoute{Audience: orchestration.AudienceUnit, Unit: "issue-5"},
		Subject:        &orchestration.MessageSubject{Handoff: "handoff-1", Owner: "issue-5", Revision: orchestration.HandoffSubject{CandidateRevision: "c1", CandidateTree: "t1"}},
		Category:       orchestration.FindingDefect,
		Body:           "shared.go conflicts",
		DocumentSHA256: strings.Repeat("a", 64),
		AdmittedAt:     deliveryTestNow,
	}
}

func seedMessages(t *testing.T, store *SQLiteOperationStore, scope, runID string, count int) {
	t.Helper()
	for i := range count {
		if err := store.AdmitMessages([]orchestration.EngineeringMessage{fillerMessage(scope, runID, i)}); err != nil {
			t.Fatalf("seeding filler message %d: %v", i, err)
		}
	}
}

// deliveryBoundFixture gives a real run id (FK-satisfying) to bind
// synthetic messages under, without the full WorkGraph/fleet machinery
// these capacity-bound tests have nothing to do with.
func deliveryBoundFixture(t *testing.T) (*fleetFixture, string) {
	t.Helper()
	fixture, _ := newMessagingFixture(t)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	return fixture, view.Items[0].RunID
}

func TestAdmitDeliveredFindingAllowsExactlyUpToTheScopeBound(t *testing.T) {
	fixture, runID := deliveryBoundFixture(t)
	const scope = "producer-scope-at-bound"
	seedMessages(t, fixture.store, scope, runID, orchestration.MaxScopeMessages-1)
	inserted, err := fixture.store.AdmitDeliveredFinding(deliveredFixture(scope, runID))
	if err != nil {
		t.Fatalf("delivery at exactly the bound was refused: %v", err)
	}
	if !inserted {
		t.Fatal("delivery at exactly the bound reported no insertion")
	}
}

func TestAdmitDeliveredFindingRefusesPastTheScopeBound(t *testing.T) {
	fixture, runID := deliveryBoundFixture(t)
	const scope = "producer-scope-past-bound"
	seedMessages(t, fixture.store, scope, runID, orchestration.MaxScopeMessages)
	if _, err := fixture.store.AdmitDeliveredFinding(deliveredFixture(scope, runID)); err == nil {
		t.Fatal("a delivery past the scope's message bound was admitted")
	} else if !strings.Contains(err.Error(), "message bound") {
		t.Fatalf("refusal reason = %q, want it naming the message bound", err)
	}
}

func TestAdmitDeliveredFindingIsIdempotentEvenWhenTheScopeHasSinceFilledUp(t *testing.T) {
	fixture, runID := deliveryBoundFixture(t)
	const scope = "producer-scope-idempotent"
	message := deliveredFixture(scope, runID)
	inserted, err := fixture.store.AdmitDeliveredFinding(message)
	if err != nil || !inserted {
		t.Fatalf("first delivery: inserted=%t err=%v", inserted, err)
	}
	// Fill the scope up to and past the bound AFTER the delivery already
	// landed - a real sequence, since ordinary handoff/message admission for
	// the producer's own work keeps happening independently of delivery.
	seedMessages(t, fixture.store, scope, runID, orchestration.MaxScopeMessages)
	inserted, err = fixture.store.AdmitDeliveredFinding(message)
	if err != nil {
		t.Fatalf("a repeated delivery of an already-admitted finding was refused once its scope filled up: %v", err)
	}
	if inserted {
		t.Fatal("a repeated delivery of an already-admitted finding reported a fresh insertion")
	}
}
