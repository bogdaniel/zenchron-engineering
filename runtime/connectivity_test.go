package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestConnectivityClassification(t *testing.T) {
	for _, err := range []error{&net.DNSError{IsNotFound: true}, fmt.Errorf("request: %w", syscall.ENETUNREACH), syscall.ECONNRESET} {
		if !failed(err).transient {
			t.Fatalf("not transient: %v", err)
		}
	}
	for _, err := range []error{errors.New("unknown"), errors.New("ENOTFOUND in test output"), syscall.EACCES} {
		if failed(err).transient {
			t.Fatalf("guessed transient: %v", err)
		}
	}
	if got := classifyAgentFailure(claudeSpec, "api error: can't reach the api server — check your internet or dns (enotfound)"); got != FailureProviderUnavailable {
		t.Fatalf("incident classified %s", got)
	}
}

func TestConnectivityBackoffSurvivesSerialization(t *testing.T) {
	s, clock := deadlineScheduler(t)
	op := plannedExecution(t, s, time.Hour)
	clock.at = clock.at.Add(time.Second)
	deadline := clock.at.Add(connectivityBackoff(op.Attempt))
	finished, err := s.finishAt(op.ID, OperationFailed, deadline)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	var restored RunOperation
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s.Store = NewMemoryOperationStore()
	if _, _, err := s.Store.PutOperation(restored, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		next, err := s.Next(op.RunID)
		if err != nil || next != nil {
			t.Fatalf("early retry: %+v %v", next, err)
		}
	}
	stored, _, _, _ := s.Store.Operation(op.ID)
	if stored.Attempt != op.Attempt || stored.ActiveSince != nil || stored.ConsumedExecution != time.Second {
		t.Fatalf("budget changed: %+v", stored)
	}
	clock.at = deadline
	next, err := s.Next(op.RunID)
	if err != nil || next == nil {
		t.Fatalf("eligible retry unavailable: %v", err)
	}
	if connectivityBackoff(1000) != 5*time.Minute {
		t.Fatal("backoff unbounded")
	}
}

func TestConnectivityDeadlinePersistsAcrossSQLiteRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRun(newJournalRun("run-1")); err != nil {
		t.Fatal(err)
	}
	s, clock := deadlineScheduler(t)
	s.Store = store
	op := plannedExecution(t, s, time.Hour)
	deadline := clock.at.Add(time.Minute)
	if _, err := s.finishAt(op.ID, OperationFailed, deadline); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	if next, err := s.Next(op.RunID); err != nil || next != nil {
		t.Fatalf("restart bypassed wait: %+v %v", next, err)
	}
	clock.at = deadline
	if next, err := s.Next(op.RunID); err != nil || next == nil {
		t.Fatalf("retry unavailable after recovery: %+v %v", next, err)
	}
	if !externalWaitReasons["connectivity_backoff"] {
		t.Fatal("connectivity wait charged as work")
	}
}

func TestConnectivitySourceObservationKeepsFiniteAttempts(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return fmt.Errorf("request: %w", syscall.ENETUNREACH) })
	first := f.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "connectivity_backoff" {
		t.Fatalf("outage: %+v", first)
	}
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var failedOp RunOperation
	for _, op := range ops {
		if op.Kind == OpSourceObserve {
			failedOp = op
		}
	}
	if failedOp.Attempt != 1 || failedOp.RetryNotBefore.IsZero() {
		t.Fatalf("missing bounded wait: %+v", failedOp)
	}
	f.runtime = f.newRuntime(f.deps)
	for i := 0; i < 3; i++ {
		if outcome := f.reconcile(runID); outcome.Disposition != Waiting {
			t.Fatalf("restart: %+v", outcome)
		}
	}
	stored, _, _, _ := f.store.Operation(failedOp.ID)
	if stored.Attempt != 1 || !stored.RetryNotBefore.Equal(failedOp.RetryNotBefore) {
		t.Fatalf("restart spent authority: %+v", stored)
	}
	for attempt := 2; attempt <= failedOp.MaxAttempts; attempt++ {
		f.clock.at = stored.RetryNotBefore
		f.reconcile(runID)
		stored, _, _, _ = f.store.Operation(failedOp.ID)
		if stored.Attempt != attempt {
			t.Fatalf("attempt %d: %+v", attempt, stored)
		}
	}
	if outcome := f.reconcile(runID); outcome.Disposition != Failed {
		t.Fatalf("exhausted authority did not stop: %+v", outcome)
	}
}

func TestConnectivitySourceObservationRecoversSameOperation(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return syscall.ENETUNREACH })
	f.reconcile(runID)
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var waiting RunOperation
	for _, op := range ops {
		if op.Kind == OpSourceObserve {
			waiting = op
		}
	}
	f.inject(func(GitHubCall) error { return nil })
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	recovered, _, _, err := f.store.Operation(waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != Succeeded || recovered.Attempt != 2 || !recovered.RetryNotBefore.IsZero() {
		t.Fatalf("recovery changed operation or budget: %+v", recovered)
	}
}

func TestConnectivityJournalStoreGapIsExternalWait(t *testing.T) {
	s, clock := deadlineScheduler(t)
	op := plannedExecution(t, s, time.Hour)
	clock.at = clock.at.Add(time.Second)
	observed := clock.at
	retryAt := observed.Add(connectivityBackoff(op.Attempt))
	after := op
	after.State, after.RetryNotBefore = OperationFailed, retryAt
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	_, openSince, work := foldExternalWait([]EngineeringEvent{{Type: EventOperationAfter, OperationID: op.ID, OccurredAt: observed, Payload: raw}})
	if !openSince.Equal(observed) || work != 0 {
		t.Fatalf("journal lost external wait: %s %s", openSince, work)
	}
	clock.at = clock.at.Add(time.Hour)
	settled, err := s.finishAt(op.ID, OperationFailed, retryAt)
	if err != nil {
		t.Fatal(err)
	}
	if settled.ConsumedExecution != time.Second || !settled.RetryNotBefore.Equal(retryAt) {
		t.Fatalf("store recovery charged downtime: %+v", settled)
	}
}
