package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #489 proofs: a continuation that completes a runtime-owned checkpoint
// without a newer mutation binds its handoff to exactly that checkpoint, and
// to nothing else.

// checkpointedRun orchestrates one issue whose worker checkpoints and then
// continues as behaviour says.
func checkpointedRun(t *testing.T, behaviour fleetBehaviour) (*fleetFixture, OrchestrationView, string) {
	t.Helper()
	fixture := newFleetFixture(t, 10)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, behaviour)
	return fixture, view, runID
}

// executeWithoutAdmission drives the run through its checkpoint and its
// continuation by the engine alone. Admission lives in the supervisor's batch
// step, so nothing here admits or refuses the handoff.
func executeWithoutAdmission(t *testing.T, fixture *fleetFixture, runID string) *EngineeringRuntime {
	t.Helper()
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("the engine admitted %d handoffs (%v); admission is the supervisor's", len(handoffs), err)
	}
	return engine
}

// theCheckpoint is the run's one runtime-owned checkpoint, as journalled.
func theCheckpoint(t *testing.T, fixture *fleetFixture, runID string) CandidateCommittedPayload {
	t.Helper()
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	var found []CandidateCommittedPayload
	for _, event := range events {
		if event.Type == EventCandidateCheckpointed {
			payload, err := decodePayload[CandidateCommittedPayload](event.Payload)
			if err != nil {
				t.Fatal(err)
			}
			found = append(found, payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d checkpoints journalled, want exactly one: %v", len(found), journalTypes(events))
	}
	return found[0]
}

func onlyHandoff(t *testing.T, fixture *fleetFixture, runID string) orchestration.EngineeringHandoff {
	t.Helper()
	handoffs, err := fixture.store.RunHandoffs(runID)
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("%d admitted handoffs (%v), want exactly one", len(handoffs), err)
	}
	return handoffs[0]
}

func admitOnce(t *testing.T, fixture *fleetFixture, batchID string) {
	t.Helper()
	batch, found, err := fixture.store.OrchestrationBatch(batchID)
	if err != nil || !found {
		t.Fatalf("batch %s: found=%t err=%v", batchID, found, err)
	}
	if err := admitOrchestratedHandoff(fixture.store, fixture.stateDir, batch, batch.Items[0], fixture.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestAZeroDeltaContinuationBindsTheCheckpointItCompleted is the #178
// dogfood shape: the continuation changed nothing and completed checkpoint C,
// so its handoff binds C's runtime-observed commit, tree and changed paths.
// Before #489 it was refused as unbindable and the item never completed.
func TestAZeroDeltaContinuationBindsTheCheckpointItCompleted(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetCheckpointThenComplete)
	settled := fixture.drive(fixture.supervisor(), view.BatchID)
	item := itemFor(t, settled, runID)
	if item.State != orchestration.ItemCompleted {
		t.Fatalf("state %s handoff %s reason %q, want completed", item.State, item.Handoff, item.Reason)
	}
	checkpoint := theCheckpoint(t, fixture, runID)
	handoff := onlyHandoff(t, fixture, runID)
	if handoff.Subject.CandidateRevision != checkpoint.Commit || handoff.Subject.CandidateTree != checkpoint.Tree ||
		handoff.Observed != (orchestration.HandoffObserved{ChangedPathCount: checkpoint.PathCount, ChangedPathsDigest: checkpoint.PathsDigest}) {
		t.Fatalf("handoff bound %+v / %+v, want checkpoint %+v", handoff.Subject, handoff.Observed, checkpoint)
	}
	// The producer is the continuation, not the invocation that checkpointed.
	state := fixture.state(runID)
	producer := state.snapshot.Operations[handoff.Producer.OperationID]
	if bindingOf(producer) != invocationContinuationPrefix+checkpoint.Commit {
		t.Fatalf("handoff producer %s is bound %q, want the continuation of %s", producer.ID, bindingOf(producer), checkpoint.Commit)
	}
	if countType(state.events, EventCandidateCommitted) != 0 {
		t.Fatalf("a zero-delta continuation produced a commit: %v", journalTypes(state.events))
	}
}

// TestAContinuationThatMutatesBindsItsOwnNewerCommit: a continuation that
// changed the candidate further binds the commit of that change, never C.
func TestAContinuationThatMutatesBindsItsOwnNewerCommit(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetCheckpointThenMutate)
	settled := fixture.drive(fixture.supervisor(), view.BatchID)
	if item := itemFor(t, settled, runID); item.State != orchestration.ItemCompleted {
		t.Fatalf("state %s reason %q, want completed", item.State, item.Reason)
	}
	checkpoint := theCheckpoint(t, fixture, runID)
	handoff := onlyHandoff(t, fixture, runID)
	state := fixture.state(runID)
	commit, ok := succeededOperation(state.snapshot.Operations, OpCandidateCommit, operationKey(OpCandidateCommit, handoff.Producer.OperationID))
	if !ok {
		t.Fatal("the mutating continuation's output was never committed")
	}
	var committed CandidateCommittedPayload
	for _, event := range state.events {
		if event.OperationID == commit.ID && event.Type == EventCandidateCommitted {
			committed, _ = decodePayload[CandidateCommittedPayload](event.Payload)
		}
	}
	if handoff.Subject.CandidateRevision == checkpoint.Commit || handoff.Subject.CandidateRevision != committed.Commit {
		t.Fatalf("handoff bound %s; checkpoint %s, continuation's own commit %s", handoff.Subject.CandidateRevision, checkpoint.Commit, committed.Commit)
	}
}

// TestAHandoffRestatingTheCheckpointSHAIsNotAdmitted: provider-written text
// that names the very commit the runtime would bind is still not a handoff.
// The identity is the runtime's to establish, not the report's to state.
func TestAHandoffRestatingTheCheckpointSHAIsNotAdmitted(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetCheckpointThenRestatesSHA)
	settled := fixture.drive(fixture.supervisor(), view.BatchID)
	item := itemFor(t, settled, runID)
	if item.State != orchestration.ItemHandoffPending || item.Handoff != orchestration.HandoffRefused {
		t.Fatalf("state %s handoff %s, want handoff_pending/refused", item.State, item.Handoff)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted (%v)", len(handoffs), err)
	}
}

// TestASupersededCheckpointIsRefused: once anything moved the candidate head
// past C, the continuation's handoff no longer describes the run's candidate
// and is durably refused - even though a current candidate exists. This is the
// mutation test for the checkpoint guard: "bind to the current candidate"
// would admit it.
func TestASupersededCheckpointIsRefused(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetCheckpointThenComplete)
	engine := executeWithoutAdmission(t, fixture, runID)
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	// A later runtime commit, journalled before admission ran.
	if err := engine.append(state, EventCandidateCommitted, "op-later-commit", CandidateCommittedPayload{
		Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), PathCount: 1, PathsDigest: "later",
	}, nil); err != nil {
		t.Fatal(err)
	}
	admitOnce(t, fixture, view.BatchID)
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("a superseded checkpoint's handoff was admitted: %d (%v)", len(handoffs), err)
	}
	item := itemFor(t, fixture.status(view.BatchID), runID)
	if item.Handoff != orchestration.HandoffRefused || !strings.Contains(item.Reason, "superseded") {
		t.Fatalf("handoff %s reason %q, want a durable superseded refusal", item.Handoff, item.Reason)
	}
}

// TestCheckpointHandoffAdmissionIsIdempotentAcrossRestart: a restart between
// the continuation completing and its admission admits the handoff once,
// bound to C, and re-running admission after a further restart neither adds a
// second handoff nor rebinds the first.
func TestCheckpointHandoffAdmissionIsIdempotentAcrossRestart(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetCheckpointThenComplete)
	executeWithoutAdmission(t, fixture, runID)
	reopen := func() {
		if err := fixture.store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err := OpenSQLiteOperationStore(fixture.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		fixture.store = store
	}
	reopen()
	if item := itemFor(t, fixture.drive(fixture.supervisor(), view.BatchID), runID); item.State != orchestration.ItemCompleted {
		t.Fatalf("state %s reason %q after restart, want completed", item.State, item.Reason)
	}
	first := onlyHandoff(t, fixture, runID)
	if first.Subject.CandidateRevision != theCheckpoint(t, fixture, runID).Commit {
		t.Fatalf("handoff bound %s, not the checkpoint", first.Subject.CandidateRevision)
	}
	reopen()
	admitOnce(t, fixture, view.BatchID)
	if again := onlyHandoff(t, fixture, runID); !reflect.DeepEqual(again, first) {
		t.Fatalf("re-admission changed the handoff\n got %+v\nwant %+v", again, first)
	}
}

// TestAHandoffWithNoCommitAndNoCheckpointStaysUnbindable: an invocation that
// changed nothing and continued no checkpoint still has nothing to bind to.
func TestAHandoffWithNoCommitAndNoCheckpointStaysUnbindable(t *testing.T) {
	fixture, view, runID := checkpointedRun(t, fleetNoChangeHandoff)
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if reported := handoffEvents(t, fixture.store, runID); len(reported) == 0 || reported[len(reported)-1].Type != EventHandoffReported {
		t.Fatal("the no-change invocation never reported its handoff, so the test proves nothing")
	}
	admitOnce(t, fixture, view.BatchID)
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted (%v)", len(handoffs), err)
	}
	item := itemFor(t, fixture.status(view.BatchID), runID)
	if item.Handoff != orchestration.HandoffRefused || !strings.Contains(item.Reason, "unbindable") {
		t.Fatalf("handoff %s reason %q, want unbindable", item.Handoff, item.Reason)
	}
}
