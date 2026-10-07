package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #492 review bounds: a repair is a provider invocation and spends the
// run's MaxProviderInvocations, an operator stop owns it exactly as it owns
// an execution, and a controller lost between the scheduler starting it and
// its operation.before being journalled loses no repair and grants no second.

// repairEngine is the engine the fixture's supervisor would drive the run with.
func repairEngine(t *testing.T, fixture *fleetFixture) *EngineeringRuntime {
	t.Helper()
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// reconcileUntil drives the run by the engine alone until done reports true.
func reconcileUntil(t *testing.T, engine *EngineeringRuntime, fixture *fleetFixture, runID string, done func() bool) {
	t.Helper()
	for range 8 {
		if done() {
			return
		}
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	if !done() {
		t.Fatal("the run never reached the state under test")
	}
}

// spentInvocations is the run total MaxProviderInvocations bounds, read as
// the runtime reads it.
func spentInvocations(t *testing.T, engine *EngineeringRuntime, runID string) int {
	t.Helper()
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	return state.providerInvocationsSpent()
}

// TestARepairSpendsTheRunsProviderInvocations: a repair that reaches a
// provider is one provider invocation, counted once by the run total, by the
// remaining-budget view and by the plan-level accounting. (An eligible repair
// the run has no invocation left for is refused before any provider: see
// TestTheRepairDecisionIsAFunctionOfTheJournal.)
func TestARepairSpendsTheRunsProviderInvocations(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	fixture.deps.Budgets.MaxProviderInvocations = 3
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, fleetCompletedWithUnresolved)
	fixture.worker.setRepair(runID, repairWritesPartial)
	if item := itemFor(t, fixture.drive(supervisor, view.BatchID), runID); item.State != orchestration.ItemPartial {
		t.Fatalf("item %s (%s)", item.State, item.Reason)
	}
	engine := repairEngine(t, fixture)
	if spent := spentInvocations(t, engine, runID); spent != 2 {
		t.Fatalf("the run total reads %d provider invocations, want the engineering one and the repair", spent)
	}
	if planned, err := (PlanReconciler{Store: fixture.store}).providerInvocations(runID); err != nil || planned != 2 {
		t.Fatalf("plan accounting reads %d provider invocations (%v), want 2", planned, err)
	}
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if left := engine.remainingBudgets(state).ProviderInvocations; left != KnownBudget(1) {
		t.Fatalf("remaining provider invocations %+v, want 1", left)
	}
}

// stopRepairRun orchestrates one #159-shaped issue and returns the engine that
// drives it and an operator stop bound to that engine's scheduler.
func stopRepairRun(t *testing.T, repair fleetRepair) (*fleetFixture, *EngineeringRuntime, OrchestrationView, string, func()) {
	t.Helper()
	fixture := newFleetFixture(t, 10)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, fleetCompletedWithUnresolved)
	fixture.worker.setRepair(runID, repair)
	engine := repairEngine(t, fixture)
	stop := func() {
		if _, err := CancelRun(fixture.store, engine.scheduler, fixture.clock.Now(), runID, "operator/stop"); err != nil {
			t.Error(err)
		}
	}
	return fixture, engine, view, runID, stop
}

// assertNoPostStopAnswer: nothing the repair wrote after the stop became a
// handoff observation or an admitted handoff, and the repair is recorded
// cancelled.
func assertNoPostStopAnswer(t *testing.T, fixture *fleetFixture, view OrchestrationView, runID string) RunOperation {
	t.Helper()
	admitOnce(t, fixture, view.BatchID)
	for _, event := range handoffEvents(t, fixture.store, runID) {
		if event.Type == EventHandoffReported {
			t.Fatal("a repair answer was journalled after the stop")
		}
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted after the stop (%v)", len(handoffs), err)
	}
	repairs := repairOperations(t, fixture, runID)
	if len(repairs) != 1 || repairs[0].State != OperationCancelled {
		t.Fatalf("repair operations %+v, want one cancelled", repairs)
	}
	if engineering, _ := fixture.worker.counts(runID); engineering != 1 {
		t.Fatalf("%d engineering invocations", engineering)
	}
	return repairs[0]
}

// TestAStopBeforeTheRepairDispatchesInvokesNothing: the stop lands the moment
// the scheduler starts the repair. The watcher's synchronous first read sees
// it, no provider is started, and no invocation is spent.
func TestAStopBeforeTheRepairDispatchesInvokesNothing(t *testing.T) {
	fixture, engine, view, runID, stop := stopRepairRun(t, repairWritesPartial)
	hook := &stopAtWrite{OperationStore: engine.scheduler.Store, kind: OpHandoffRepair, state: Running, stop: stop}
	engine.scheduler.Store = hook
	reconcileUntil(t, engine, fixture, runID, func() bool { return hook.fired })
	if _, repairs := fixture.worker.counts(runID); repairs != 0 {
		t.Fatalf("the repair provider was invoked %d time(s) after the stop", repairs)
	}
	repair := assertNoPostStopAnswer(t, fixture, view, runID)
	if repairReachedProvider(repair) || spentInvocations(t, engine, runID) != 1 {
		t.Fatalf("a repair stopped before dispatch was counted as a provider invocation: %s", repair.Result)
	}
}

// TestAStopDuringTheRepairInterruptsIt: the stop lands while the repair's
// provider runs. The watcher ends it, the provider attributes its ending to
// the stop, the operation is interrupted, and the document it wrote after the
// stop is never journalled or admitted. The invocation it reached is spent.
func TestAStopDuringTheRepairInterruptsIt(t *testing.T) {
	fixture, engine, view, runID, stop := stopRepairRun(t, repairStoppedMidway)
	fixture.worker.onRepair = func(string) { stop() }
	reconcileUntil(t, engine, fixture, runID, func() bool { _, repairs := fixture.worker.counts(runID); return repairs > 0 })
	repair := assertNoPostStopAnswer(t, fixture, view, runID)
	if !repairReachedProvider(repair) || spentInvocations(t, engine, runID) != 2 {
		t.Fatalf("an interrupted repair that reached its provider was not counted once: %s", repair.Result)
	}
	if outcome, err := engine.Reconcile(context.Background(), runID); err != nil || outcome.Disposition != Cancelled {
		t.Fatalf("a later pass reported %+v (%v)", outcome, err)
	}
	if _, repairs := fixture.worker.counts(runID); repairs != 1 {
		t.Fatalf("%d repair invocations", repairs)
	}
}

// TestAStopThatLostTheRaceKeepsTheProvidersEnding: the stop reaches the
// provider, but the provider exits successfully by itself. Its own ending is
// kept - the operation is not recorded as stopped - and the authority the
// stop revoked still refuses the document it wrote.
func TestAStopThatLostTheRaceKeepsTheProvidersEnding(t *testing.T) {
	fixture, engine, view, runID, stop := stopRepairRun(t, repairFinishesDespiteStop)
	fixture.worker.onRepair = func(string) { stop() }
	// The stop finished the scheduler row while the provider ran, so the
	// repair's own Finish is refused exactly as main refuses an execution's
	// (TestAStopAfterAProviderSucceededDoesNotRewriteIt); the journal keeps
	// the provider's ending.
	var err error
	for pass := 0; pass < 8; pass++ {
		if _, repairs := fixture.worker.counts(runID); repairs > 0 {
			break
		}
		_, err = engine.Reconcile(context.Background(), runID)
		fixture.clock.advance(61 * time.Second)
	}
	if err == nil || !strings.Contains(err.Error(), "operation is not active") {
		t.Fatalf("the stale Finish was reported as %v, want main's error", err)
	}
	admitOnce(t, fixture, view.BatchID)
	repairs := repairOperations(t, fixture, runID)
	if len(repairs) != 1 || repairs[0].State != Succeeded {
		t.Fatalf("repair operations %+v, want one that kept its own successful ending", repairs)
	}
	item := itemFor(t, fixture.status(view.BatchID), runID)
	if item.HandoffRepair != HandoffRepairFailed || !strings.Contains(item.HandoffRepairDetail, "revoked (run_cancelled)") ||
		!strings.Contains(item.HandoffRepairDetail, `"success"`) {
		t.Fatalf("repair %q detail %q", item.HandoffRepair, item.HandoffRepairDetail)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted from a revoked repair (%v)", len(handoffs), err)
	}
}

// crashAtWrite loses the controller immediately after the scheduler durably
// writes one operation of one kind in one state: the write lands, and
// nothing after it in that pass does.
type crashAtWrite struct {
	OperationStore
	kind  string
	state OperationState
	fired bool
}

func (s *crashAtWrite) PutOperation(op RunOperation, revision int64) (int64, bool, error) {
	next, written, err := s.OperationStore.PutOperation(op, revision)
	if !s.fired && written && err == nil && op.Kind == s.kind && op.State == s.state {
		s.fired = true
		crashRepair()
	}
	return next, written, err
}

// TestAControllerLostBetweenStartAndBeforeStillRepairsOnce is the crash the
// second scheduler attempt of handoff.repair exists for: StartWithin is
// durable, operation.before is not, and no provider was reached. A successor
// controller performs exactly one repair - not zero, which is what a ceiling
// of one attempt would leave (the run settles attempts_exhausted), and not
// two - and the engineering is never run again.
func TestAControllerLostBetweenStartAndBeforeStillRepairsOnce(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, fleetCompletedWithUnresolved)
	fixture.worker.setRepair(runID, repairWritesPartial)
	engine := repairEngine(t, fixture)
	hook := &crashAtWrite{OperationStore: engine.scheduler.Store, kind: OpHandoffRepair, state: Running}
	engine.scheduler.Store = hook
	for pass := 0; pass < 8 && !hook.fired; pass++ {
		lost := make(chan struct{})
		go func() {
			defer close(lost)
			_, _ = engine.Reconcile(context.Background(), runID)
		}()
		<-lost
		fixture.clock.advance(61 * time.Second)
	}
	if !hook.fired {
		t.Fatal("the repair operation was never started")
	}
	// The durable intermediate state the regression is about.
	if starts := operationBefores(t, fixture, runID, OpHandoffRepair); starts != 0 {
		t.Fatalf("operation.before was journalled %d time(s) before the loss", starts)
	}
	rows, err := fixture.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	started := false
	for _, row := range rows {
		started = started || (row.Kind == OpHandoffRepair && row.Attempt == 1 && row.State == Running)
	}
	if !started {
		t.Fatalf("the scheduler row of the repair is not durably started: %+v", rows)
	}
	if _, repairs := fixture.worker.counts(runID); repairs != 0 {
		t.Fatal("the lost controller reached the repair provider")
	}

	successor := fixture.supervisor()
	item := itemFor(t, fixture.drive(successor, view.BatchID), runID)
	if item.State != orchestration.ItemPartial || item.HandoffRepair != HandoffRepairRepaired {
		t.Fatalf("after the successor: item %s repair %q (%s)", item.State, item.HandoffRepair, item.Reason)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations, want exactly one of each", engineering, repairs)
	}
	if starts := operationBefores(t, fixture, runID, OpHandoffRepair); starts != 1 {
		t.Fatalf("the repair was started %d times by the successor", starts)
	}
	if spent := spentInvocations(t, repairEngine(t, fixture), runID); spent != 2 {
		t.Fatalf("the run total reads %d provider invocations, want the engineering one and one repair", spent)
	}
}
