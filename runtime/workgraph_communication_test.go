package runtime

// #473's WorkGraph-scoped delivery seam: a WorkGraph (#472) unit executes as
// its own one-item batch (activateWorkUnit), so it already has the #473
// message channel through the same batchMessageScope/admitOrchestratedMessages
// path a direct batch uses. What this proves is the boundary docs/workgraph.md
// states: units of one graph do NOT share a message scope with each other, so
// a WorkUnit only ever sees messages it is entitled to - its own.

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestAWorkGraphUnitOnlySeesItsOwnScopedMessages is the #473 WorkGraph delivery
// seam acceptance: A's StateUpdate is admitted into A's own one-item batch
// scope and never reaches B's, proving isolation; and a message from A that
// NAMES B as a target is refused, because B is not a unit of A's scope - the
// guard that keeps one graph's units from becoming an ad-hoc chat bus.
func TestAWorkGraphUnitOnlySeesItsOwnScopedMessages(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	runA, runB := issueRunOf(t, fixture, graphUnitA), issueRunOf(t, fixture, graphUnitB)
	worker.say(runA, messageDocument(t, orchestration.MessageDraft{Kind: orchestration.KindStateUpdate, Body: "schema drafted"}), false)
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	fixture.driveGraph(supervisor, view.GraphID, nil)

	batchA, batchB := batchOf(t, fixture, view.GraphID, "a"), batchOf(t, fixture, view.GraphID, "b")
	if batchA == batchB {
		t.Fatalf("units a and b share one batch/scope: %s", batchA)
	}
	messagesA, err := fixture.store.ScopeMessages(batchA)
	if err != nil || len(messagesA) != 1 || messagesA[0].Source.RunID != runA {
		t.Fatalf("a's scope messages = %+v (%v)", messagesA, err)
	}
	messagesB, err := fixture.store.ScopeMessages(batchB)
	if err != nil || len(messagesB) != 0 {
		t.Fatalf("b's scope carries a's message: %+v (%v)", messagesB, err)
	}
	// B's rendered inbox - what its next invocation would actually be shown -
	// is equally empty: the isolation holds at the delivery seam, not only in
	// the store.
	run, _, err := fixture.store.Run(runB)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := communicationContext(fixture.store, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "schema drafted") {
		t.Fatalf("b was rendered a's message: %s", rendered)
	}

	// THE BREAK: A tries to address a message to B by name. B is not a unit of
	// A's scope (A's one-item batch names only "a"'s unit), so this is refused,
	// not silently delivered cross-graph.
	simulateInvocation(t, fixture, runA, "op-a-cross-unit", messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindCollaborationRequest, Target: orchestration.BatchItemUnit(graphUnitB), Purpose: "p", Body: "x",
	}))
	admitBatch(t, fixture, batchA)
	refusals, err := fixture.store.ScopeMessageRefusals(batchA)
	if err != nil || len(refusals) != 1 {
		t.Fatalf("the cross-unit target was not refused durably: %v (%v)", refusals, err)
	}
	var reason string
	for _, r := range refusals {
		reason = r
	}
	if !strings.Contains(reason, "not another unit of scope") {
		t.Fatalf("refusal reason = %q, want it naming the scope boundary", reason)
	}
	if again, err := fixture.store.ScopeMessages(batchA); err != nil || len(again) != 1 {
		t.Fatalf("a refused cross-unit message was admitted: %+v (%v)", again, err)
	}
}
