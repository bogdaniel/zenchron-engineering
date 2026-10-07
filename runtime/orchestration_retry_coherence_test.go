package runtime

import (
	"strings"
	"testing"
	"time"
)

func retryLeaseFixture(t *testing.T) (string, string, *SQLiteOperationStore, Scheduler, RunOperation) {
	t.Helper()
	dir, store := openJournal(t)
	runID := "r"
	scheduler := capacityScheduler(store, "owner", 10, 2)
	planned, _, err := scheduler.Plan(RunOperation{
		RunID: runID, Kind: OpExecutionInvoke, IdempotencyKey: "retry-boundary",
		MaxAttempts: 2, WallBudget: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEvent(t, store, runID, EventOperationPlanned, planned)
	leased := mustNext(t, scheduler, runID)
	if leased == nil {
		t.Fatal("initial operation was not leased")
	}
	started, err := scheduler.Start(leased.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEvent(t, store, runID, EventOperationBefore, started)
	failed := started
	failed.State, failed.Lease = OperationFailed, nil
	lifecycleEvent(t, store, runID, EventOperationAfter, failed)
	if _, err := scheduler.Finish(started.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	retry := mustNext(t, scheduler, runID)
	if retry == nil {
		t.Fatal("failed operation was not leased for retry")
	}
	return dir, runID, store, scheduler, *retry
}

func lifecycleSnapshot(t *testing.T, store *SQLiteOperationStore, runID string) (EngineeringRun, RunSnapshot, map[string]RunOperation) {
	t.Helper()
	run, found, err := store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run: found=%v err=%v", found, err)
	}
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[string]RunOperation, len(rows))
	for _, row := range rows {
		operations[row.ID] = row
	}
	return run, snapshot, operations
}

func TestOrchestrationLifecycleAcceptsRetryLeaseBeforeOperationBefore(t *testing.T) {
	_, runID, store, _, retry := retryLeaseFixture(t)
	run, snapshot, operations := lifecycleSnapshot(t, store, runID)
	journal := snapshot.Operations[retry.ID]
	if retry.State != Leased || journal.State != OperationFailed || retry.AttemptIdentity != journal.AttemptIdentity {
		t.Fatalf("fixture missed retry lease boundary: row=%s/%d journal=%s/%d",
			retry.State, retry.AttemptIdentity, journal.State, journal.AttemptIdentity)
	}
	if err := orchestrationLifecycleCoherent(run, snapshot, operations); err != nil {
		t.Fatalf("ordinary retry acquisition reported corrupt: %v", err)
	}
}

func TestCancelledRetryStillRequiresMatchingJournalTerminalFact(t *testing.T) {
	_, runID, store, scheduler, retry := retryLeaseFixture(t)
	if _, err := scheduler.Finish(retry.ID, OperationCancelled); err != nil {
		t.Fatal(err)
	}
	run, snapshot, operations := lifecycleSnapshot(t, store, runID)
	err := orchestrationLifecycleCoherent(run, snapshot, operations)
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("cancelled row with failed journal was accepted: %v", err)
	}
}

func TestStaleRetryCancellationJournalsBeforeFinishingRow(t *testing.T) {
	dir, runID, store, scheduler, retry := retryLeaseFixture(t)
	run, _, err := store.Run(runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	clock := scheduler.Clock
	runtime := &EngineeringRuntime{
		deps:      Dependencies{Store: store, Clock: clock},
		scheduler: scheduler,
	}
	state := &runState{rt: runtime, run: run, events: events}
	if err := runtime.cancelStaleLeasedOperation(state, retry); err != nil {
		t.Fatal(err)
	}

	check := func(read *SQLiteOperationStore) {
		t.Helper()
		current, _, found, err := read.Operation(retry.ID)
		if err != nil || !found {
			t.Fatalf("row: found=%v err=%v", found, err)
		}
		if current.State != OperationCancelled || current.Lease != nil {
			t.Fatalf("scheduler row = %+v, want durable cancellation", current)
		}
		run, snapshot, operations := lifecycleSnapshot(t, read, runID)
		journal := snapshot.Operations[retry.ID]
		if journal.State != OperationCancelled || journal.AttemptIdentity != retry.AttemptIdentity {
			t.Fatalf("journal terminal = %s/%d, want cancelled/%d",
				journal.State, journal.AttemptIdentity, retry.AttemptIdentity)
		}
		if err := orchestrationLifecycleCoherent(run, snapshot, operations); err != nil {
			t.Fatalf("row/journal cancellation mismatch: %v", err)
		}
	}
	check(store)

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}
