package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestCapacityWaitSupersededByMatchingOperationAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		class      CapacityClass
		kind       string
		superseded bool
	}{
		{"work resumes", CapacityWork, OpExecutionInvoke, true},
		{"observation resumes", CapacityObservation, OpSourceObserve, true},
		{"verification resumes", CapacityVerification, OpAssuranceGo, true},
		{"observation preserves work wait", CapacityWork, OpSourceObserve, false},
		{"work preserves observation wait", CapacityObservation, OpExecutionInvoke, false},
		{"nonverifier work preserves verification wait", CapacityVerification, OpExecutionInvoke, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := openJournal(t)
			run, _, err := store.Run("r")
			if err != nil {
				t.Fatal(err)
			}
			wait := CapacityWait{Class: tc.class, Ceiling: 2}
			lifecycleEvent(t, store, run.ID, EventRunWaiting, dispositionRecord{Reason: wait.reason(), Capacity: &wait})
			run.Disposition, run.Reason = Waiting, wait.reason()
			if err := store.PutRun(run); err != nil {
				t.Fatal(err)
			}
			batch := lifecycleBatch(t, store, run.ID)
			scheduler := capacityScheduler(store, "owner", 10, 2)
			op := planKind(t, scheduler, run.ID, tc.kind)
			lifecycleEvent(t, store, run.ID, EventOperationPlanned, op)
			planned, err := OrchestrationStatus(store, dir, batch, time.Unix(200, 0).UTC())
			if err != nil || planned.Items[0].State != orchestration.ItemQueued || planned.Items[0].Capacity == nil {
				t.Fatalf("planning cleared wait: %+v %v", planned, err)
			}
			if mustNext(t, scheduler, run.ID) == nil {
				t.Fatal("operation not acquired")
			}
			op, err = scheduler.Start(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			lifecycleEvent(t, store, run.ID, EventOperationBefore, op)
			check := func(read *SQLiteOperationStore, owned, fresh bool) {
				t.Helper()
				view, err := OrchestrationStatus(read, dir, batch, time.Unix(200, 0).UTC())
				if err != nil {
					t.Fatal(err)
				}
				item := view.Items[0]
				if owned {
					if item.State != orchestration.ItemRunning || !item.Executing || item.Observation != "transitioning" || item.Capacity != nil {
						t.Fatalf("owned: %+v", item)
					}
				} else if tc.superseded && !fresh {
					if item.State != "" || item.Observation != "transitioning" || item.Capacity != nil || item.Executing {
						t.Fatalf("stale capacity resurrected: %+v", item)
					}
				} else {
					if item.State != orchestration.ItemQueued || item.Observation != "" || item.Capacity == nil || *item.Capacity != wait {
						t.Fatalf("current capacity: %+v", item)
					}
				}
			}
			check(store, true, false)
			ended := op
			ended.State, ended.Lease = Succeeded, nil
			lifecycleEvent(t, store, run.ID, EventOperationAfter, ended)
			if _, err := scheduler.Finish(op.ID, Succeeded); err != nil {
				t.Fatal(err)
			}
			check(store, false, false)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			check(reopened, false, false)
			// A renewed refusal after reconciliation must be current even with the
			// same reason and ceiling as the superseded wait.
			lifecycleEvent(t, reopened, run.ID, EventRunWaiting, dispositionRecord{Reason: wait.reason(), Capacity: &wait})
			check(reopened, false, true)
		})
	}
}

func TestReconcileRenewsSupersededCapacityWait(t *testing.T) {
	for _, tc := range []struct {
		class CapacityClass
		kind  string
	}{
		{CapacityWork, OpExecutionInvoke}, {CapacityObservation, OpSourceObserve},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			fixture := newFleetFixture(t, 1)
			fixture.deps.MaxConcurrentObservations = 1
			supervisor := fixture.supervisor()
			view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
			runID := view.Items[0].RunID
			store := fixture.store
			run, _, err := store.Run(runID)
			if err != nil {
				t.Fatal(err)
			}
			wait := CapacityWait{tc.class, 1}
			lifecycleEvent(t, store, runID, EventRunWaiting, dispositionRecord{Reason: wait.reason(), Capacity: &wait})
			run.Disposition, run.Reason = Waiting, wait.reason()
			if err := store.PutRun(run); err != nil {
				t.Fatal(err)
			}
			scheduler := capacityScheduler(store, "owner-1", 1, 1)
			scheduler.Clock = fixture.clock
			scheduler.LeaseDuration = time.Hour
			op := planKind(t, scheduler, runID, tc.kind)
			lifecycleEvent(t, store, runID, EventOperationPlanned, op)
			if mustNext(t, scheduler, runID) == nil {
				t.Fatal("operation not acquired")
			}
			op, err = scheduler.Start(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			lifecycleEvent(t, store, runID, EventOperationBefore, op)
			op.State, op.Lease = Succeeded, nil
			lifecycleEvent(t, store, runID, EventOperationAfter, op)
			if _, err := scheduler.Finish(op.ID, Succeeded); err != nil {
				t.Fatal(err)
			}
			if err := store.PutRun(newJournalRun("holder")); err != nil {
				t.Fatal(err)
			}
			planKind(t, scheduler, "holder", tc.kind)
			if mustNext(t, scheduler, "holder") == nil {
				t.Fatal("holder not acquired")
			}
			engine, err := supervisor.engine("acme/repo", "claude")
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := engine.Reconcile(context.Background(), runID)
			if err != nil || outcome.Reason != wait.reason() {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			item := fixture.status(view.BatchID).Items[0]
			if item.State != orchestration.ItemQueued || item.Observation != "" || item.Capacity == nil || *item.Capacity != wait {
				t.Fatalf("renewed refusal not current: %+v", item)
			}
		})
	}
}
