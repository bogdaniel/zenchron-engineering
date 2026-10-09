package runtime

// Split out of workgraph_integration_test.go (#475 review N1): these test
// AdmitIntegratedHandoff's atomic SQLite admission predicate directly -
// the superseded-input race and the two named gaps a later review found in
// it - a distinct responsibility from the end-to-end WorkGraph acceptance
// scenarios the rest of that file covers.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// integratedHandoffInterleaveStore wraps the real store to inject a second,
// genuinely separate writer connection's competing admission between
// AdmitIntegratedHandoff's freshness check and its own insert (#475 review
// B3's "two database handles" ask) - the same technique
// interleavedCheckpointHandoffStore (orchestration_checkpoint_handoff_test.go)
// already proves for the pre-existing checkpoint-handoff race.
type integratedHandoffInterleaveStore struct {
	*SQLiteOperationStore
	beforeInsert func()
}

func (s integratedHandoffInterleaveStore) AdmitIntegratedHandoff(handoff orchestration.EngineeringHandoff, inputs orchestration.WorkUnitInputs) (bool, error) {
	s.beforeInsert()
	return s.SQLiteOperationStore.AdmitIntegratedHandoff(handoff, inputs)
}

func inputFor(unitID string, handoff orchestration.EngineeringHandoff) orchestration.WorkUnitInput {
	return orchestration.WorkUnitInput{UnitID: unitID, UnitOutput: orchestration.UnitOutput{
		HandoffID: handoff.ID, RunID: handoff.RunID,
		CandidateRevision: handoff.Subject.CandidateRevision, CandidateTree: handoff.Subject.CandidateTree,
		Outcome: orchestration.OutcomeCompleted,
	}}
}

// TestAdmitIntegratedHandoffLosesToSupersededInputBeforeInsert is Scenario
// B3: an upstream input superseded in the GAP between the live freshness
// check and the handoff INSERT must still lose, because the check and the
// write are the SAME SQLite statement - proven with a genuinely separate
// writer connection performing the superseding admission between them, not
// merely a stale read followed by a later write on one connection. (The
// non-superseded, successful case is already proven end to end by every
// other #475 scenario here - this test exercises only the race.)
//
// Two ordinary #470 batch items (not WorkGraph units) stand in for the
// producer and the integrator: AdmitIntegratedHandoff's own contract needs
// only valid run_id/batch_id foreign keys, which an ordinary completed item
// already provides, without any WorkGraph/ExecutionKind machinery this test
// does not need.
func TestAdmitIntegratedHandoffLosesToSupersededInputBeforeInsert(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(2))
	settled := fixture.drive(supervisor, view.BatchID)
	producerHandoffs, err := fixture.store.RunHandoffs(settled.Items[0].RunID)
	if err != nil || len(producerHandoffs) != 1 {
		t.Fatalf("producer item did not admit exactly one handoff: %d (%v)", len(producerHandoffs), err)
	}
	h1 := producerHandoffs[0]
	integratorHandoffs, err := fixture.store.RunHandoffs(settled.Items[1].RunID)
	if err != nil || len(integratorHandoffs) != 1 {
		t.Fatalf("integrator-stand-in item did not admit exactly one handoff: %d (%v)", len(integratorHandoffs), err)
	}
	// A SYNTHETIC second attempt against the same already-admitted run/batch
	// (valid FK anchors), never actually produced by the ordinary flow -
	// AdmitIntegratedHandoff itself decides whether it is admissible.
	synthetic := integratorHandoffs[0]
	synthetic.ID = "handoff-integrator-synthetic"

	inputs := orchestration.WorkUnitInputs{inputFor("a", h1)}
	writer, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	interleaved := false
	store := integratedHandoffInterleaveStore{SQLiteOperationStore: fixture.store, beforeInsert: func() {
		interleaved = true
		// A genuinely separate connection admits producer a's SECOND,
		// superseding handoff for the SAME run - the race window this
		// proves closed.
		superseding := h1
		superseding.ID = "handoff-a-superseding"
		superseding.AdmittedAt = h1.AdmittedAt.Add(time.Second)
		if ok, err := writer.AdmitHandoff(superseding); err != nil || !ok {
			t.Fatalf("competing admission did not land: ok=%t err=%v", ok, err)
		}
	}}
	inserted, err := store.AdmitIntegratedHandoff(synthetic, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("admission never reached the insert boundary")
	}
	if inserted {
		t.Fatal("the integrated handoff was admitted despite its input being superseded between the check and the insert")
	}
	// Exactly the one REAL handoff the ordinary flow admitted - the
	// synthetic, superseded attempt never landed.
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 1 {
		t.Fatalf("run %s has %d handoffs after a refused synthetic admission, want exactly 1 (%v)", synthetic.RunID, len(handoffs), err)
	}
}

// TestAdmitIntegratedHandoffRefusesAMissingReferencedHandoff is #475 review
// N1's first named gap: a scalar subquery comparing against a HandoffID
// that does not exist evaluates to NULL, and `> NULL` is never true, so
// NOT-EXISTS alone would wrongly let the admission through. The fix
// requires the referenced row to EXIST (with the exact run and subject)
// as a separate, explicit condition.
func TestAdmitIntegratedHandoffRefusesAMissingReferencedHandoff(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	settled := fixture.drive(supervisor, view.BatchID)
	realHandoffs, err := fixture.store.RunHandoffs(settled.Items[0].RunID)
	if err != nil || len(realHandoffs) != 1 {
		t.Fatalf("item did not admit exactly one handoff: %d (%v)", len(realHandoffs), err)
	}
	synthetic := realHandoffs[0]
	synthetic.ID = "handoff-integrator-missing-ref"
	ghost := orchestration.WorkUnitInput{UnitID: "a", UnitOutput: orchestration.UnitOutput{
		HandoffID: "handoff-never-admitted", RunID: realHandoffs[0].RunID,
		CandidateRevision: "commit-ghost", CandidateTree: "tree-ghost", Outcome: orchestration.OutcomeCompleted,
	}}
	inserted, err := fixture.store.AdmitIntegratedHandoff(synthetic, orchestration.WorkUnitInputs{ghost})
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("admission succeeded against a HandoffID that was never admitted")
	}
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 1 {
		t.Fatalf("run %s has %d handoffs after a refused missing-reference admission, want exactly 1", synthetic.RunID, len(handoffs))
	}
}

// TestAdmitIntegratedHandoffRefusesASameTimestampReplacement is #475 review
// N1's second named gap: a strictly-greater-timestamp comparison alone
// misses a competing handoff admitted at the EXACT same
// admitted_unix_nano (a real possibility with a coarse or collision
// clock). The fix breaks ties by id, matching queryRunHandoffs's own
// ORDER BY admitted_unix_nano ASC, id ASC.
func TestAdmitIntegratedHandoffRefusesASameTimestampReplacement(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	settled := fixture.drive(supervisor, view.BatchID)
	h1, err := fixture.store.RunHandoffs(settled.Items[0].RunID)
	if err != nil || len(h1) != 1 {
		t.Fatalf("item did not admit exactly one handoff: %d (%v)", len(h1), err)
	}
	// A second handoff for the SAME run, same nanosecond, lexicographically
	// GREATER id - a same-timestamp replacement.
	tied := h1[0]
	tied.ID = h1[0].ID + "-tied-later"
	if ok, err := fixture.store.AdmitHandoff(tied); err != nil || !ok {
		t.Fatalf("seeding the same-timestamp replacement: ok=%t err=%v", ok, err)
	}
	synthetic := h1[0]
	synthetic.ID = "handoff-integrator-same-timestamp"
	inputs := orchestration.WorkUnitInputs{inputFor("a", h1[0])}
	inserted, err := fixture.store.AdmitIntegratedHandoff(synthetic, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("admission succeeded against a handoff a same-timestamp row already replaced")
	}
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 2 {
		t.Fatalf("run %s has %d handoffs after a refused same-timestamp admission, want exactly 2 (h1 and the tied replacement)", synthetic.RunID, len(handoffs))
	}
}

// setUpProducerAndIntegratorHandoffs drives a 2-item ordinary #470 batch to
// settlement and returns the producer's own admitted handoff (standing in
// for "a") and a synthetic copy of the second item's handoff, retargeted to
// a fresh ID, that the caller admits against inputs referencing the first.
func setUpProducerAndIntegratorHandoffs(t *testing.T, fixture *fleetFixture, syntheticID string) (h1, synthetic orchestration.EngineeringHandoff) {
	t.Helper()
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(2))
	settled := fixture.drive(supervisor, view.BatchID)
	producerHandoffs, err := fixture.store.RunHandoffs(settled.Items[0].RunID)
	if err != nil || len(producerHandoffs) != 1 {
		t.Fatalf("producer item did not admit exactly one handoff: %d (%v)", len(producerHandoffs), err)
	}
	integratorHandoffs, err := fixture.store.RunHandoffs(settled.Items[1].RunID)
	if err != nil || len(integratorHandoffs) != 1 {
		t.Fatalf("integrator-stand-in item did not admit exactly one handoff: %d (%v)", len(integratorHandoffs), err)
	}
	synthetic = integratorHandoffs[0]
	synthetic.ID = syntheticID
	return producerHandoffs[0], synthetic
}

// TestAdmitIntegratedHandoffRefusesASameOperationRetryNotYetAdmitted is #475
// review #5473634194's correction of the prior N1 attempt: the checks above
// prove H is still the latest ADMITTED row for its run, but admission is not
// the only way a producer's current output can move. The SAME operation can
// journal its NEXT attempt as reported before that attempt is ever admitted
// - from that exact moment inspectHandoff/WorkGraphStatus no longer presents
// H as the run's satisfied output, yet orchestration_handoffs still has no
// newer row. The journalled report is appended through the real AppendEvent
// path (the canonical EngineeringEvent document, with the attempt identity
// at '$.payload.attempt' - never a hand-built payload-shaped document at the
// event's own root, which a prior version of this test wrongly used).
func TestAdmitIntegratedHandoffRefusesASameOperationRetryNotYetAdmitted(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	h1, synthetic := setUpProducerAndIntegratorHandoffs(t, fixture, "handoff-integrator-same-op-retry")
	inputs := orchestration.WorkUnitInputs{inputFor("a", h1)}

	writer, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	interleaved := false
	store := integratedHandoffInterleaveStore{SQLiteOperationStore: fixture.store, beforeInsert: func() {
		interleaved = true
		// A genuinely separate connection journals the SAME operation's
		// NEXT attempt as REPORTED - never admitted - between the
		// integrator's own freshness read and its INSERT.
		payload, err := json.Marshal(HandoffReportedPayload{
			OperationID: h1.Producer.OperationID, Attempt: h1.Producer.Attempt + 1,
			Outcome: orchestration.OutcomeCompleted, ReportSHA256: "sha-attempt-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.AppendEvent(EngineeringEvent{
			SchemaVersion: SchemaVersion, ID: "event-a-attempt-2-reported", RunID: h1.RunID,
			Type: EventHandoffReported, OperationID: h1.Producer.OperationID,
			OccurredAt: time.Now(), Payload: payload,
		}); err != nil {
			t.Fatalf("journalling the same-operation retry: %v", err)
		}
	}}
	inserted, err := store.AdmitIntegratedHandoff(synthetic, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("admission never reached the insert boundary")
	}
	if inserted {
		t.Fatal("the integrated handoff was admitted despite its input's producer having journalled a same-operation retry not yet admitted")
	}
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 1 {
		t.Fatalf("run %s has %d handoffs after a refused synthetic admission, want exactly 1 (%v)", synthetic.RunID, len(handoffs), err)
	}
}

// TestAdmitIntegratedHandoffRefusesWhenLatestEventIsARefusal is #475 review
// #5473634194's second named form of the same defect: inspectHandoff treats
// the run's latest event among {handoff.reported, handoff.refused} as
// authoritative, but the predicate above only ever looked at reported
// events. The journalled refusal here deliberately names H's OWN producer
// identity (operation_id, attempt) - so only the EVENT TYPE, never the
// identity fields, distinguishes this from a legitimate match - because a
// retroactive refusal of the very attempt H was admitted for is the
// narrowest case: WorkGraphStatus no longer presents H as a satisfied
// output purely because the latest outcome recorded for it is a refusal,
// and the predicate must honor that even when the identity still lines up.
func TestAdmitIntegratedHandoffRefusesWhenLatestEventIsARefusal(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	h1, synthetic := setUpProducerAndIntegratorHandoffs(t, fixture, "handoff-integrator-latest-is-refusal")
	inputs := orchestration.WorkUnitInputs{inputFor("a", h1)}

	writer, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	interleaved := false
	store := integratedHandoffInterleaveStore{SQLiteOperationStore: fixture.store, beforeInsert: func() {
		interleaved = true
		payload, err := json.Marshal(HandoffRefusedPayload{
			OperationID: h1.Producer.OperationID, Attempt: h1.Producer.Attempt,
			Kind: HandoffMissing, Detail: "the invocation completed without writing a handoff report",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.AppendEvent(EngineeringEvent{
			SchemaVersion: SchemaVersion, ID: "event-a-same-attempt-refused", RunID: h1.RunID,
			Type: EventHandoffRefused, OperationID: h1.Producer.OperationID,
			OccurredAt: time.Now(), Payload: payload,
		}); err != nil {
			t.Fatalf("journalling the superseding refusal: %v", err)
		}
	}}
	inserted, err := store.AdmitIntegratedHandoff(synthetic, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("admission never reached the insert boundary")
	}
	if inserted {
		t.Fatal("the integrated handoff was admitted despite its input's producer having journalled a later refusal")
	}
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 1 {
		t.Fatalf("run %s has %d handoffs after a refused synthetic admission, want exactly 1 (%v)", synthetic.RunID, len(handoffs), err)
	}
}
