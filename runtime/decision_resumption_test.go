package runtime

// #508 P4b: the bounded, same-RunID decision continuation.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// decisionResumeFixture builds a replayed runState directly from durable
// operations and resolved decisions, the same technique
// execution_continuation_budget_test.go's continuationFixture already uses:
// what is under test is how the runtime READS its own durable state, never
// a provider.
func decisionResumeFixture(resolved []orchestration.DecisionResolution, operations map[string]RunOperation, candidateRevision string) *runState {
	return &runState{
		rt:       &EngineeringRuntime{deps: Dependencies{Budgets: RunBudgets{MaxExecutionAttempts: 3}, Clock: newSteppingClock()}},
		run:      EngineeringRun{ID: "run-decision-resume", Contract: Ref{ID: "c", Revision: "1"}},
		snapshot: RunSnapshot{Operations: operations},
		projection: RunProjection{
			Contract: Ref{ID: "c", Revision: "1"}, CandidateRevision: candidateRevision,
		},
		resolvedOwnDecisions: resolved,
	}
}

// TestUnresumedDecisionResumeBindingReselectsBeforeDerivingNew is #508
// review P4b §4's reselection proof: an already-started, FAILED decision-
// resumed operation is reselected - its exact binding, unchanged - even
// though the CURRENT undelivered set would otherwise digest to a DIFFERENT
// binding. The same invariant unresolvedFeedbackBinding already proves for
// reviewer feedback: preserving the exact binding is what keeps its
// ORIGINAL attempt ceiling in force, rather than minting a fresh one.
func TestUnresumedDecisionResumeBindingReselectsBeforeDerivingNew(t *testing.T) {
	existingBinding := "decision-resumed|deadbeef|cafebabe"
	state := decisionResumeFixture(
		[]orchestration.DecisionResolution{{RequestID: "decision-1", ID: "resolution-1"}},
		map[string]RunOperation{
			"op-decision-resumed": {
				ID: "op-decision-resumed", Kind: OpExecutionInvoke,
				IdempotencyKey: operationKey(OpExecutionInvoke, existingBinding),
				State:          OperationFailed,
			},
		}, "head-sha")
	binding, ok := state.unresumedDecisionResumeBinding()
	if !ok || binding != existingBinding {
		t.Fatalf("expected the EXISTING failed binding to be reselected, got ok=%t binding=%q, want %q", ok, binding, existingBinding)
	}
}

// TestUnresumedDecisionResumeBindingDerivesFreshWhenNoneStarted is the
// complementary case: with no existing decision-resumed operation, a fresh
// binding is derived from the current undelivered set.
func TestUnresumedDecisionResumeBindingDerivesFreshWhenNoneStarted(t *testing.T) {
	resolved := []orchestration.DecisionResolution{{RequestID: "decision-1", ID: "resolution-1"}}
	state := decisionResumeFixture(resolved, map[string]RunOperation{}, "head-sha")
	binding, ok := state.unresumedDecisionResumeBinding()
	want := decisionResumeBinding(state.contractRevision(), "head-sha", resolved)
	if !ok || binding != want {
		t.Fatalf("expected a freshly derived binding %q, got ok=%t binding=%q", want, ok, binding)
	}
}

// TestUnresumedDecisionResumeBindingEmptyWhenAllDelivered confirms the
// fallback proposes nothing once every own decision has already been
// delivered by SOME attempt - never a phantom resumption for a resolution
// nothing still owes a context to.
func TestUnresumedDecisionResumeBindingEmptyWhenAllDelivered(t *testing.T) {
	state := decisionResumeFixture(nil, map[string]RunOperation{}, "head-sha")
	if binding, ok := state.unresumedDecisionResumeBinding(); ok {
		t.Fatalf("expected no binding when there is nothing undelivered, got %q", binding)
	}
}

// TestASupersededDecisionNeverResumesAfterItsResolutionWasAlreadyWritten is
// #508 review P4b §6's revalidation proof at the unit level: a decision-
// resumed operation's own binding is checked against a FRESH read, not the
// state loaded when the binding was first derived - a request superseded
// after its resolution was written must refuse the stale operation rather
// than dispatch it with a context that no longer matches its own claim.
func TestASupersededDecisionNeverResumesAfterItsResolutionWasAlreadyWritten(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	authority := testHoldAuthority("operator-1")
	if _, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", authority, now); err != nil {
		t.Fatal(err)
	}

	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := state.unresumedDecisionResumeBinding()
	if !ok {
		t.Fatal("expected a decision-resumed binding before the supersession")
	}
	operation := RunOperation{ID: "op-under-test", RunID: runID, Kind: OpExecutionInvoke, IdempotencyKey: operationKey(OpExecutionInvoke, binding)}

	// The request is superseded AFTER its resolution already exists - the
	// resolution itself stays a true historical fact, but the question it
	// answered no longer stands.
	if _, err := admitTestMessage(store, batch, runID, "op-2",
		orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "pick one, revised", Supersedes: d1.ID}, now); err != nil {
		t.Fatal(err)
	}

	if refusal := engine.decisionResumeStillValid(state, operation); refusal == nil {
		t.Fatal("expected the stale, superseded decision-resumed operation to be refused, got nil (proceed)")
	} else if class := refusal.result.(executionRecord).FailureClass; class != FailureDecisionBindingStale {
		t.Fatalf("expected FailureDecisionBindingStale, got %v", class)
	}
}

// decisionResumedOperations returns every OpExecutionInvoke operation of
// this run whose binding is decision-resumed.
func decisionResumedOperations(t *testing.T, fixture *fleetFixture, runID string) []RunOperation {
	t.Helper()
	ops, err := fixture.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var found []RunOperation
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke && strings.HasPrefix(bindingOf(op), decisionResumptionPrefix) {
			found = append(found, op)
		}
	}
	return found
}

// TestADecisionDeliveredByAnOrdinaryContinuationIsNeverResumedTwice is #508
// review P4b's central no-duplicate-invocation proof: a worker checkpoints
// and asks in the same attempt; the decision resolves BEFORE the ordinary
// #489 continuation runs; that continuation's own compiled context already
// carries the resolution (communicationContext is scope-wide, given to
// every invocation alike) and delivers it by finishing normally. No
// decision-resumed operation is ever created merely because no operation
// happens to be named that - delivery is tracked by the fact, not by which
// binding produced it.
func TestADecisionDeliveredByAnOrdinaryContinuationIsNeverResumedTwice(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	worker := &messagingWorker{fleetProvider: fixture.worker, documents: map[string]string{}, seeded: map[string]bool{}}
	fixture.deps.Provider = worker
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, fleetCheckpointThenComplete)
	worker.say(runID, decisionWaitDocument(t), false)

	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Pass 1 (one external Reconcile call): the checkpointing invoke, then
	// its own candidate.commit. CandidateComplete is now false; the
	// continuation is wanted next, but the decision has not been ADMITTED
	// yet (a separate batch step), so nothing blocks it from being proposed
	// - it simply has not run yet.
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found || run.Orchestration == nil {
		t.Fatalf("run %s has no orchestration batch: found=%t err=%v", runID, found, err)
	}
	batch, found, err := fixture.store.OrchestrationBatch(run.Orchestration.BatchID)
	if err != nil || !found {
		t.Fatalf("batch unreadable: found=%t err=%v", found, err)
	}
	if err := admitOrchestratedMessages(fixture.store, fixture.stateDir, batch, fixture.clock.Now()); err != nil {
		t.Fatal(err)
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected exactly one admitted, open decision: open=%v err=%v", open, err)
	}
	// The gate must now hold the continuation back.
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if run := decisionWaitRunRow(t, fixture, runID); run.Reason != ReasonDecisionPending {
		t.Fatalf("expected the continuation to be gated on the open decision, got reason=%s", run.Reason)
	}

	if _, err := fixture.supervisor().ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: "approach-b",
		Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}
	worker.say(runID, "", false) // the continuation asks nothing further

	// The gate lifts; the ordinary #489 continuation now runs, completing
	// the checkpoint, and its own compiled context already carries the
	// resolution.
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}

	if resumed := decisionResumedOperations(t, fixture, runID); len(resumed) != 0 {
		t.Fatalf("a decision-resumed operation was created even though the ordinary continuation already delivered the resolution: %+v", resumed)
	}
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for _, event := range events {
		if event.Type == EventDecisionsDelivered {
			deliveries++
		}
	}
	if deliveries != 1 {
		t.Fatalf("expected exactly one decision-delivery record (from the continuation), got %d", deliveries)
	}
}

func decisionWaitRunRow(t *testing.T, fixture *fleetFixture, runID string) EngineeringRun {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run %s is unreadable: found=%t err=%v", runID, found, err)
	}
	return run
}

// TestTwoDecisionsResolvedBeforeAnyResumptionBecomeOneOperation is #508
// review P4b §4's coalescing proof: two of a run's own decisions resolve
// before any resumption ever runs; exactly one decision-resumed operation is
// created, its own binding covering BOTH, never two separate operations or
// two separate invocations.
func TestTwoDecisionsResolvedBeforeAnyResumptionBecomeOneOperation(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Drive the run through its WHOLE ordinary lifecycle FIRST - the
	// fixture's default worker behaviour mutates and completes in one
	// invocation, and nothing here stops the run from running the entire
	// pipeline to goal_state_reached in one external Reconcile call. Only
	// AFTER that is settled do the two decisions get asked and resolved, so
	// neither one could possibly have been delivered by an invocation that
	// ran before either existed.
	for range 10 {
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		if run := decisionWaitRunRow(t, fixture, runID); run.Reason == ReasonGoalStateReached {
			break
		}
	}
	if run := decisionWaitRunRow(t, fixture, runID); run.Reason != ReasonGoalStateReached {
		t.Fatalf("test precondition violated: the run never reached goal_state_reached on its own, got reason=%s", run.Reason)
	}

	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	d2 := admitTestDecisionRequest(t, store, batch, runID, "op-2", nil, now)
	authority := testHoldAuthority("operator-1")
	if _, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", authority, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveDecisionRequest(d2.ID, denyOutcomeForStore(), "no", authority, now); err != nil {
		t.Fatal(err)
	}

	// Re-reconciling the SAME, already goal-state-reached run must now find
	// the two newly resolved, undelivered decisions and propose exactly ONE
	// coalesced resumption for both - never one operation per decision.
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	resumed := decisionResumedOperations(t, fixture, runID)
	if len(resumed) != 1 {
		t.Fatalf("expected exactly one decision-resumed operation covering both decisions, got %d: %+v", len(resumed), resumed)
	}
}

// TestIsResumptionBindingRecognizesBothContinuationAndDecisionResumeShapes
// is #508 review P4b §3/§5's budget-sharing proof at the unit level: a
// decision-resumed binding spends the SAME finite continuation ceiling a
// checkpoint continuation already does (startedContinuationBindings,
// continuationCeilingReached, reconciler.go), never a second, unbounded
// resource - the one fact that integration relies on, isolated from the
// rest of the (already-proven, unchanged) ceiling machinery.
func TestIsResumptionBindingRecognizesBothContinuationAndDecisionResumeShapes(t *testing.T) {
	cases := []struct {
		binding string
		want    bool
	}{
		{"continuation|abc123", true},
		{"decision-resumed|abc|def", true},
		{"initial|1|base", false},
		{"feedback|rev|digest", false},
		{"remediation|rev|class", false},
	}
	for _, c := range cases {
		if got := isResumptionBinding(c.binding); got != c.want {
			t.Fatalf("isResumptionBinding(%q) = %t, want %t", c.binding, got, c.want)
		}
	}
}

// TestADecisionResumptionSurvivesRestartWithoutDuplicating is #508 review
// P4b's restart/replay proof: once a decision-resumed operation exists, a
// close/reopen of the durable store followed by a fresh Reconcile call must
// never mint a second one for the same undelivered set.
func TestADecisionResumptionSurvivesRestartWithoutDuplicating(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", []int{fleetFirstIssue})
	runID := view.Items[0].RunID
	worker.say(runID, decisionWaitDocument(t), false)

	tick := func() {
		t.Helper()
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	var waiting EngineeringRun
	for range 40 {
		tick()
		waiting = decisionWaitRunRow(t, fixture, runID)
		if waiting.Reason == ReasonDecisionPending {
			break
		}
	}
	if waiting.Reason != ReasonDecisionPending {
		t.Fatal("test precondition violated: the run never reached the decision wait")
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected exactly one open decision request: open=%v err=%v", open, err)
	}
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: "approach-b",
		Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}
	worker.say(runID, "", false)
	for range 10 {
		tick()
		if resumed := decisionResumedOperations(t, fixture, runID); len(resumed) > 0 {
			break
		}
	}
	before := decisionResumedOperations(t, fixture, runID)
	if len(before) != 1 {
		t.Fatalf("expected exactly one decision-resumed operation before restart, got %d", len(before))
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
	supervisor = fixture.supervisor()
	for range 5 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}

	after := decisionResumedOperations(t, fixture, runID)
	if len(after) != 1 {
		t.Fatalf("expected exactly one decision-resumed operation after restart (no duplicate), got %d: %+v", len(after), after)
	}
	if after[0].ID != before[0].ID || after[0].IdempotencyKey != before[0].IdempotencyKey {
		t.Fatalf("the operation's own identity changed across restart: before=%+v after=%+v", before[0], after[0])
	}
}
