package runtime

// #475 B1 acceptance: the delivered copy of an integration unit's conflict
// Finding, over the real supervisor/scheduler/store/WorkGraph fleet already
// used by workgraph_integration_test.go. TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff
// proves the three core facts (delivered to the exact producer, not the
// unrelated one, exact subject preserved); this file covers the remaining
// required scenarios: idempotent reconciliation, restart recovery, staleness
// once the blamed producer's own output is superseded, and that a dependent
// of the blocked integrator is never activated early.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// conflictedIntegrationFixture adopts two producers that conflict on the same
// file and an integrator depending on both (extra units, if any, are added to
// the same graph - e.g. a dependent of the integrator), and drives it until
// the integrator's run parks on its unchanged deterministic conflict.
func conflictedIntegrationFixture(t *testing.T, extra ...orchestration.WorkUnit) (*fleetFixture, *Supervisor, WorkGraphView, string) {
	t.Helper()
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "shared.go", content: "package candidate\nconst Shared = \"a\"\n"},
		"b": {path: "shared.go", content: "package candidate\nconst Shared = \"b\"\n"},
	}}
	supervisor := fixture.supervisor()
	units := append(integrationGraphUnits(issueA, issueB, issueC), extra...)
	view := fixture.adoptGraph(supervisor, "claude", 1, units)
	runID := driveUntilParkedOnUnchangedConflict(t, fixture, supervisor, view.GraphID, "c")
	// Message admission and delivery both lag the tick that parked the run by
	// one pass (admitOrchestratedMessages/deliverIntegrationConflicts read the
	// batch state a pass STARTED with) - exactly the gap
	// TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff already
	// documents and waits out the same way.
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fixture, supervisor, view, runID
}

func driveUntilParkedOnUnchangedConflict(t *testing.T, fixture *fleetFixture, supervisor *Supervisor, graphID, unitID string) string {
	t.Helper()
	var runID string
	var parked bool
	for range 60 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if u := fixture.graph(graphID).unit(t, unitID); u.RunID != "" {
			runID = u.RunID
		}
		if runID == "" {
			continue
		}
		events, err := fixture.store.Events(runID)
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
		t.Fatalf("unit %s never parked on its unchanged deterministic conflict", unitID)
	}
	return runID
}

// blamedProducer reads the integrator's own locally-admitted conflict
// Finding and returns the exact blamed unit's WorkGraphUnitView.
func blamedProducer(t *testing.T, fixture *fleetFixture, view WorkGraphView, integratorRunID string) WorkGraphUnitView {
	t.Helper()
	run, ok := storedRun(t, fixture.phase8Fixture, integratorRunID)
	if !ok || run.Orchestration == nil {
		t.Fatal("integrator run not found or not orchestrated")
	}
	messages, err := fixture.store.ScopeMessages(run.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding {
			return fixture.graph(view.GraphID).unit(t, message.Route.Unit)
		}
	}
	t.Fatal("integrator batch holds no conflict finding")
	return WorkGraphUnitView{}
}

// integratorConflictFinding reads the ONE local Finding an integration
// unit's own conflict admitted into ITS OWN batch scope - the original
// record delivery copies from, never a delivered copy itself.
func integratorConflictFinding(t *testing.T, fixture *fleetFixture, integratorRunID string) orchestration.EngineeringMessage {
	t.Helper()
	messages, err := fixture.store.ScopeMessages(storedBatchID(t, fixture, integratorRunID))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding {
			return message
		}
	}
	t.Fatal("integrator batch holds no conflict finding")
	return orchestration.EngineeringMessage{}
}

// deliveredFindingsOf is every Finding in scope bound to subjectHandoff - in
// a PRODUCER's own scope, that is always a delivered copy: the producer
// never authors a Finding about its own output, and the original record
// lives only in the integrator's own scope. A test uses this to prove
// delivery ran exactly once regardless of how many reconcile passes follow.
func deliveredFindingsOf(t *testing.T, fixture *fleetFixture, scope, subjectHandoff string) []orchestration.EngineeringMessage {
	t.Helper()
	messages, err := fixture.store.ScopeMessages(scope)
	if err != nil {
		t.Fatal(err)
	}
	var found []orchestration.EngineeringMessage
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding && message.Subject != nil && message.Subject.Handoff == subjectHandoff {
			found = append(found, message)
		}
	}
	return found
}

// TestIntegrationConflictDeliveryIsIdempotentAcrossReconcilePasses is
// required scenario 4: repeated reconciliation over an already-delivered
// conflict creates no duplicate delivered message.
func TestIntegrationConflictDeliveryIsIdempotentAcrossReconcilePasses(t *testing.T) {
	fixture, supervisor, view, cRunID := conflictedIntegrationFixture(t)
	blamed := blamedProducer(t, fixture, view, cRunID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	originalFinding := integratorConflictFinding(t, fixture, cRunID)
	first := deliveredFindingsOf(t, fixture, blamedRun.Orchestration.BatchID, originalFinding.Subject.Handoff)
	if len(first) != 1 {
		t.Fatalf("after the first delivery pass, producer scope holds %d delivered findings, want 1", len(first))
	}
	for range 5 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	again := deliveredFindingsOf(t, fixture, blamedRun.Orchestration.BatchID, originalFinding.Subject.Handoff)
	if len(again) != 1 {
		t.Fatalf("after 5 more reconcile passes, producer scope holds %d delivered findings, want exactly 1", len(again))
	}
	if again[0].ID != first[0].ID {
		t.Fatalf("repeated reconciliation produced a different delivered record: %s then %s", first[0].ID, again[0].ID)
	}
}

// TestIntegrationConflictDeliveryFastPathSkipsAlreadyDeliveredFindings is
// #475 review P2: once delivered, a later reconcile pass must recognize the
// SAME conflict via the cheap existence check alone, never redo the full
// handoff lookup and write transaction for it. integrationConflictDeliverySkipped
// is the one seam that lets a test observe which path actually ran, the same
// pattern afterMergeBeforeHeadRead/afterCommitGates already use elsewhere in
// this package.
func TestIntegrationConflictDeliveryFastPathSkipsAlreadyDeliveredFindings(t *testing.T) {
	fixture, supervisor, view, cRunID := conflictedIntegrationFixture(t)
	blamed := blamedProducer(t, fixture, view, cRunID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	originalFinding := integratorConflictFinding(t, fixture, cRunID)
	delivered := deliveredFindingsOf(t, fixture, blamedRun.Orchestration.BatchID, originalFinding.Subject.Handoff)
	if len(delivered) != 1 {
		t.Fatalf("delivery did not land before the fast path is exercised: %d findings", len(delivered))
	}

	var skippedIDs []string
	integrationConflictDeliverySkipped = func(id string) { skippedIDs = append(skippedIDs, id) }
	defer func() { integrationConflictDeliverySkipped = nil }()

	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(61 * time.Second)

	if len(skippedIDs) == 0 {
		t.Fatal("a reconcile pass over an already-delivered conflict never engaged the fast already-delivered path")
	}
	found := false
	for _, id := range skippedIDs {
		if id == delivered[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the fast path fired for %v, not the actually-delivered finding %s", skippedIDs, delivered[0].ID)
	}
}

// TestIntegrationConflictDeliveryRefusesAConflictingRecordUnderTheSameID is
// #475 review P2's follow-up: the fast path must never trust a bare id
// match alone. A row corrupted to disagree with what this exact delivery
// would write (here, its scope) is recognized as a conflict, not silently
// accepted as proof the work is already done.
func TestIntegrationConflictDeliveryRefusesAConflictingRecordUnderTheSameID(t *testing.T) {
	fixture, supervisor, view, cRunID := conflictedIntegrationFixture(t)
	blamed := blamedProducer(t, fixture, view, cRunID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	originalFinding := integratorConflictFinding(t, fixture, cRunID)
	delivered := deliveredFindingsOf(t, fixture, blamedRun.Orchestration.BatchID, originalFinding.Subject.Handoff)
	if len(delivered) != 1 {
		t.Fatalf("delivery did not land before corrupting it: %d findings", len(delivered))
	}
	corruptDeliveredRowScope(t, fixture.store, delivered[0].ID)

	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(61 * time.Second)
	var sawConflict bool
	for _, problem := range report.Orchestration {
		if strings.Contains(problem, "already names a different record") {
			sawConflict = true
		}
	}
	if !sawConflict {
		t.Fatalf("a corrupted record under the delivery's own id was silently treated as already delivered: %+v", report.Orchestration)
	}
}

// corruptDeliveredRowScope simulates corruption or a digest collision: the
// durable row at id still exists, but no longer agrees with what delivery
// would write. There is no application-level UPDATE path for an insert-only
// message (by design), so this reaches around it deliberately, exactly as a
// test proving a guard against real corruption has to.
func corruptDeliveredRowScope(t *testing.T, store *SQLiteOperationStore, id string) {
	t.Helper()
	stored, found, err := store.MessageByID(id)
	if err != nil || !found {
		t.Fatalf("delivered row %s not found to corrupt: found=%t err=%v", id, found, err)
	}
	stored.Scope += "-corrupted"
	document, err := CanonicalJSON(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE orchestration_messages SET scope = ?, document = ? WHERE id = ?`, stored.Scope, string(document), id); err != nil {
		t.Fatal(err)
	}
}

// storedBatchID is the integrator run's own batch id, read through the
// durable store exactly as every other helper in this file does.
func storedBatchID(t *testing.T, fixture *fleetFixture, runID string) string {
	t.Helper()
	run, ok := storedRun(t, fixture.phase8Fixture, runID)
	if !ok || run.Orchestration == nil {
		t.Fatal("run not found or not orchestrated")
	}
	return run.Orchestration.BatchID
}

// TestIntegrationConflictDeliverySurvivesRestart is required scenario 6: a
// genuinely closed and reopened store replays to the identical delivered
// finding and recipient, never a second one.
func TestIntegrationConflictDeliverySurvivesRestart(t *testing.T) {
	fixture, _, view, cRunID := conflictedIntegrationFixture(t)
	blamed := blamedProducer(t, fixture, view, cRunID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	before, err := fixture.store.ScopeMessages(blamedRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeID string
	for _, m := range before {
		if m.Kind == orchestration.KindFinding {
			beforeID = m.ID
		}
	}
	if beforeID == "" {
		t.Fatal("no delivered finding present before restart")
	}

	reopen(t, fixture.phase8Fixture)
	restarted := fixture.supervisor()
	for range 10 {
		if _, err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	after, err := fixture.store.ScopeMessages(blamedRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var afterCount int
	var afterID string
	for _, m := range after {
		if m.Kind == orchestration.KindFinding {
			afterCount++
			afterID = m.ID
		}
	}
	if afterCount != 1 {
		t.Fatalf("after restart, producer scope holds %d delivered findings, want exactly 1", afterCount)
	}
	if afterID != beforeID {
		t.Fatalf("restart delivered a different finding: %s before, %s after", beforeID, afterID)
	}
}

// TestIntegrationConflictFindingBecomesStaleWhenItsProducerSupersedesOutput
// is required scenario 5: once the blamed producer's own admitted output is
// superseded, the SAME delivered finding - still bound to the handoff the
// conflict actually was about - must read as historical, not a live claim
// the producer's NEW output could be remediated against.
func TestIntegrationConflictFindingBecomesStaleWhenItsProducerSupersedesOutput(t *testing.T) {
	fixture, _, view, cRunID := conflictedIntegrationFixture(t)
	blamed := blamedProducer(t, fixture, view, cRunID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamed.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatal("blamed producer's run not found or not orchestrated")
	}
	liveInbox := blamedUnitInbox(t, fixture, blamedRun)
	if len(liveInbox.Inbox.Findings) == 0 {
		t.Fatalf("delivered finding is not live before supersession: %+v", liveInbox.Inbox)
	}
	deliveredID := liveInbox.Inbox.Findings[0].ID

	replaceAdmittedOutput(t, fixture, view.GraphID, blamed)

	staleInbox := blamedUnitInbox(t, fixture, blamedRun)
	if len(staleInbox.Inbox.Findings) != 0 {
		t.Fatalf("delivered finding is still live after its producer's output was superseded: %+v", staleInbox.Inbox)
	}
	if !containsMessageID(staleInbox.Inbox.StaleFindings, deliveredID) {
		t.Fatalf("delivered finding did not move to stale findings after supersession: %+v", staleInbox.Inbox)
	}
}

// TestIntegrationConflictNeverActivatesADependent is required scenario 7: a
// unit depending on a conflicted, parked integrator never gets a run, no
// matter how many passes follow.
func TestIntegrationConflictNeverActivatesADependent(t *testing.T) {
	const issueD = fleetFirstIssue + 3
	dependent := orchestration.WorkUnit{
		ID: "d", Purpose: "downstream of the integration", Issue: issueD,
		Role: domain.RoleImplementer, DependsOn: []string{"c"},
	}
	fixture, supervisor, view, _ := conflictedIntegrationFixture(t, dependent)
	for range 10 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		d := fixture.graph(view.GraphID).unit(t, "d")
		if d.RunID != "" || d.State != orchestration.UnitBlocked {
			t.Fatalf("dependent d is %s with run %q while the integrator is parked on an unresolved conflict", d.State, d.RunID)
		}
	}
}
