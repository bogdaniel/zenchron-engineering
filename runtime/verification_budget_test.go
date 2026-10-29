package runtime

import (
	"fmt"
	"testing"
	"time"
)

func recordVerificationOperation(t *testing.T, store *SQLiteOperationStore, op RunOperation, kind string, at time.Time) {
	t.Helper()
	raw, err := CanonicalJSON(op)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: op.ID + "/" + kind, RunID: op.RunID, OperationID: op.ID, Type: kind, OccurredAt: at, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNestedVerificationWaitAccountingSurvivesRestartAndRecovery(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			dir, store := openJournal(t)
			origin := time.Unix(100, 0).UTC()
			clock := &fakeClock{now: origin}
			s := verificationScheduler(store, "controller", 10, 1)
			s.Clock = clock
			holder := nestedParent(t, store, s, "holder")
			p, err := s.RequestVerification(ExecutionAttemptRef{RunID: holder.RunID, OperationID: holder.ID, Attempt: holder.AttemptIdentity}, "holder-tool", "holder-owner")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.AcquireVerification(p); !ok || err != nil {
				t.Fatal("holder not granted")
			}
			if err := store.PutRun(newJournalRun("budget")); err != nil {
				t.Fatal(err)
			}
			if external {
				raw, _ := CanonicalJSON(dispositionRecord{Reason: ReasonGoalStateReached})
				_, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "review-wait", RunID: "budget", Type: EventRunWaiting, OccurredAt: origin.Add(5 * time.Second), Payload: raw})
				if err != nil {
					t.Fatal(err)
				}
			}
			clock.now = origin.Add(10 * time.Second)
			parent := nestedParent(t, store, s, "budget")
			recordVerificationOperation(t, store, parent, EventOperationBefore, clock.now)
			clock.now = origin.Add(20 * time.Second)
			q, err := s.RequestVerification(ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, "waiting-tool", "waiting-owner")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.AcquireVerification(q); ok || err != nil {
				t.Fatal("capacity wait did not remain inside parent")
			}
			clock.now = origin.Add(80 * time.Second)
			reopened, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s.Store = reopened
			assertActive := func(want time.Duration) {
				t.Helper()
				events, err := reopened.Events("budget")
				if err != nil {
					t.Fatal(err)
				}
				run := newJournalRun("budget")
				if got := ActiveElapsed(run, events, clock.now); got != want {
					t.Fatalf("active=%s want=%s at=%s", got, want, clock.now.Sub(origin))
				}
				state := runState{run: run, events: events}
				if got := state.activeElapsed(clock.now); got != want {
					t.Fatalf("run/plan budget disagrees: %s/%s", got, want)
				}
			}
			want := 20 * time.Second
			if external {
				want = 15 * time.Second
			}
			assertActive(want)
			dead := s
			dead.Liveness = neverAlive()
			if err := dead.ReleaseVerification(p); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.AcquireVerification(q); !ok || err != nil {
				t.Fatal("recovery did not admit tool")
			}
			clock.now = origin.Add(100 * time.Second)
			assertActive(want + 20*time.Second)
			clock.now = origin.Add(160 * time.Second)
			if err := dead.ReleaseVerification(q); err != nil {
				t.Fatal(err)
			}
			finished, err := s.Finish(parent.ID, Succeeded)
			if err != nil {
				t.Fatal(err)
			}
			if finished.ConsumedExecution != 90*time.Second {
				t.Fatalf("operation charged capacity wait: %s", finished.ConsumedExecution)
			}
			if finished.Attempt != parent.Attempt || finished.AttemptIdentity != parent.AttemptIdentity {
				t.Fatal("wait consumed another attempt")
			}
			recordVerificationOperation(t, reopened, finished, EventOperationAfter, clock.now)
			clock.now = origin.Add(260 * time.Second)
			if external {
				assertActive(95 * time.Second)
			} else {
				assertActive(200 * time.Second)
			}
		})
	}
}

func TestNestedVerificationOrphanedToolWorkIsChargedDuringExternalWait(t *testing.T) {
	_, store := openJournal(t)
	origin := time.Unix(100, 0).UTC()
	clock := &fakeClock{now: origin}
	s := verificationScheduler(store, "controller", 10, 1)
	s.Clock = clock
	parent := nestedParent(t, store, s, "producer")
	recordVerificationOperation(t, store, parent, EventOperationBefore, origin)
	p, err := s.RequestVerification(ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, "tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(p); !ok || err != nil {
		t.Fatal("no tool grant")
	}
	clock.now = origin.Add(10 * time.Second)
	finished, err := s.Finish(parent.ID, OperationFailed)
	if err != nil {
		t.Fatal(err)
	}
	recordVerificationOperation(t, store, finished, EventOperationAfter, clock.now)
	raw, err := CanonicalJSON(dispositionRecord{Reason: waitReason(FailureControllerShutdown)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "shutdown-wait", RunID: parent.RunID, Type: EventRunWaiting, OccurredAt: clock.now, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	clock.now = origin.Add(100 * time.Second)
	events, err := store.Events(parent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ActiveElapsed(newJournalRun(parent.RunID), events, clock.now); got != 100*time.Second {
		t.Fatalf("live tool work was excluded after parent shutdown: %s", got)
	}
	dead := s
	dead.Liveness = neverAlive()
	if err := dead.ReleaseVerification(p); err != nil {
		t.Fatal(err)
	}
	clock.now = origin.Add(200 * time.Second)
	events, err = store.Events(parent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ActiveElapsed(newJournalRun(parent.RunID), events, clock.now); got != 100*time.Second {
		t.Fatalf("actual external wait did not begin after tool cleanup: %s", got)
	}
}
