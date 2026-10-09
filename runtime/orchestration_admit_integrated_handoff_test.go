package runtime

// Split out of workgraph_integration_test.go (#475 review N1): these test
// AdmitIntegratedHandoff's atomic SQLite admission predicate directly -
// the superseded-input race and the two named gaps a later review found in
// it - a distinct responsibility from the end-to-end WorkGraph acceptance
// scenarios the rest of that file covers.

import (
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
