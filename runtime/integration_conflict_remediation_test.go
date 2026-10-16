package runtime

// #475 bounded acceptance, Stage D: once a real textual conflict Finding
// (Scenario B, TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff)
// is delivered into the blamed producer's own batch inbox (#579's
// deliverIntegrationConflicts), does the real runtime's own WorkGraph
// lifecycle legitimately reactivate that producer - give it a new attempt,
// a new run, a new handoff - from that delivery alone?
//
// This test proves the answer executably rather than by code reading alone.
// It is NOT a defect regression: #472's own WorkUnit projection
// (orchestration/workgraph_projection.go) deliberately treats activation as
// write-once and invalidation (the only existing re-run trigger) as an
// upstream-output fact, never a message-presence fact - "re-performing
// invalidated work is remediation, and remediation is deliberately not in
// #472". #508's decision continuation only resumes a still-live run blocked
// on its OWN emitted decision_request; #474's review remediation is keyed to
// a published GitHub PR's REQUEST_CHANGES verdict. Neither reads a
// producer's own #473 inbox. This test is the durable, executable evidence
// for that gap and a permanent guard on the frozen invariant it rests on: a
// delivered Finding, by itself, is information, never an implicit
// authorization to rerun a producer. Legitimate governed remediation remains
// #480/#508's open scope to add as an explicit, decision-gated transition -
// never an automatic one triggered by a message's mere presence.
import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestDeliveredConflictFindingNeverReactivatesTheBlamedProducer(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "shared.go", content: "package candidate\nconst Shared = \"a\"\n"},
		"b": {path: "shared.go", content: "package candidate\nconst Shared = \"b\"\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))

	// Drive to the same parked-conflict state Scenario B proves (same loop
	// as TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff),
	// then one more tick for delivery to catch up.
	var cRunID string
	var parked bool
	for range 60 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if c := fixture.graph(view.GraphID).unit(t, "c"); c.RunID != "" {
			cRunID = c.RunID
		}
		if cRunID == "" {
			continue
		}
		events, err := fixture.store.Events(cRunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type != EventRunWaiting {
				continue
			}
			var payload dispositionRecord
			if err := decodeJSON(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Reason == ReasonDeterministicFailureUnchanged {
				parked = true
			}
		}
		if parked {
			break
		}
	}
	if !parked {
		t.Fatal("the blocked integration unit never parked on its unchanged deterministic conflict")
	}
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	before := fixture.graph(view.GraphID)
	a, b := before.unit(t, "a"), before.unit(t, "b")
	if a.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("producer a did not complete: %s", a.State)
	}
	if b.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("producer b did not complete: %s", b.State)
	}
	cRun, ok := storedRun(t, fixture.phase8Fixture, before.unit(t, "c").RunID)
	if !ok || cRun.Orchestration == nil {
		t.Fatal("integrator run not found or not orchestrated")
	}
	messages, err := fixture.store.ScopeMessages(cRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var findingUnit string
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding {
			findingUnit = message.Route.Unit
		}
	}
	if findingUnit != "a" && findingUnit != "b" {
		t.Fatalf("integrator's finding routes to %q, not one of the conflicting units", findingUnit)
	}
	blamedID := findingUnit
	blamed := a
	if blamedID == "b" {
		blamed = b
	}
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	inboxBefore := blamedUnitInbox(t, fixture, blamedRun)
	if len(inboxBefore.Inbox.Findings) == 0 {
		t.Fatal("blamed producer has no delivered finding live in its own inbox; precondition for Stage D not met")
	}
	activationsBefore, err := fixture.store.WorkUnitActivations(view.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	handoffsBefore, err := fixture.store.RunHandoffs(blamed.RunID)
	if err != nil {
		t.Fatal(err)
	}
	runsBefore, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}

	// The only thing that has happened since the producer completed is that
	// its own Finding landed in its own inbox. Nothing else acts on it: no
	// operator decision, no plan, no new WorkGraph edge. Give the real
	// scheduler many more passes to prove that fact alone, by itself, never
	// moves the blamed unit.
	for range 20 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}

	after := fixture.graph(view.GraphID)
	if len(after.Frontier) != 0 {
		t.Fatalf("a producer's own delivered finding put unit(s) back in the frontier: %v", after.Frontier)
	}
	afterBlamed := after.unit(t, blamedID)
	if afterBlamed.RunID != blamed.RunID {
		t.Fatalf("blamed producer %s got a new run (%s -> %s) from a delivered finding alone", blamedID, blamed.RunID, afterBlamed.RunID)
	}
	if afterBlamed.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("blamed producer %s left its completed state (%s) from a delivered finding alone", blamedID, afterBlamed.State)
	}
	activationsAfter, err := fixture.store.WorkUnitActivations(view.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	if activationsAfter[blamedID] != activationsBefore[blamedID] {
		t.Fatalf("blamed producer %s's activation changed from a delivered finding alone: %+v -> %+v",
			blamedID, activationsBefore[blamedID], activationsAfter[blamedID])
	}
	handoffsAfter, err := fixture.store.RunHandoffs(blamed.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffsAfter) != len(handoffsBefore) {
		t.Fatalf("blamed producer %s's run admitted %d handoffs after delivery, had %d before - a new handoff with no new work",
			blamedID, len(handoffsAfter), len(handoffsBefore))
	}
	runsAfter, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runsAfter) != len(runsBefore) {
		t.Fatalf("20 ticks after delivery created %d new run(s); a delivered finding must never by itself create one", len(runsAfter)-len(runsBefore))
	}
	// The Finding itself is untouched: still live, still unconsumed, exactly
	// as #473/#475 leave it - information a human or a future governed #480
	// decision may act on, never a fact this runtime resolves on its own.
	inboxAfter := blamedUnitInbox(t, fixture, blamedRun)
	if len(inboxAfter.Inbox.Findings) != len(inboxBefore.Inbox.Findings) {
		t.Fatalf("delivered finding count changed with no consuming action: %d -> %d", len(inboxBefore.Inbox.Findings), len(inboxAfter.Inbox.Findings))
	}
}
