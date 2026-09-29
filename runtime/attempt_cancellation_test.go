package runtime

import (
	"context"
	"testing"
	"time"
)

type stopAfterStart struct {
	OperationStore
	stop  func()
	fired bool
}

func (s *stopAfterStart) PutOperation(op RunOperation, revision int64) (int64, bool, error) {
	next, written, err := s.OperationStore.PutOperation(op, revision)
	if written && err == nil && op.Kind == OpExecutionInvoke && op.State == Running && !s.fired {
		s.fired = true
		s.stop()
	}
	return next, written, err
}

func TestStopAfterStartSkipsHandler(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	hook := &stopAfterStart{OperationStore: f.store, stop: func() {
		if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
			t.Fatal(err)
		}
	}}
	f.runtime.scheduler.Store = hook
	outcome := f.reconcile(runID)
	if !hook.fired || len(f.provider.requests) != 0 || outcome.Disposition != Cancelled || outcome.Reason != "operator/stop" {
		t.Fatalf("fired=%v calls=%d outcome=%+v", hook.fired, len(f.provider.requests), outcome)
	}
}

type cancellationProvider struct {
	*isolatedProvider
	stop     func()
	observed bool
}

func (p *cancellationProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.stop()
	select {
	case <-ctx.Done():
		p.observed = true
		return ExecutionResult{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return ExecutionResult{}, context.DeadlineExceeded
	}
}

func TestStopInterruptsExecutingProvider(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	p := &cancellationProvider{isolatedProvider: f.provider, stop: func() {
		if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
			t.Fatal(err)
		}
	}}
	f.runtime.deps.Provider = p
	outcome := f.reconcile(runID)
	if !p.observed || outcome.Disposition != Cancelled || outcome.Reason != "operator/stop" {
		t.Fatalf("observed=%v outcome=%+v", p.observed, outcome)
	}
	for _, op := range mustOperations(t, f.store, runID) {
		if op.State == Running || op.State == Leased {
			t.Fatalf("active operation after stop: %+v", op)
		}
	}
}

func mustOperations(t *testing.T, store *SQLiteOperationStore, runID string) []RunOperation {
	t.Helper()
	ops, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestAttemptMonitorReadsStopFromAnotherHandle(t *testing.T) {
	_, writer, reader := openPair(t)
	run := newJournalRun("run-a")
	if err := writer.PutRun(run); err != nil {
		t.Fatal(err)
	}
	r := &EngineeringRuntime{deps: Dependencies{Store: reader}}
	ctx, stop := r.watchAttempt(context.Background(), run.ID)
	defer stop()
	run.Disposition = Cancelled
	if err := writer.PutRun(run); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("durable cancellation was not observed")
	}
}
