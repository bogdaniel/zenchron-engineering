package runtime

// #508 P4b: the bounded, same-RunID decision continuation.

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

	binding := decisionResumeBinding("contract-rev", "", []orchestration.DecisionResolution{resolved})
	if _, err := store.AdmitDecisionResumption("op-x", binding, "contract-rev", "", []orchestration.DecisionResolution{resolved}, now); err == nil {
		t.Fatal("expected the admission to refuse a decision already superseded before it was ever called")
	}
	if _, found, err := decisionResumptionAdmissionByOperationID(store.db, "op-x"); err != nil || found {
		t.Fatalf("a refused admission must leave no durable row: found=%t err=%v", found, err)
	}
}

// admitDecisionResumptionUnderHeldTx replicates AdmitDecisionResumption's own
// steps using its own production helpers (decisionResumptionAdmissionByOperationID,
// findDecisionRequestTx), inside a transaction the CALLER opened and controls
// the commit of - the same technique decision_linearizability_test.go's
// resolveUnderHeldTx already uses, for the same reason: it holds the exact
// window between this admission's validation and its own insert open for an
// externally observed duration.
func admitDecisionResumptionUnderHeldTx(tx *sql.Tx, operationID, binding, contractRevision, candidateSubject string,
	proposed []orchestration.DecisionResolution, now time.Time) (DecisionResumptionAdmission, error) {
	if existing, found, err := decisionResumptionAdmissionByOperationID(tx, operationID); err != nil {
		return DecisionResumptionAdmission{}, err
	} else if found {
		return existing, nil
	}
	decisions := make([]DeliveredDecision, len(proposed))
	for i, r := range proposed {
		ref, current, err := findDecisionRequestTx(tx, r.RequestID)
		if err != nil {
			return DecisionResumptionAdmission{}, err
		}
		if !ref.Live {
			return DecisionResumptionAdmission{}, fmt.Errorf("decision request %s is no longer live", r.RequestID)
		}
		if ref.Subject != nil && (current == nil || *current != ref.Subject.Revision) {
			return DecisionResumptionAdmission{}, fmt.Errorf("decision request %s is bound to a subject that is no longer current", r.RequestID)
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
	binding := decisionResumeBinding("contract-rev", "", []orchestration.DecisionResolution{resolved})

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitDecisionResumptionUnderHeldTx(tx, operationID, binding, "contract-rev", "", []orchestration.DecisionResolution{resolved}, now); err != nil {
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

	pinned, err := store.AdmitDecisionResumption(operationID, binding, "contract-rev", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.Decisions) != 1 || pinned.Decisions[0].RequestID != d1.ID {
		t.Fatalf("the admission's pinned snapshot changed after a later-landing supersession: %+v", pinned)
	}
}

// TestDecisionDeliveryEntryBoundsToOwnDecisionsEvenInALargeMultiUnitScope is
// #508 review B4's required proof: communicationContext's OWN return is
// scope-wide (every unit's resolved decisions, not only this run's), and a
// real multi-unit batch can easily carry more than
// orchestration.MaxMessagesPerInvocation (8) live resolved decisions spread
// across several units. decisionDeliveryEntry must still succeed and carry
// ONLY this run's own decision(s) - never fail validation merely because the
// WHOLE scope happens to be large, and never silently lose this run's own
// provenance by trying to represent the other units' decisions too.
func TestDecisionDeliveryEntryBoundsToOwnDecisionsEvenInALargeMultiUnitScope(t *testing.T) {
	own := []orchestration.DecisionResolution{{RequestID: "own-1", ID: "res-own-1"}}
	shown := append([]orchestration.DecisionResolution{}, own...)
	for i := range 9 {
		shown = append(shown, orchestration.DecisionResolution{
			RequestID: fmt.Sprintf("other-unit-decision-%d", i), ID: fmt.Sprintf("res-other-%d", i),
		})
	}
	if len(shown) <= orchestration.MaxMessagesPerInvocation {
		t.Fatalf("test precondition violated: the whole-scope set (%d) must exceed MaxMessagesPerInvocation (%d)", len(shown), orchestration.MaxMessagesPerInvocation)
	}

	entry, ok, err := decisionDeliveryEntry(DecisionResumptionAdmission{}, own, shown, "op-1", 1, "contract-rev", "subject")
	if err != nil {
		t.Fatalf("a whole-batch scope of %d resolved decisions must not fail delivery validation when only 1 is THIS run's own: %v", len(shown), err)
	}
	if !ok {
		t.Fatal("expected a delivery entry")
	}
	payload, ok := entry.Payload.(DecisionsDeliveredPayload)
	if !ok {
		t.Fatalf("unexpected payload type %T", entry.Payload)
	}
	if len(payload.Decisions) != 1 || payload.Decisions[0].RequestID != "own-1" {
		t.Fatalf("expected the delivery to carry ONLY this run's own decision, got %+v", payload.Decisions)
	}
	if err := validateDecisionsDelivered(payload); err != nil {
		t.Fatalf("the bounded, own-only delivery payload must pass its own schema validation: %v", err)
	}
}

// TestDecisionDeliveryEntryFailsClosedWhenGenuinelyOversized is the
// complementary proof: if THIS run's own delivered set itself ever exceeds
// maxDeliveredDecisions, the result is an ERROR - never a silently
// truncated or dropped delivery record (review B4).
func TestDecisionDeliveryEntryFailsClosedWhenGenuinelyOversized(t *testing.T) {
	var own []orchestration.DecisionResolution
	for i := range maxDeliveredDecisions + 1 {
		own = append(own, orchestration.DecisionResolution{RequestID: fmt.Sprintf("own-%03d", i), ID: fmt.Sprintf("res-%03d", i)})
	}
	_, _, err := decisionDeliveryEntry(DecisionResumptionAdmission{}, own, own, "op-1", 1, "contract-rev", "subject")
	if err == nil {
		t.Fatalf("expected an error when this run's own delivered set (%d) exceeds the %d bound, got none", len(own), maxDeliveredDecisions)
	}
}

// TestDecisionResumptionAdmissionOperationIDStaysWithinTheFieldBound is the
// author's own missing regression this review named explicitly: a real bug
// found and fixed during this implementation (an early binding format that
// embedded the full contract revision and candidate subject verbatim
// overran the 200-byte operation_id field bound every event payload
// enforces, silently exhausting a decision-resumed operation's attempts
// with no diagnostic). Proves, with WORST-CASE-LENGTH inputs a contract
// revision and a candidate revision can actually take in this build (a full
// reassessment-suffixed revision over a 40-character commit SHA, per
// bindContractCompile/ReassessmentCompletedPayload), that the resulting
// operation id still fits, and that operation.planned/before/after and
// EventDecisionsDelivered all validate against it without error.
func TestDecisionResumptionAdmissionOperationIDStaysWithinTheFieldBound(t *testing.T) {
	longCommit := strings.Repeat("a", 40)
	contractRevision := "1-" + longCommit + "-next" // the exact reassessment-suffixed shape operations.go's commitCandidate produces
	candidateSubject := longCommit
	undelivered := []orchestration.DecisionResolution{{RequestID: strings.Repeat("r", 64), ID: strings.Repeat("s", 64)}}
	binding := decisionResumeBinding(contractRevision, candidateSubject, undelivered)

	runID := "run-" + strings.Repeat("f", 32)
	operationID := runID + ":" + OpExecutionInvoke + ":" + operationKey(OpExecutionInvoke, binding)
	const fieldBound = 200
	if len(operationID) > fieldBound {
		t.Fatalf("worst-case decision-resumed operation id is %d bytes, exceeds the %d byte field bound: %s", len(operationID), fieldBound, operationID)
	}

	// The field this review's own fix actually protects: operation_id as
	// carried by the EVENT payloads that reference it (messages.observed,
	// handoff.reported, decision.delivered) - required()/bounded() enforces
	// maxPayloadFieldBytes (200) on THIS string, never on RunOperation.ID
	// itself. decisionDeliveryEntry + validateDecisionsDelivered is the
	// exact mechanism that broke (handoff.reported's own validator, the
	// same required()/bounded() pair, broke identically on the same
	// oversized id before this binding was digested).
	entry, ok, err := decisionDeliveryEntry(DecisionResumptionAdmission{}, undelivered, undelivered, operationID, 1, contractRevision, candidateSubject)
	if err != nil || !ok {
		t.Fatalf("a worst-case-length decision delivery must still validate: ok=%t err=%v", ok, err)
	}
	if err := validateDecisionsDelivered(entry.Payload.(DecisionsDeliveredPayload)); err != nil {
		t.Fatalf("a worst-case-length decision delivery payload must pass schema validation: %v", err)
	}
}

// TestDeliveredDecisionIDsFromEventsFailsClosedOnAMalformedEvent is #508
// review's additional required fix: deliveredDecisionIDs must distinguish
// "no delivery event exists" (legacy absence, fine) from "a delivery event
// exists but cannot be decoded" (present-but-invalid evidence) - the latter
// must fail closed (an error load() propagates, failing the whole
// Reconcile pass) rather than silently forgetting a real delivery.
func TestDeliveredDecisionIDsFromEventsFailsClosedOnAMalformedEvent(t *testing.T) {
	events := []EngineeringEvent{
		{Type: EventDecisionsDelivered, Payload: []byte(`{"operation_id":`)}, // truncated, invalid JSON
	}
	if _, err := deliveredDecisionIDsFromEvents(events); err == nil {
		t.Fatal("expected a malformed EventDecisionsDelivered payload to fail closed, got nil error")
	}
	// A run with NO delivery event at all (legacy absence) is unaffected.
	if ids, err := deliveredDecisionIDsFromEvents(nil); err != nil || len(ids) != 0 {
		t.Fatalf("expected no delivery events to decode cleanly to an empty set: ids=%v err=%v", ids, err)
	}
}
