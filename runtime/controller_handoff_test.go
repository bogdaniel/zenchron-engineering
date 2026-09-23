package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeRunReader drives journals a fixture cannot produce on demand: a run that
// moved between the preflight and the transfer, a run that appeared after it,
// and a journal that cannot be read at all.
type fakeRunReader struct {
	runs   []EngineeringRun
	events map[string][]EngineeringEvent
	fail   map[string]error
}

func (f fakeRunReader) Runs() ([]EngineeringRun, error) { return f.runs, nil }

func (f fakeRunReader) Events(runID string) ([]EngineeringEvent, error) {
	if err, failing := f.fail[runID]; failing {
		return nil, err
	}
	return f.events[runID], nil
}

func handoffParties(t *testing.T) (HandoffParty, HandoffParty) {
	t.Helper()
	config := ConfigDigest{Global: "config-a"}
	return HandoffParty{Binding: adoptedBinding(predecessorRevision, "tree-a", config), ArtifactPath: "/bin/main-a"},
		HandoffParty{Binding: adoptedBinding(successorRevision, "tree-b", config), ArtifactPath: "/bin/main-b"}
}

func handoffDigest(t *testing.T, party HandoffParty) string {
	t.Helper()
	digest, err := party.Binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// liveRun is a nonterminal run bound to the predecessor.
func liveRun(t *testing.T, id string, disposition Disposition, predecessor HandoffParty) EngineeringRun {
	t.Helper()
	return EngineeringRun{ID: id, Disposition: disposition, ControllerSHA256: handoffDigest(t, predecessor)}
}

func preflightInput(predecessor, successor HandoffParty) HandoffPreflightInput {
	return HandoffPreflightInput{
		Predecessor: predecessor, Successor: successor,
		TrustedMain: RevisionRecord{Revision: successorRevision, Tree: "tree-b"},
		IsAncestor:  ancestorAlways,
		Now:         time.Unix(1700000000, 0).UTC(),
	}
}

// THE GATE IS ALL OR NOTHING. One run the successor cannot continue blocks the
// whole transition, because the alternative is a stranded run, two schedulers,
// or a successor taking work it just proved it cannot interpret.
func TestPreflightGatesTheWholeHandoff(t *testing.T) {
	predecessor, successor := handoffParties(t)
	stranger := HandoffParty{Binding: adoptedBinding(strangerRevision, "tree-x", ConfigDigest{Global: "config-a"})}

	for _, test := range []struct {
		name    string
		reader  fakeRunReader
		want    HandoffPhase
		blocker string
	}{
		{"no live work at all", fakeRunReader{}, HandoffPrepared, ""},
		{"every live run compatible", fakeRunReader{runs: []EngineeringRun{
			liveRun(t, "run-a", Active, predecessor), liveRun(t, "run-b", Waiting, predecessor),
		}}, HandoffPrepared, ""},
		// A WAITING run is the one that looks idle and is not: the successor
		// may have to resume it, so it is classified like any other.
		{"one waiting run incompatible", fakeRunReader{runs: []EngineeringRun{
			liveRun(t, "run-a", Active, predecessor),
			{ID: "run-b", Disposition: Waiting, ControllerSHA256: handoffDigest(t, stranger)},
		}}, HandoffFailed, "run-b source_lineage"},
		{"an unreadable journal blocks exactly as a refusal does", fakeRunReader{
			runs: []EngineeringRun{liveRun(t, "run-a", Active, predecessor)},
			fail: map[string]error{"run-a": fmt.Errorf("disk is gone")},
		}, HandoffFailed, "run-a durable_replay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := PreflightControllerHandoff(test.reader, preflightInput(predecessor, successor))
			if err != nil {
				t.Fatal(err)
			}
			if record.Phase != test.want {
				t.Fatalf("phase = %q, want %q (blockers %v)", record.Phase, test.want, record.Blockers())
			}
			if test.blocker == "" {
				if len(record.Blockers()) != 0 {
					t.Fatalf("a prepared handoff carries blockers: %v", record.Blockers())
				}
				return
			}
			var named bool
			for _, blocker := range record.Blockers() {
				if strings.HasPrefix(blocker, test.blocker) {
					named = true
				}
			}
			if !named {
				t.Fatalf("blockers %v do not name %q", record.Blockers(), test.blocker)
			}
		})
	}
}

// A finished run needs no successor, so it is not a reason to block an upgrade.
func TestPreflightIgnoresTerminalRuns(t *testing.T) {
	predecessor, successor := handoffParties(t)
	stranger := HandoffParty{Binding: adoptedBinding(strangerRevision, "tree-x", ConfigDigest{Global: "config-a"})}
	reader := fakeRunReader{runs: []EngineeringRun{
		{ID: "run-done", Disposition: Completed, ControllerSHA256: handoffDigest(t, stranger)},
		{ID: "run-failed", Disposition: Failed, ControllerSHA256: handoffDigest(t, stranger)},
		{ID: "run-cancelled", Disposition: Cancelled, ControllerSHA256: handoffDigest(t, stranger)},
		liveRun(t, "run-live", Active, predecessor),
	}}
	record, err := PreflightControllerHandoff(reader, preflightInput(predecessor, successor))
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != HandoffPrepared {
		t.Fatalf("phase = %q, want prepared (blockers %v)", record.Phase, record.Blockers())
	}
	if len(record.Runs) != 1 || record.Runs[0].RunID != "run-live" {
		t.Fatalf("classified %d run(s), want only the live one: %+v", len(record.Runs), record.Runs)
	}
}

func TestHandoffAdvancesOnlyAlongItsPath(t *testing.T) {
	predecessor, successor := handoffParties(t)
	now := time.Unix(1700000000, 0).UTC()
	record, err := PreflightControllerHandoff(fakeRunReader{}, preflightInput(predecessor, successor))
	if err != nil {
		t.Fatal(err)
	}

	// Skipping a phase is refused: a record that claimed a phase nothing
	// walked to would answer the recovery question with a fiction.
	for _, skip := range []HandoffPhase{HandoffActivated, HandoffRevalidated, HandoffSuccessorAcquired} {
		if _, err := record.Advance(skip, now); err == nil {
			t.Fatalf("a prepared handoff jumped to %q", skip)
		}
	}

	wantOwner := map[HandoffPhase]string{
		HandoffDraining:          handoffDigest(t, predecessor),
		HandoffOwnershipReleased: handoffDigest(t, predecessor),
		HandoffSuccessorAcquired: handoffDigest(t, predecessor),
		// The admissions are written here, and a journal carrying events the
		// predecessor may not understand is not one it can be handed back.
		HandoffRevalidated: handoffDigest(t, successor),
		HandoffActivated:   handoffDigest(t, successor),
	}
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased, HandoffSuccessorAcquired, HandoffRevalidated, HandoffActivated} {
		record, err = record.Advance(phase, now)
		if err != nil {
			t.Fatalf("advance to %q: %v", phase, err)
		}
		if record.RecoveryOwner != wantOwner[phase] {
			t.Fatalf("at %q recovery owner is %s, want %s",
				phase, shortSHA(record.RecoveryOwner), shortSHA(wantOwner[phase]))
		}
	}
	// A settled record is settled.
	if _, err := record.Advance(HandoffFailed, now); err == nil {
		t.Fatal("an activated handoff moved to failed")
	}
	if !record.MayRecover(handoffDigest(t, successor)) || record.MayRecover(handoffDigest(t, predecessor)) {
		t.Fatal("an activated handoff must permit only the successor to recover")
	}
	if record.InFlight() {
		t.Fatal("an activated handoff is still in flight")
	}
}

func TestRevalidationRefusesStateThatMoved(t *testing.T) {
	predecessor, successor := handoffParties(t)
	run := liveRun(t, "run-a", Active, predecessor)
	reader := fakeRunReader{runs: []EngineeringRun{run}}
	record, err := PreflightControllerHandoff(reader, preflightInput(predecessor, successor))
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != HandoffPrepared {
		t.Fatalf("preflight refused: %v", record.Blockers())
	}
	if err := RevalidateControllerHandoff(reader, record); err != nil {
		t.Fatalf("unchanged state failed revalidation: %v", err)
	}

	for _, test := range []struct {
		name   string
		reader fakeRunReader
		want   string
	}{
		{"a run whose journal grew while draining", fakeRunReader{
			runs:   []EngineeringRun{run},
			events: map[string][]EngineeringEvent{"run-a": {{Type: EventRunWaiting}}},
		}, "moved between the preflight and the transfer"},
		{"a run that became live after the preflight", fakeRunReader{
			runs: []EngineeringRun{run, liveRun(t, "run-new", Active, predecessor)},
		}, "never classified"},
		{"a journal that can no longer be read", fakeRunReader{
			runs: []EngineeringRun{run},
			fail: map[string]error{"run-a": fmt.Errorf("disk is gone")},
		}, "could not be re-read"},
		// The reverse is fine: a run that FINISHED while draining needs
		// nobody to continue it.
		{"a run that finished while draining", fakeRunReader{
			runs: []EngineeringRun{{ID: "run-a", Disposition: Completed, ControllerSHA256: run.ControllerSHA256}},
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := RevalidateControllerHandoff(test.reader, record)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

// Two processes are alive during a handoff and both can write. The phase is a
// compare-and-set so the last writer cannot decide who owns the scheduler.
func TestHandoffWritesAreConditionalOnThePhaseTheCallerRead(t *testing.T) {
	fixture := newPhase8Fixture(t)
	predecessor, successor := handoffParties(t)
	now := time.Unix(1700000000, 0).UTC()
	record, err := PreflightControllerHandoff(fixture.store, preflightInput(predecessor, successor))
	if err != nil {
		t.Fatal(err)
	}

	inserted, err := fixture.store.PutControllerHandoff(record, "")
	if err != nil || !inserted {
		t.Fatalf("insert: %v inserted=%v", err, inserted)
	}
	if again, err := fixture.store.PutControllerHandoff(record, ""); err != nil || again {
		t.Fatalf("a second insert of the same transition wrote: %v inserted=%v", err, again)
	}

	draining, err := record.Advance(HandoffDraining, now)
	if err != nil {
		t.Fatal(err)
	}
	// A writer that read a phase the record has moved past writes nothing.
	if wrote, err := fixture.store.PutControllerHandoff(draining, HandoffSuccessorAcquired); err != nil || wrote {
		t.Fatalf("a stale expectation wrote: %v wrote=%v", err, wrote)
	}
	if wrote, err := fixture.store.PutControllerHandoff(draining, HandoffPrepared); err != nil || !wrote {
		t.Fatalf("the correct expectation did not write: %v wrote=%v", err, wrote)
	}
	stored, ok, err := fixture.store.ControllerHandoff(record.ID)
	if err != nil || !ok {
		t.Fatalf("read back: %v ok=%v", err, ok)
	}
	if stored.Phase != HandoffDraining {
		t.Fatalf("stored phase = %q, want %q", stored.Phase, HandoffDraining)
	}
}

// THE CRASH BOUNDARY THIS SLICE EXISTS FOR: the successor holds the scheduler,
// fails revalidation, and must leave behind a state in which it admitted
// nothing, scheduled nothing, and the predecessor is the only controller
// permitted to resume.
func TestSuccessorThatFailsRevalidationAdmitsNothingAndHandsBack(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.ControllerBuild = attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	predecessorRuntime, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = predecessorRuntime
	runID := fixture.start()

	predecessorBuild := deps.ControllerBuild
	successorBuild := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	predecessor := HandoffParty{
		Binding:      ControllerBinding{Controller: deps.ControllerID, Build: &predecessorBuild, Config: deps.ConfigDigest},
		ArtifactPath: "/bin/main-a",
	}
	successor := HandoffParty{
		Binding:      ControllerBinding{Controller: deps.ControllerID, Build: &successorBuild, Config: deps.ConfigDigest},
		ArtifactPath: "/bin/main-b",
	}

	record, err := PreflightControllerHandoff(fixture.store, HandoffPreflightInput{
		Predecessor: predecessor, Successor: successor,
		TrustedMain: RevisionRecord{Revision: successorRevision, Tree: "tree-b"},
		IsAncestor:  ancestorAlways, Now: fixture.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != HandoffPrepared {
		t.Fatalf("preflight refused a compatible successor: %v", record.Blockers())
	}
	eventsBefore, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}

	// The preflight persisted nothing: the predecessor is still the active
	// controller and its state must not record a transition that never began.
	if handoffs, err := fixture.store.ControllerHandoffs(); err != nil || len(handoffs) != 0 {
		t.Fatalf("the preflight wrote %d record(s): %v", len(handoffs), err)
	}

	// Walk to the boundary: the successor holds the scheduler.
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased, HandoffSuccessorAcquired} {
		record, err = record.Advance(phase, fixture.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	if record.RecoveryOwner != handoffDigest(t, predecessor) {
		t.Fatal("before revalidation the predecessor must be the recovery owner")
	}

	// The state moved underneath the decision, so revalidation refuses.
	moved := fakeRunReader{
		runs:   []EngineeringRun{{ID: runID, Disposition: Active, ControllerSHA256: handoffDigest(t, predecessor)}},
		events: map[string][]EngineeringEvent{runID: append(eventsBefore, EngineeringEvent{Type: EventRunWaiting})},
	}
	revalidationErr := RevalidateControllerHandoff(moved, record)
	if revalidationErr == nil {
		t.Fatal("revalidation passed on state that moved")
	}

	failed, err := record.Fail(revalidationErr.Error(), predecessor, fixture.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if failed.Phase != HandoffFailed {
		t.Fatalf("phase = %q, want failed", failed.Phase)
	}
	if !failed.MayRecover(handoffDigest(t, predecessor)) {
		t.Fatal("the predecessor is not permitted to resume the state it never left")
	}
	if failed.MayRecover(handoffDigest(t, successor)) {
		t.Fatal("a successor that could not revalidate may not recover")
	}
	if failed.InFlight() {
		t.Fatal("a failed handoff is still in flight")
	}

	// NOTHING WAS ADMITTED. The journal must be byte-for-byte what the
	// predecessor left, or the run would now believe a controller that never
	// activated may append to it.
	eventsAfter, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("the failed handoff appended %d event(s)", len(eventsAfter)-len(eventsBefore))
	}
	successorDigest, err := successor.Binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := fixture.store.Run(runID)
	if err != nil || !ok {
		t.Fatalf("run %s: %v", runID, err)
	}
	if ControllerSuccessionContinues(run, eventsAfter, successorDigest) {
		t.Fatal("a successor that failed revalidation may continue the run")
	}
	// And the predecessor still can, which is what makes recovery possible.
	if !ControllerSuccessionContinues(run, eventsAfter, predecessorRuntime.controller) {
		t.Fatal("the predecessor can no longer continue its own run")
	}
}

// ACTIVATION IS THE POINT OF NO SILENT RETURN. Before it the predecessor is the
// recovery controller; at it the successor becomes one, and the predecessor's
// artifact staying on disk is provenance rather than permission.
func TestActivationEndsThePredecessorsEligibility(t *testing.T) {
	predecessor, successor := handoffParties(t)
	now := time.Unix(1700000000, 0).UTC()
	record, err := PreflightControllerHandoff(fakeRunReader{}, preflightInput(predecessor, successor))
	if err != nil {
		t.Fatal(err)
	}

	// Every phase up to revalidation hands back to the predecessor, and a
	// failure there is a return of state to the process that wrote it.
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased, HandoffSuccessorAcquired} {
		record, err = record.Advance(phase, now)
		if err != nil {
			t.Fatal(err)
		}
		if !record.MayRecover(handoffDigest(t, predecessor)) {
			t.Fatalf("at %q the predecessor is not the recovery controller", phase)
		}
	}

	for _, phase := range []HandoffPhase{HandoffRevalidated, HandoffActivated} {
		record, err = record.Advance(phase, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if record.MayRecover(handoffDigest(t, predecessor)) {
		t.Fatal("an activated handoff still permits the predecessor to resume scheduling")
	}
	if !record.MayRecover(handoffDigest(t, successor)) {
		t.Fatal("an activated handoff does not permit its own successor")
	}

	// The predecessor's artifact is still named - that is what makes it
	// addressable for provenance - and naming it grants nothing.
	if record.Predecessor.ArtifactPath == "" {
		t.Fatal("the record stopped naming the predecessor's artifact")
	}

	// There is no edge out of activated, so no later failure can be recorded
	// as a return to the predecessor. A successor failure after this point is
	// a NEW recovery event against the durable head as it stands then, and
	// #234 deliberately builds no machinery for it.
	if _, err := record.Fail("the successor died", predecessor, now); err == nil {
		t.Fatal("an activated handoff was rolled back to its predecessor")
	}
}
