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

// The wrapper intercepts only the durable I/O boundary, after the real admission
// code has validated C and the report. Its insert still uses the real SQLite
// store; the competing append uses a separately opened connection.
type interleavedCheckpointHandoffStore struct {
	*SQLiteOperationStore
	beforeInsert func(orchestration.EngineeringHandoff, int64)
}

func (s interleavedCheckpointHandoffStore) AdmitCheckpointHandoff(handoff orchestration.EngineeringHandoff, sequence int64) (bool, error) {
	s.beforeInsert(handoff, sequence)
	return s.SQLiteOperationStore.AdmitCheckpointHandoff(handoff, sequence)
}

func TestCheckpointHandoffLosesToCandidateMovementBeforeInsert(t *testing.T) {
	for _, eventType := range []string{EventCandidateCommitted, EventCandidateCheckpointed, EventCandidateBaseIntegrated} {
		t.Run(eventType, func(t *testing.T) {
			fixture, view, runID := checkpointedRun(t, fleetCheckpointThenComplete)
			executeWithoutAdmission(t, fixture, runID)
			checkpoint := theCheckpoint(t, fixture, runID)
			writer, err := OpenSQLiteOperationStore(fixture.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { writer.Close() })
			batch, found, err := fixture.store.OrchestrationBatch(view.BatchID)
			if err != nil || !found {
				t.Fatalf("batch found=%t: %v", found, err)
			}
			interleaved := false
			store := interleavedCheckpointHandoffStore{SQLiteOperationStore: fixture.store,
				beforeInsert: func(handoff orchestration.EngineeringHandoff, sequence int64) {
					interleaved = true
					if handoff.Subject.CandidateRevision != checkpoint.Commit || handoff.Subject.CandidateTree != checkpoint.Tree {
						t.Fatalf("admission did not validate checkpoint C: %+v", handoff.Subject)
					}
					events, err := writer.Events(runID)
					if err != nil || len(events) == 0 || sequence != events[len(events)-1].Sequence {
						t.Fatalf("admission did not anchor the current journal: sequence=%d err=%v", sequence, err)
					}
					if _, refusal, err := checkpointCommitOf(events, handoff.Producer.OperationID, checkpoint.Commit); err != nil || refusal != "" {
						t.Fatalf("C was not current before the interleaving: refusal=%q err=%v", refusal, err)
					}
					var payload any = CandidateCommittedPayload{
						Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), PathCount: 1, PathsDigest: "later",
					}
					if eventType == EventCandidateBaseIntegrated {
						payload = CandidateBaseIntegratedPayload{Strategy: "rebase", BaseRevision: fixture.base,
							Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}
					}
					raw, err := marshalPayloadJSON(payload)
					if err != nil {
						t.Fatal(err)
					}
					appended, err := writer.AppendEvent(EngineeringEvent{
						SchemaVersion: SchemaVersion, ID: newEventID(runID), RunID: runID,
						Type: eventType, OperationID: "op-concurrent-candidate-movement",
						OccurredAt: fixture.clock.Now(), Payload: raw,
					})
					if err != nil || appended.Sequence <= sequence {
						t.Fatalf("competing append did not commit after the validated snapshot: %+v (%v)", appended, err)
					}
				}}
			if err := admitOrchestratedHandoff(store, fixture.stateDir, batch, batch.Items[0], fixture.clock.Now()); err != nil {
				t.Fatal(err)
			}
			if !interleaved {
				t.Fatal("admission never reached the checkpoint insert boundary")
			}
			if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
				t.Fatalf("checkpoint C was admitted after a concurrent candidate movement: %d handoffs (%v)", len(handoffs), err)
			}
			// Crash/reopen before another pass can record the refusal. The
			// journal alone must preserve the lost admission across replay.
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			fixture.store = reopened
			if _, err := reopened.Replay(runID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				admitOnce(t, fixture, view.BatchID)
				if handoffs, err := reopened.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
					t.Fatalf("replay admitted stale checkpoint C: %d handoffs (%v)", len(handoffs), err)
				}
				item := itemFor(t, fixture.status(view.BatchID), runID)
				if item.Handoff != orchestration.HandoffRefused || !strings.Contains(item.Reason, "superseded") {
					t.Fatalf("handoff %s reason %q, want durable superseded refusal after restart", item.Handoff, item.Reason)
				}
			}
		})
	}
}

func TestCheckpointHandoffAllowsNonMovingEventsBeforeInsert(t *testing.T) {
	for _, eventType := range []string{EventCandidateQuarantined, EventCandidateExternalChanged} {
		t.Run(eventType, func(t *testing.T) {
			fixture, view, runID := checkpointedRun(t, fleetCheckpointThenComplete)
			executeWithoutAdmission(t, fixture, runID)
			checkpoint := theCheckpoint(t, fixture, runID)
			batch, found, err := fixture.store.OrchestrationBatch(view.BatchID)
			if err != nil || !found {
				t.Fatalf("batch found=%t: %v", found, err)
			}
			interleaved := false
			store := interleavedCheckpointHandoffStore{SQLiteOperationStore: fixture.store,
				beforeInsert: func(handoff orchestration.EngineeringHandoff, sequence int64) {
					interleaved = true
					// A missing or invented high-water mark cannot authorize insertion.
					for _, invalid := range []int64{0, -1, sequence + 1} {
						inserted, err := fixture.store.AdmitCheckpointHandoff(handoff, invalid)
						if inserted || (invalid <= 0 && err == nil) || (invalid > 0 && err != nil) {
							t.Fatalf("invalid cursor %d: inserted=%t err=%v", invalid, inserted, err)
						}
					}
					var payload any = CandidateExternalChangedPayload{
						ExpectedRevision: checkpoint.Commit, ObservedRevision: strings.Repeat("a", 40),
					}
					if eventType == EventCandidateQuarantined {
						payload = CandidateQuarantinedPayload{OperationID: "op-quarantine", Attempt: 1,
							Subject: checkpoint.Commit, PathsDigest: "quarantined", Location: "quarantine"}
					}
					raw, err := marshalPayloadJSON(payload)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := fixture.store.AppendEvent(EngineeringEvent{
						SchemaVersion: SchemaVersion, ID: newEventID(runID), RunID: runID,
						Type: eventType, OccurredAt: fixture.clock.Now(), Payload: raw,
					}); err != nil {
						t.Fatal(err)
					}
				}}
			if err := admitOrchestratedHandoff(store, fixture.stateDir, batch, batch.Items[0], fixture.clock.Now()); err != nil {
				t.Fatal(err)
			}
			if !interleaved {
				t.Fatal("admission never reached the checkpoint insert boundary")
			}
			if handoff := onlyHandoff(t, fixture, runID); handoff.Subject.CandidateRevision != checkpoint.Commit {
				t.Fatalf("handoff bound %s, want checkpoint %s", handoff.Subject.CandidateRevision, checkpoint.Commit)
			}
		})
	}
}
