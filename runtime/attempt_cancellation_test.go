package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Operator stop reaching a RUNNING execution.invoke (#213). What a stop does to
// the runtime's own later effects - commit, push, pull request - is #215 and is
// deliberately not changed here; TestAStopDoesNotReachOtherOperationKinds pins
// that boundary.

// stopAtWrite commits an operator stop immediately after (or, with before set,
// immediately before) the scheduler writes one operation of one kind in one
// state. It fires once.
type stopAtWrite struct {
	OperationStore
	kind   string
	state  OperationState
	before bool
	stop   func()
	fired  bool
}

func (s *stopAtWrite) PutOperation(op RunOperation, revision int64) (int64, bool, error) {
	armed := !s.fired && op.Kind == s.kind && op.State == s.state
	if armed && s.before {
		s.fired = true
		s.stop()
	}
	next, written, err := s.OperationStore.PutOperation(op, revision)
	if armed && !s.before && written && err == nil {
		s.fired = true
		s.stop()
	}
	return next, written, err
}

func operatorStop(t *testing.T, f *phase8Fixture, runID *string) func() {
	return func() {
		if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), *runID, "operator/stop"); err != nil {
			t.Error(err)
		}
	}
}

// executionAfter returns the journalled terminal record of the run's
// execution.invoke operation, failing if there is none.
func executionAfter(t *testing.T, f *phase8Fixture, runID string) (RunOperation, *ExecutionDiagnostic) {
	t.Helper()
	var last *RunOperation
	for _, e := range f.state(runID).events {
		if e.Type != EventOperationAfter {
			continue
		}
		op, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind == OpExecutionInvoke {
			last = &op
		}
	}
	if last == nil {
		t.Fatal("the execution operation has no terminal operation.after")
	}
	diagnostic, err := executionDiagnosticOf(last.Result)
	if err != nil {
		t.Fatal(err)
	}
	return *last, diagnostic
}

// assertStoppedExecution is the terminal record an interrupted execution must
// leave: operation.after OperationCancelled with a run_cancelled diagnostic in
// the journal, the same diagnostic in status, nothing still active, and no
// execution.completed.
func assertStoppedExecution(t *testing.T, f *phase8Fixture, runID string, outcome Outcome) {
	t.Helper()
	if outcome.Disposition != Cancelled || outcome.Reason != "operator/stop" {
		t.Fatalf("outcome = %+v, want cancelled by operator/stop", outcome)
	}
	op, diagnostic := executionAfter(t, f, runID)
	if op.State != OperationCancelled {
		t.Fatalf("interrupted execution journalled %q, want %q", op.State, OperationCancelled)
	}
	if diagnostic == nil || diagnostic.FailureClass != FailureRunCancelled {
		t.Fatalf("terminal diagnostic = %+v, want %q", diagnostic, FailureRunCancelled)
	}
	status, err := f.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExecutionDiagnostic == nil || status.ExecutionDiagnostic.FailureClass != FailureRunCancelled {
		t.Fatalf("status diagnostic = %+v, want %q", status.ExecutionDiagnostic, FailureRunCancelled)
	}
	for _, stored := range mustOperations(t, f.store, runID) {
		if stored.State == Running || stored.State == Leased {
			t.Fatalf("active operation after stop: %+v", stored)
		}
	}
	if countType(f.state(runID).events, EventExecutionCompleted) != 0 {
		t.Fatal("a stopped execution journalled execution.completed")
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

// A stop committed after Start and before the handler: the watcher's first,
// synchronous observation sees it, and the provider is never invoked.
func TestAStopAfterStartNeverInvokesTheProvider(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	hook := &stopAtWrite{OperationStore: f.runtime.scheduler.Store, kind: OpExecutionInvoke, state: Running, stop: operatorStop(t, f, &runID)}
	f.runtime.scheduler.Store = hook
	var outcome Outcome
	for pass := 0; pass < 8 && !hook.fired; pass++ {
		outcome = f.reconcile(runID)
	}
	if !hook.fired {
		t.Fatal("the execution operation never started")
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("the provider was invoked %d time(s) after the stop", len(f.provider.requests))
	}
	assertStoppedExecution(t, f, runID, outcome)
}

// blockingProvider runs until its context ends and then reports what the
// provider adapter would have reported before #213: a controller shutdown. The
// runtime, not the adapter, is what must turn an observed stop into
// run_cancelled.
type blockingProvider struct {
	*isolatedProvider
	during      func(ctx context.Context)
	interrupted bool
}

func (p *blockingProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	p.during(ctx)
	select {
	case <-ctx.Done():
		p.interrupted = true
		return ExecutionResult{Outcome: OperationCancelled, Failure: &ProviderFailure{Classification: FailureControllerShutdown}}, ctx.Err()
	case <-time.After(10 * time.Second):
		return ExecutionResult{}, errors.New("the provider was never interrupted")
	}
}

// The #213 case itself: the stop lands while the provider is running. The
// watcher interrupts it, the stop has already finished the scheduler row, and
// the pass still ends in a clean cancelled outcome - because the interruption
// was OBSERVED, not because the run happens to be cancelled.
func TestAStopInterruptsARunningExecution(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	stop := operatorStop(t, f, &runID)
	p := &blockingProvider{isolatedProvider: f.provider, during: func(context.Context) { stop() }}
	f.runtime.deps.Provider = p
	var outcome Outcome
	for pass := 0; pass < 8 && len(p.requests) == 0; pass++ {
		outcome = f.reconcile(runID)
	}
	if !p.interrupted {
		t.Fatal("the running provider was not interrupted by the stop")
	}
	assertStoppedExecution(t, f, runID, outcome)
	if outcome := f.reconcile(runID); outcome.Disposition != Cancelled {
		t.Fatalf("a later pass reported %+v", outcome)
	}
}

// A stop that lands AFTER the handler returned interrupted nothing. The Finish
// the stop beat still fails exactly as it does on main, and the provider's own
// failure class stays in the journal: a concurrent stop is not evidence that
// this execution was cancelled, and must not hide what really happened.
func TestAnUnobservedStopDoesNotHideARealFailure(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.provider.mutate = func(string) error { return nil }
	f.runtime.deps.Provider = failingProvider{f.provider}
	hook := &stopAtWrite{OperationStore: f.runtime.scheduler.Store, kind: OpExecutionInvoke, state: OperationFailed, before: true, stop: operatorStop(t, f, &runID)}
	f.runtime.scheduler.Store = hook
	var err error
	for pass := 0; pass < 8 && !hook.fired; pass++ {
		_, err = f.runtime.Reconcile(context.Background(), runID)
	}
	if !hook.fired {
		t.Fatal("the execution operation never finished")
	}
	if err == nil || !strings.Contains(err.Error(), "operation is not active") {
		t.Fatalf("the stale Finish was reported as %v; an unobserved stop must not become a clean cancellation", err)
	}
	op, diagnostic := executionAfter(t, f, runID)
	if op.State != OperationFailed || diagnostic == nil || diagnostic.FailureClass != FailureProviderQuota {
		t.Fatalf("the real failure was rewritten: state %q, diagnostic %+v", op.State, diagnostic)
	}
}

type failingProvider struct{ *isolatedProvider }

func (p failingProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	return ExecutionResult{Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderQuota}}, nil
}

// A controller shutdown cancels the same attempt through the PARENT context.
// It is not a stop: it stays controller_shutdown, journals no cancellation, and
// leaves the run resumable.
func TestAShutdownMidExecutionIsNotAStop(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	p := &blockingProvider{isolatedProvider: f.provider, during: func(context.Context) { shutdown() }}
	f.runtime.deps.Provider = p
	var outcome Outcome
	for pass := 0; pass < 8 && len(p.requests) == 0; pass++ {
		var err error
		if outcome, err = f.runtime.Reconcile(ctx, runID); err != nil {
			t.Fatal(err)
		}
	}
	if !p.interrupted {
		t.Fatal("shutdown did not reach the provider")
	}
	if outcome.Disposition != Waiting || outcome.Reason != "controller_shutdown" {
		t.Fatalf("shutdown settled %+v, want a resumable controller_shutdown wait", outcome)
	}
	_, diagnostic := executionAfter(t, f, runID)
	if diagnostic == nil || diagnostic.FailureClass != FailureControllerShutdown {
		t.Fatalf("shutdown diagnostic = %+v", diagnostic)
	}
	if countType(f.state(runID).events, EventRunCancelled) != 0 {
		t.Fatal("a shutdown journalled a cancellation")
	}
}

// A drain winds the supervisor down and changes no durable intent, so a
// running execution is left to finish.
func TestADrainDoesNotInterruptARunningExecution(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	supervisor := supervisorFixture(t, f, 2)
	interrupted := false
	f.provider.mutate = func(string) error { return nil }
	drained := &drainingProvider{isolatedProvider: f.provider, during: func(ctx context.Context) {
		supervisor.Drain()
		select {
		case <-ctx.Done():
			interrupted = true
		case <-time.After(150 * time.Millisecond):
		}
	}}
	f.runtime.deps.Provider = drained
	for pass := 0; pass < 8 && len(f.provider.requests) == 0; pass++ {
		f.reconcile(runID)
	}
	if len(f.provider.requests) == 0 || !supervisor.Draining() {
		t.Fatal("the drain was not exercised during an execution")
	}
	if interrupted {
		t.Fatal("a drain interrupted a running execution")
	}
	state := f.state(runID)
	if countType(state.events, EventRunCancelled) != 0 || countType(state.events, EventExecutionCompleted) == 0 {
		t.Fatalf("a drained execution did not complete normally: %v", journalTypes(state.events))
	}
}

type drainingProvider struct {
	*isolatedProvider
	during func(ctx context.Context)
}

func (p *drainingProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.during(ctx)
	return p.isolatedProvider.Execute(ctx, request)
}

// The operation-kind boundary. A stop landing as a commit, push or pull
// request operation starts leaves that operation exactly as it behaves on main:
// the effect happens and the stale Finish is reported. Gating these is #215.
func TestAStopDoesNotReachOtherOperationKinds(t *testing.T) {
	for _, kind := range []string{OpCandidateCommit, OpCandidatePush, OpPullRequestCreate} {
		t.Run(kind, func(t *testing.T) {
			f := newPhase8Fixture(t)
			runID := f.start()
			hook := &stopAtWrite{OperationStore: f.runtime.scheduler.Store, kind: kind, state: Running, stop: operatorStop(t, f, &runID)}
			f.runtime.scheduler.Store = hook
			var err error
			for pass := 0; pass < 12 && !hook.fired; pass++ {
				if _, err = f.runtime.Reconcile(context.Background(), runID); err != nil && !hook.fired {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}
			if !hook.fired {
				t.Fatalf("%s never started", kind)
			}
			if err == nil || !strings.Contains(err.Error(), "operation is not active") {
				t.Fatalf("%s after a stop returned %v, want main's stale-Finish error", kind, err)
			}
			events := f.state(runID).events
			switch kind {
			case OpCandidateCommit:
				if countType(events, EventCandidateCommitted) == 0 {
					t.Fatal("the commit handler did not run to completion")
				}
			case OpCandidatePush:
				if _, err := gitOutput(f.origin, "rev-parse", "--verify", "refs/heads/"+candidateBranch(runID)); err != nil {
					t.Fatalf("the push handler did not run to completion: %v", err)
				}
			case OpPullRequestCreate:
				if len(f.forge.PullRequests) != 1 {
					t.Fatalf("the pull request handler did not run to completion: %d", len(f.forge.PullRequests))
				}
			}
			op := lastOperationAfter(t, events, kind)
			if op.State == OperationCancelled {
				t.Fatalf("%s was recorded as cancelled by a stop it never observed", kind)
			}
		})
	}
}

func lastOperationAfter(t *testing.T, events []EngineeringEvent, kind string) RunOperation {
	t.Helper()
	var last *RunOperation
	for _, e := range events {
		if e.Type != EventOperationAfter {
			continue
		}
		op, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind == kind {
			last = &op
		}
	}
	if last == nil {
		t.Fatalf("%s has no operation.after", kind)
	}
	return *last
}

// The watcher cancels only on a SUCCESSFUL read of a Cancelled run. A missing
// run and an unreadable store are not stops; a later read still sees the stop.
func TestTheExecutionWatcherActsOnlyOnAnObservedStop(t *testing.T) {
	t.Run("not found, then stopped from another handle", func(t *testing.T) {
		_, writer, reader := openPair(t)
		r := &EngineeringRuntime{deps: Dependencies{Store: reader}}
		ctx, stop := r.watchExecution(context.Background(), "run-a")
		defer stop()
		select {
		case <-ctx.Done():
			t.Fatal("a run that could not be found was treated as stopped")
		case <-time.After(100 * time.Millisecond):
		}
		run := newJournalRun("run-a")
		run.Disposition = Cancelled
		if err := writer.PutRun(run); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("a durable stop from another handle was not observed")
		}
		if !stop() || !runStopObserved(ctx) {
			t.Fatal("the observed stop is not reported as the cancellation cause")
		}
	})
	t.Run("store read failure", func(t *testing.T) {
		_, writer, reader := openPair(t)
		run := newJournalRun("run-a")
		run.Disposition = Cancelled
		if err := writer.PutRun(run); err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.Run("run-a"); err == nil {
			t.Fatal("the injected store failure did not fail the read")
		}
		r := &EngineeringRuntime{deps: Dependencies{Store: reader}}
		ctx, stop := r.watchExecution(context.Background(), "run-a")
		select {
		case <-ctx.Done():
			t.Fatal("a failed store read interrupted the provider")
		case <-time.After(150 * time.Millisecond):
		}
		if stop() {
			t.Fatal("a failed store read was reported as an observed stop")
		}
	})
	t.Run("parent cancelled", func(t *testing.T) {
		_, writer, reader := openPair(t)
		if err := writer.PutRun(newJournalRun("run-a")); err != nil {
			t.Fatal(err)
		}
		r := &EngineeringRuntime{deps: Dependencies{Store: reader}}
		parent, shutdown := context.WithCancel(context.Background())
		ctx, stop := r.watchExecution(parent, "run-a")
		shutdown()
		<-ctx.Done()
		if stop() || runStopObserved(ctx) {
			t.Fatal("a controller shutdown was reported as an operator stop")
		}
	})
}

// The real CLI adapter, with a real blocked subprocess: a stop is run_cancelled
// with a run_stopped termination cause, never controller_shutdown.
func TestARunStopIsNotRecordedAsAControllerShutdown(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	fake.block = true
	ctx, cancel := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, func() { cancel(errRunStopped) })
	defer timer.Stop()
	started := time.Now()
	result, _ := provider.Execute(ctx, request)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the stopped invocation ran %s", elapsed)
	}
	if result.Outcome != OperationCancelled || result.Failure == nil || result.Failure.Classification != FailureRunCancelled {
		t.Fatalf("a run stop produced %q %#v", result.Outcome, result.Failure)
	}
	if result.Invocation == nil || result.Invocation.TerminationCause != TerminationRunStopped {
		t.Fatalf("termination cause = %#v, want %q", result.Invocation, TerminationRunStopped)
	}
}
