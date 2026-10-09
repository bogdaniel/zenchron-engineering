package runtime

// #508 P4b review B3/B4/F3 and the additional required regressions: decision
// delivery is journalled on every reached-worker exit, scoped to this run's
// own decisions, bounded, preflighted before any provider start, and fails
// closed on corrupt evidence. See decision_resumption_test.go for the
// binding-selection and admission mechanism (review B1/B2/F1/F2) this
// delivery evidence depends on.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestAnOversizedOwnDecisionSetRefusesBeforeAnyProviderStart is #508 review
// F3's required proof that the delivery-size bound is enforced BEFORE
// Provider.Execute ever runs, not discovered only after a worker already
// saw the oversized set: maxDeliveredDecisions+1 of this run's own
// decisions are resolved before its first execution.invoke operation is
// ever dispatched, so decisionDeliveryPreflight must refuse it with ZERO
// provider starts, never "return OperationFailed after an unrecorded
// provider call" standing in as proof of a safe refusal.
func TestAnOversizedOwnDecisionSetRefusesBeforeAnyProviderStart(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	for i := range maxDeliveredDecisions + 1 {
		operationID := fmt.Sprintf("op-oversized-%d", i)
		message := admitTestDecisionRequest(t, store, batch, runID, operationID, nil, now)
		if _, err := store.ResolveDecisionRequest(message.ID, allowOutcomeForStore(), "go", testHoldAuthority("operator-1"), now); err != nil {
			t.Fatal(err)
		}
	}
	provider := &countingProvider{}
	fixture.deps.Provider = provider

	supervisor := fixture.supervisor()
	for range 10 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	if provider.calls != 0 {
		t.Fatalf("expected ZERO provider starts once this run's own undelivered decisions exceed the %d bound, got %d calls", maxDeliveredDecisions, provider.calls)
	}
	run, found, err := store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run unreadable: found=%t err=%v", found, err)
	}
	if run.Disposition == Completed {
		t.Fatalf("expected the oversized run to never complete via a dispatch that was refused before it started, got disposition=%s", run.Disposition)
	}
}

// verificationLeakProvider grants, on its leakOnCount'th call for runID, a
// nested verification permit it never itself releases - simulating a tool
// that forked a background process and never confirmed its own cleanup
// (#508 review F3's required early-exit reproduction). invokeExecution's
// own VerificationCleanupPending check, run immediately after
// Provider.Execute returns and strictly BEFORE the later common delivery-
// injection point, must then force the EARLY recorded() exit even though
// the provider itself reported a bounded-retryable failure of its own. The
// scheduler's OWN lease acquisition (Scheduler.Next) refuses to lease
// anything further for this run while the permit stays pending, so the
// test itself - never this provider - releases it once that first,
// forced failure durably lands.
type verificationLeakProvider struct {
	*messagingWorker
	scheduler   Scheduler
	runID       string
	leakOnCount int
	count       int
	permit      *VerificationPermit
	t           *testing.T
}

func (p *verificationLeakProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if request.RunID != p.runID {
		return p.messagingWorker.Execute(ctx, request)
	}
	p.count++
	if p.count != p.leakOnCount {
		return p.messagingWorker.Execute(ctx, request)
	}
	parent := ExecutionAttemptRef{RunID: request.RunID, OperationID: request.OperationID, Attempt: request.Attempt}
	requested, reqErr := p.scheduler.RequestVerification(parent, "leak-tool", "leaked-tool-process")
	if reqErr != nil {
		p.t.Fatalf("test setup: request verification: %v", reqErr)
	}
	if ok, acqErr := p.scheduler.AcquireVerification(requested); acqErr != nil || !ok {
		p.t.Fatalf("test setup: acquire verification: ok=%t err=%v", ok, acqErr)
	}
	p.permit = &requested
	// A bounded-retryable class, reported by the provider itself exactly as
	// a real transient failure would be - the leaked tool process is a
	// SEPARATE, additional fact invokeExecution's own cleanup check
	// discovers, never something this result has to name.
	return ExecutionResult{ProviderID: "fleet-worker", Outcome: execution.Failed,
		Failure: &ProviderFailure{Classification: FailureTransientProvider}}, nil
}

// TestAnEarlyCleanupPendingExitStillJournalsDeliveryExactlyOnce is #508
// review F3's required early-exit reproduction: the decision-resumed
// operation's FIRST physical attempt reaches the worker and succeeds from
// the PROVIDER's own point of view, but leaks a nested verification permit
// (verificationLeakProvider), forcing invokeExecution's EARLY cleanup-
// pending recorded() exit rather than the later common success path. That
// early exit must still journal exactly one EventDecisionsDelivered for
// attempt 1; the retry (attempt 2), once the leaked permit is released,
// succeeds normally and journals its own, exactly once; and a fresh load
// after restart derives exactly one distinct delivered decision id, never
// a duplicate binding for the same resolution.
func TestAnEarlyCleanupPendingExitStillJournalsDeliveryExactlyOnce(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	inner := &messagingWorker{fleetProvider: fixture.worker, documents: map[string]string{}, seeded: map[string]bool{}}
	scheduler := Scheduler{Store: fixture.store, Clock: fixture.clock, Owner: "owner-1", Liveness: fixture.deps.Liveness, MaxConcurrentVerifications: 10}
	worker := &verificationLeakProvider{messagingWorker: inner, scheduler: scheduler, runID: "", leakOnCount: 2, t: t}
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

	// The leaked permit blocks the scheduler from leasing ANYTHING further
	// for this run (Scheduler.Next), so the forced failure must land and
	// settle BEFORE release - never within the same pass the retry needs.
	var firstAttemptFailed bool
	for range 20 {
		tick()
		resumed := decisionResumedOperations(t, fixture, runID)
		if len(resumed) == 1 && resumed[0].State == OperationFailed {
			firstAttemptFailed = true
			break
		}
	}
	if !firstAttemptFailed {
		t.Fatal("test precondition violated: the leaked-permit attempt never failed")
	}
	if worker.permit == nil {
		t.Fatal("test setup: the permit was never leaked")
	}
	if err := scheduler.ReleaseVerification(*worker.permit); err != nil {
		t.Fatal(err)
	}

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
		t.Fatalf("expected exactly one decision-resumed operation, got %d: %+v", len(resumed), resumed)
	}
	if resumed[0].State != Succeeded {
		t.Fatalf("expected the SAME decision-resumed operation to eventually succeed once the leaked permit is released, got state=%s attempt=%d", resumed[0].State, resumed[0].Attempt)
	}

	attempt1Deliveries, attempt2Deliveries := 0, 0
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if payload.OperationID != resumed[0].ID {
			continue
		}
		switch payload.Attempt {
		case 1:
			attempt1Deliveries++
		case 2:
			attempt2Deliveries++
		default:
			t.Fatalf("unexpected physical attempt in a delivery record: %+v", payload)
		}
	}
	if attempt1Deliveries != 1 {
		t.Fatalf("expected EXACTLY ONE delivery journalled for the leaked-permit cleanup-pending early exit (attempt 1), got %d", attempt1Deliveries)
	}
	if attempt2Deliveries != 1 {
		t.Fatalf("expected exactly one delivery for the successful retry (attempt 2), got %d", attempt2Deliveries)
	}

	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.deliveredDecisionIDs()); got != 1 {
		t.Fatalf("expected exactly one distinct delivered decision id after restart, never a duplicate binding, got %d: %+v", got, reloaded.deliveredDecisionIDs())
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
