package runtime

// #508 P4b: the bounded, same-RunID decision continuation. This file covers
// the binding-selection and admission mechanism (review B1/B2/F1/F2); see
// decision_delivery_test.go for the delivery-journalling, bound and fail-
// closed tests (review B3/B4/F3 and the additional regressions).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
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
// #508 review B2's admission-fence proof at the unit level: a decision-
// resumed operation's FIRST admission is validated against a FRESH read
// inside AdmitDecisionResumption's own transaction, not the state loaded
// when the binding was first derived - a request superseded after its
// resolution was written must refuse the admission rather than pin a
// context that no longer matches its own claim.
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

	fresh, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	_, refusal := engine.admitDecisionResumption(fresh, operation)
	if refusal == nil {
		t.Fatal("expected the stale, superseded decision-resumed operation's admission to be refused, got nil (proceed)")
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
// continuationCeilingReached, execution_continuation_budget.go), never a
// second, unbounded resource - the one fact that integration relies on,
// isolated from the rest of the (already-proven, unchanged) ceiling
// machinery.
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

// failOnceProvider lets exactly one chosen invocation of one run reach the
// worker and then fail with a bounded-RETRYABLE class (FailureTransientProvider,
// never a checkpoint), while every other invocation of that run keeps the
// wrapped provider's own ordinary behaviour. It exists to drive #508 review
// B1's exact failure shape: a decision-resumed operation's FIRST physical
// attempt reaches the worker (so it delivers) and then fails, and its RETRY
// must proceed under the SAME operation, not be refused as stale against
// its own prior delivery.
type failOnceProvider struct {
	*messagingWorker
	runID       string
	failOnCount int
	count       int
}

func (p *failOnceProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if request.RunID == p.runID {
		p.count++
		if p.count == p.failOnCount {
			return ExecutionResult{ProviderID: "fleet-worker", Outcome: execution.Failed,
				Failure: &ProviderFailure{Classification: FailureTransientProvider}}, nil
		}
	}
	return p.messagingWorker.Execute(ctx, request)
}

// TestAFailedAfterDeliveryDecisionResumptionRetriesTheSameOperation is #508
// review B1's required end-to-end test: the decision-resumed operation's
// FIRST physical attempt reaches the worker (so EventDecisionsDelivered is
// journalled for it) and then fails with a bounded-retryable class; its
// RETRY - the SAME operation, SAME idempotency key - reaches the worker
// again with the SAME resolution facts and succeeds, never refused as
// stale against its own prior delivery, and never a second, distinct
// decision-resumed operation.
func TestAFailedAfterDeliveryDecisionResumptionRetriesTheSameOperation(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	inner := &messagingWorker{fleetProvider: fixture.worker, documents: map[string]string{}, seeded: map[string]bool{}}
	worker := &failOnceProvider{messagingWorker: inner, runID: "", failOnCount: 2}
	fixture.deps.Provider = worker
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	worker.runID = runID
	inner.say(runID, decisionWaitDocument(t), false)

	supervisor := fixture.supervisor()
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
	inner.say(runID, "", false)

	// Drive far enough for the decision-resumed operation's FIRST attempt
	// to fail (count reaches failOnCount) and its RETRY to then succeed.
	var settled EngineeringRun
	for range 20 {
		tick()
		settled = decisionWaitRunRow(t, fixture, runID)
		if settled.Reason != ReasonDecisionPending && settled.Disposition != Active {
			break
		}
	}

	resumed := decisionResumedOperations(t, fixture, runID)
	if len(resumed) != 1 {
		t.Fatalf("expected exactly one decision-resumed operation despite its first attempt failing, got %d: %+v", len(resumed), resumed)
	}
	if resumed[0].State != Succeeded {
		t.Fatalf("expected the SAME decision-resumed operation to eventually succeed on retry, got state=%s attempt=%d", resumed[0].State, resumed[0].Attempt)
	}
	if resumed[0].Attempt < 2 {
		t.Fatalf("expected the operation to have been retried (attempt >= 2), got attempt=%d", resumed[0].Attempt)
	}

	deliveries := 0
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err == nil && payload.OperationID == resumed[0].ID {
			deliveries++
		}
	}
	if deliveries < 2 {
		t.Fatalf("expected delivery to be journalled on BOTH the failed first attempt and the successful retry, got %d records for this operation", deliveries)
	}
}

// TestARetryRefusesRatherThanSubstitutesWhenItsPinnedDecisionIsSuperseded is
// #508 review F1's required proof, at the exact two calls invokeExecution
// itself makes before every dispatch (admitDecisionResumption then
// recoverDecisionResumptionContext - the scheduler's own immediate-retry
// timing is not under test here, decisionResumptionAdmissionByOperationID
// and decision_resumption_test's B1/B2 tests already cover that): D1's
// admission is pinned once, the FIRST attempt's own recovery renders a
// context carrying exactly D1, D1 is then superseded (D2 admitted in its
// place), and the SAME operation's retry reuses the SAME pinned admission
// (B1) but its OWN dispatch-time recovery refuses once D1 is no longer
// live - never silently substituting D2's content, and never claiming D1
// delivered a second time, because no context is ever rendered at all.
func TestARetryRefusesRatherThanSubstitutesWhenItsPinnedDecisionIsSuperseded(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	if _, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now); err != nil {
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

	// FIRST attempt: admit and recover - exactly what invokeExecution does
	// right before rendering the provider's own context.
	admission, refusal := engine.admitDecisionResumption(state, operation)
	if refusal != nil {
		t.Fatalf("expected the first admission to succeed, got refusal: %+v", refusal)
	}
	firstResolved, refusal := engine.recoverDecisionResumptionContext(admission)
	if refusal != nil {
		t.Fatalf("expected the first recovery to succeed, got refusal: %+v", refusal)
	}
	if len(firstResolved) != 1 || firstResolved[0].RequestID != d1.ID {
		t.Fatalf("expected the first attempt's context to carry exactly D1, got %+v", firstResolved)
	}
	communication, resolvedInView, err := communicationContext(store, state.run, firstResolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolvedInView) != 1 || resolvedInView[0].RequestID != d1.ID {
		t.Fatalf("expected the rendered provider context's own resolved_decisions to agree with the pinned admission, got %+v", resolvedInView)
	}
	if !strings.Contains(communication, d1.ID) {
		t.Fatalf("expected the rendered ExecutionRequest.Communication to name D1, got %s", communication)
	}

	// D1 is superseded by D2 BETWEEN the pinned admission and the retry -
	// exactly the review F1 scenario.
	if _, err := admitTestMessage(store, batch, runID, "op-2",
		orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "revised", Supersedes: d1.ID}, now); err != nil {
		t.Fatal(err)
	}

	fresh, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	retryAdmission, refusal := engine.admitDecisionResumption(fresh, operation)
	if refusal != nil {
		t.Fatalf("expected the SAME operation's admission to be reused unchanged (review B1), got refusal: %+v", refusal)
	}
	if len(retryAdmission.Decisions) != 1 || retryAdmission.Decisions[0].RequestID != d1.ID {
		t.Fatalf("expected the retry to reuse the SAME pinned admission (D1), got %+v", retryAdmission)
	}
	retryResolved, refusal := engine.recoverDecisionResumptionContext(retryAdmission)
	if refusal == nil {
		t.Fatalf("expected the retry's dispatch-time recovery to refuse once D1 went non-live, never silently substitute D2, got resolved=%+v", retryResolved)
	}
	if class := refusal.result.(executionRecord).FailureClass; class != FailureDecisionBindingStale {
		t.Fatalf("expected FailureDecisionBindingStale, got %v", class)
	}
}

// TestAdmitDecisionResumptionRefusesWhenANewResolutionLandsBeforeAdmission
// is #508 review F2's CONTENDER-FIRST ordering with a NEW resolution (not a
// supersession) as the perturbing fact: a caller's "old load" binding,
// derived when only D1 was resolved, is stale the moment D2 resolves before
// AdmitDecisionResumption is ever called - the fresh re-derivation inside
// admission now sees BOTH as undelivered, computes a DIFFERENT binding, and
// refuses before any provider is ever reached. No concurrency is needed to
// prove this ordering, exactly like the supersession-based contender-first
// test above.
func TestAdmitDecisionResumptionRefusesWhenANewResolutionLandsBeforeAdmission(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	d2 := admitTestDecisionRequest(t, store, batch, runID, "op-2", nil, now)
	resolvedD1, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	contractRevision, candidateSubject := realContractAndCandidate(t, store, runID)
	staleBinding := decisionResumeBinding(contractRevision, candidateSubject, []orchestration.DecisionResolution{resolvedD1})

	if _, err := store.ResolveDecisionRequest(d2.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AdmitDecisionResumption(runID, "op-x", staleBinding, now); err == nil {
		t.Fatal("expected the admission to refuse a binding that no longer reflects D2's own, later resolution")
	}
	if _, found, err := decisionResumptionAdmissionByOperationID(store.db, "op-x"); err != nil || found {
		t.Fatalf("a refused admission must leave no durable row: found=%t err=%v", found, err)
	}
}

// TestAdmitDecisionResumptionPinsD1OnlyAndNeverExpandsWhenD2ResolvesLater is
// #508 review F2's RESOLUTION-FIRST (here: admission-first) ordering: D1's
// admission is pinned while D2 is still unresolved; D2 then resolves. A
// later read of the SAME operation's admission proves it was never
// expanded or otherwise mutated - D2 belongs to a separate, later epoch,
// never substituted or silently merged into an admission already pinned.
func TestAdmitDecisionResumptionPinsD1OnlyAndNeverExpandsWhenD2ResolvesLater(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	d2 := admitTestDecisionRequest(t, store, batch, runID, "op-2", nil, now)
	resolvedD1, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	contractRevision, candidateSubject := realContractAndCandidate(t, store, runID)
	binding := decisionResumeBinding(contractRevision, candidateSubject, []orchestration.DecisionResolution{resolvedD1})

	pinned, err := store.AdmitDecisionResumption(runID, "op-d1-only", binding, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.Decisions) != 1 || pinned.Decisions[0].RequestID != d1.ID {
		t.Fatalf("expected the admission to pin exactly D1, got %+v", pinned)
	}

	if _, err := store.ResolveDecisionRequest(d2.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now); err != nil {
		t.Fatal(err)
	}

	again, found, err := decisionResumptionAdmissionByOperationID(store.db, "op-d1-only")
	if err != nil || !found {
		t.Fatalf("expected the existing admission to still be found: found=%t err=%v", found, err)
	}
	if len(again.Decisions) != 1 || again.Decisions[0].RequestID != d1.ID {
		t.Fatalf("expected D1's admission to stay exactly as pinned, never expanded by D2's later resolution: %+v", again)
	}
}

// realContractAndCandidate reads one run's ACTUAL current contract revision
// and candidate subject straight from its own event-sourced projection -
// never a fixed test literal - so a binding built from them is the SAME one
// AdmitDecisionResumption's own fresh re-derivation (review F2) will
// recompute.
func realContractAndCandidate(t *testing.T, store *SQLiteOperationStore, runID string) (string, string) {
	t.Helper()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	return projection.Contract.Revision, projection.CandidateRevision
}

// TestAdmitDecisionResumptionRefusesWhenSupersededBeforeAdmission is #508
// review B2's CONTENDER-FIRST ordering: a decision superseded before
// AdmitDecisionResumption is ever called refuses the whole admission and
// leaves no durable row. No concurrency is needed to prove this ordering.
func TestAdmitDecisionResumptionRefusesWhenSupersededBeforeAdmission(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	resolved, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitTestMessage(store, batch, runID, "op-2",
		orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "revised", Supersedes: d1.ID}, now); err != nil {
		t.Fatal(err)
	}

	contractRevision, candidateSubject := realContractAndCandidate(t, store, runID)
	binding := decisionResumeBinding(contractRevision, candidateSubject, []orchestration.DecisionResolution{resolved})
	if _, err := store.AdmitDecisionResumption(runID, "op-x", binding, now); err == nil {
		t.Fatal("expected the admission to refuse a decision already superseded before it was ever called")
	}
	if _, found, err := decisionResumptionAdmissionByOperationID(store.db, "op-x"); err != nil || found {
		t.Fatalf("a refused admission must leave no durable row: found=%t err=%v", found, err)
	}
}

// admitDecisionResumptionUnderHeldTx replicates AdmitDecisionResumption's own
// steps using its own production helpers (decisionResumptionAdmissionByOperationID,
// queryEvents, Project, resolvedOwnDecisionRequestsTx, validateLiveAndCurrent),
// inside a transaction the CALLER opened and controls the commit of - the
// same technique decision_linearizability_test.go's resolveUnderHeldTx
// already uses, for the same reason: it holds the exact window between this
// admission's validation and its own insert open for an externally observed
// duration.
func admitDecisionResumptionUnderHeldTx(tx *sql.Tx, runID, operationID, binding string, now time.Time) (DecisionResumptionAdmission, error) {
	if existing, found, err := decisionResumptionAdmissionByOperationID(tx, operationID); err != nil {
		return DecisionResumptionAdmission{}, err
	} else if found {
		return existing, nil
	}
	events, err := queryEvents(tx, runID)
	if err != nil {
		return DecisionResumptionAdmission{}, err
	}
	projection, err := Project(events)
	if err != nil {
		return DecisionResumptionAdmission{}, err
	}
	delivered, err := deliveredDecisionIDsFromEvents(events)
	if err != nil {
		return DecisionResumptionAdmission{}, err
	}
	liveResolved, err := resolvedOwnDecisionRequestsTx(tx, runID)
	if err != nil {
		return DecisionResumptionAdmission{}, err
	}
	contractRevision, candidateSubject := projection.Contract.Revision, projection.CandidateRevision
	undelivered := undeliveredOf(liveResolved, delivered)
	if expected := decisionResumeBinding(contractRevision, candidateSubject, undelivered); expected != binding {
		return DecisionResumptionAdmission{}, fmt.Errorf("the undelivered decision set, contract or candidate no longer matches (recomputed binding %q, got %q)", expected, binding)
	}
	decisions := make([]DeliveredDecision, len(undelivered))
	for i, r := range undelivered {
		if err := validateLiveAndCurrent(tx, r.RequestID); err != nil {
			return DecisionResumptionAdmission{}, err
		}
		decisions[i] = DeliveredDecision{RequestID: r.RequestID, ResolutionID: r.ID}
	}
	admission := DecisionResumptionAdmission{
		SchemaVersion: DecisionResumptionAdmissionSchemaVersion, OperationID: operationID, Binding: binding,
		Decisions: decisions, ContractRevision: contractRevision, CandidateSubject: candidateSubject, AdmittedAt: now,
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		return DecisionResumptionAdmission{}, err
	}
	if _, err := tx.Exec(`INSERT INTO decision_resumption_admissions (operation_id, binding, admitted_unix_nano, document)
		VALUES (?, ?, ?, ?) ON CONFLICT(operation_id) DO NOTHING`,
		operationID, binding, now.UnixNano(), string(document)); err != nil {
		return DecisionResumptionAdmission{}, err
	}
	return admission, nil
}

// TestAdmitDecisionResumptionBlocksConcurrentSupersessionAndPinsRegardless
// is #508 review B2's RESOLUTION-FIRST (here: admission-first) ordering,
// forced open deterministically: the admission's own validate-then-insert
// is held past its insert but before its commit, a SINGLE contender's
// supersession is given a ready-to-attempt signal and proven blocked for a
// bound, the transaction then commits, and the contender's own completion
// is awaited afterward on the SAME channel. The supersession, landing only
// AFTER the admission committed, belongs to a LATER epoch: a further read
// of the SAME operation's admission proves it was never retroactively
// rewritten - the second store handle never silently mutates an already-
// admitted context.
func TestAdmitDecisionResumptionBlocksConcurrentSupersessionAndPinsRegardless(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	second, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	resolved, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "op-decision-resumed-under-test"
	contractRevision, candidateSubject := realContractAndCandidate(t, store, runID)
	binding := decisionResumeBinding(contractRevision, candidateSubject, []orchestration.DecisionResolution{resolved})

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitDecisionResumptionUnderHeldTx(tx, runID, operationID, binding, now); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		close(ready)
		_, err := admitTestMessage(second, batch, runID, "op-2",
			orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "revised", Supersedes: d1.ID}, now)
		completed <- err
	}()
	<-ready
	select {
	case err := <-completed:
		t.Fatalf("the ONE contender's supersession landed while the admission's own transaction was still open (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the contender's supersession never completed after the admission's transaction committed")
	}

	pinned, err := store.AdmitDecisionResumption(runID, operationID, binding, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.Decisions) != 1 || pinned.Decisions[0].RequestID != d1.ID {
		t.Fatalf("the admission's pinned snapshot changed after a later-landing supersession: %+v", pinned)
	}
}
