package runtime

// The #508 acceptance for a WORKER-EMITTED #473 decision_request - distinct
// from the pre-activation WorkUnitHold proved in workgraph_decision_test.go:
// a run that has asked a live question is held with zero active capacity
// until an authorized operator resolves it, and the compiled context a later
// invocation is given reflects the resolution, never the open question.
//
// What this file does NOT claim: that resolving a decision automatically
// produces a SECOND provider invocation of the SAME run through today's
// existing continuation machinery. It cannot, for a structural reason proven
// empirically while building this: a message report is only ever observed
// (TestEveryFocusedSourceIsClassified et al. aside, see
// appendMessageObservation's call site in operations.go) from an invocation
// the engine already treats as COMPLETING - Outcome succeeded, not a
// checkpoint stop, not a feedback/review/continuation-unresolved attempt. The
// ONLY existing mechanism that re-invokes a run without a brand new
// EngineeringRun is the #489 checkpoint-then-continue pair, and a checkpoint
// STOP is a FAILED outcome specifically so the engine does not treat it as
// finished - which means its message slot is never read at all. So the one
// invocation whose message CAN be observed (a completing one) is, by the same
// stroke, the one invocation after which this engine's own planner considers
// the engineering operation done and schedules no automatic continuation -
// with or without #508. Composing "ask and keep going" into one automatic
// run would need a genuine extension to state.plan()'s continuation model
// (reconciler.go), which is both out of #508's scope and too large a change
// to make safely inside this review-fix pass. #508 holds up its own half
// completely: nothing is invoked while a question is open, and the fact is
// correctly compiled the moment it resolves (TestResumedContextCarries...
// below proves exactly that, directly). What closes the loop for a run that
// needs to keep doing engineering work after a decision is an explicit
// operator action (`autonomy resume`) or a fresh run, exactly as a goal-state
// -reached run already requires today for any other kind of human answer.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// compiledDecisions is the one slice of communicationView this file reads:
// which decisions a compiled context shows as still open versus resolved.
type compiledDecisions struct {
	Decisions         []orchestration.EngineeringMessage `json:"open_decisions"`
	ResolvedDecisions []orchestration.DecisionResolution `json:"resolved_decisions"`
}

func decodeCompiledDecisions(t *testing.T, document string) compiledDecisions {
	t.Helper()
	var decoded compiledDecisions
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("compiled context is not valid JSON: %v (%s)", err, document)
	}
	return decoded
}

func decisionWaitDocument(t *testing.T) string {
	t.Helper()
	return messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindDecisionRequest, Purpose: "which migration strategy?", Body: "two viable approaches with different risk profiles",
	})
}

// TestAWorkerEmittedDecisionRequestParksItsRunWithZeroCapacity is #508
// acceptance 2, 8, 9, 13, 14 and 16 for a #473 message (as opposed to a
// WorkUnitHold): zero capacity while a request the run itself admitted is
// open, an authorized resolution (idempotent under retry, durable across
// restart) is what lifts it, and #472/#473's own machinery - never this
// package - decides what the run does next.
func TestAWorkerEmittedDecisionRequestParksItsRunWithZeroCapacity(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()

	issue := fleetFirstIssue
	view := fixture.orchestrate(supervisor, "claude", []int{issue})
	runID := view.Items[0].RunID
	if runID == "" {
		t.Fatal("orchestrating one issue created no run")
	}
	// A worker that asks a question and reports nothing else: there is
	// nothing further THIS invocation can do until a human answers it.
	worker.say(runID, decisionWaitDocument(t), false)

	tick := func() {
		t.Helper()
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	runRow := func() EngineeringRun {
		t.Helper()
		run, found, err := fixture.store.Run(runID)
		if err != nil || !found {
			t.Fatalf("run %s is unreadable: found=%t err=%v", runID, found, err)
		}
		return run
	}
	engineeringInvocations := func() int {
		n, _ := fixture.worker.counts(runID)
		return n
	}

	// DRIVE TO THE WAIT.
	var waiting EngineeringRun
	for range 40 {
		tick()
		waiting = runRow()
		if waiting.Disposition == Waiting && waiting.Reason == ReasonDecisionPending {
			break
		}
	}
	if waiting.Disposition != Waiting || waiting.Reason != ReasonDecisionPending {
		t.Fatalf("run settled at %s/%s, want waiting/%s", waiting.Disposition, waiting.Reason, ReasonDecisionPending)
	}
	if n := engineeringInvocations(); n != 1 {
		t.Fatalf("the worker was invoked %d times before its own question was even admitted, want 1", n)
	}

	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("expected exactly one open decision request, got %d", len(open))
	}
	requestID := open[0].ID

	// ZERO CAPACITY WHILE WAITING: several more passes must not invoke the
	// worker again or change the run's disposition.
	for range 5 {
		tick()
	}
	if n := engineeringInvocations(); n != 1 {
		t.Fatalf("the worker was invoked %d times while its question stood open, want 1 (zero capacity consumed)", n)
	}
	if run := runRow(); run.Disposition != Waiting || run.Reason != ReasonDecisionPending {
		t.Fatalf("run moved to %s/%s while its own question was still open", run.Disposition, run.Reason)
	}
	worker.say(runID, "", false)

	// AUTHORIZED RESOLUTION, including the lost-reply retry: resolving the
	// IDENTICAL request twice is idempotent, never a second write and never a
	// refusal.
	outcome := orchestration.DecisionOutcome{Kind: orchestration.DecisionSelectedOption, Value: "approach-b"}
	resolve := ControlRequest{
		DecisionID: requestID, DecisionOutcomeKind: outcome.Kind, DecisionOutcomeValue: outcome.Value,
		Operator: "operator@example", Note: "go with approach B: lower migration risk",
	}
	first, err := supervisor.ResolveDecision(resolve)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	retry, err := supervisor.ResolveDecision(resolve)
	if err != nil || retry.Resolution != first.Resolution {
		t.Fatalf("a retried resolution after a lost reply must be idempotent: err=%v first=%+v retry=%+v", err, first.Resolution, retry.Resolution)
	}

	// CRASH-AFTER-WRITE / RESTART: close this store handle and reopen a
	// fresh supervisor on the SAME durable state, exactly as a restarted
	// `serve` would. The resolution, and the wait it lifted, must survive.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	fixture.store = reopened
	stored, found, err := fixture.store.DecisionResolutionByRequestID(requestID)
	if err != nil || !found || stored.Outcome != outcome {
		t.Fatalf("the resolution did not survive restart: found=%t err=%v stored=%+v", found, err, stored)
	}
	supervisor = fixture.supervisor()

	// THE GATE LIFTS: the next pass sees no open request at all, and
	// Reconcile falls through to whatever #470's own planner decides for an
	// invocation that produced nothing - goal_state_reached, same as any
	// other finished producer with nothing further to plan. #508 is not the
	// thing that decides that; it only stopped deciding it while the
	// question stood open.
	var settled EngineeringRun
	for range 10 {
		tick()
		settled = runRow()
		if settled.Reason != ReasonDecisionPending {
			break
		}
	}
	if settled.Reason == ReasonDecisionPending {
		t.Fatal("the run is still gated on its own question after it was resolved")
	}
	if open, err := fixture.store.OpenDecisionRequestsForRun(runID); err != nil || len(open) != 0 {
		t.Fatalf("expected no open decision requests after resolution: open=%v err=%v", open, err)
	}
	if n := engineeringInvocations(); n != 1 {
		t.Fatalf("the worker was invoked %d times after the gate lifted; #508 must never itself start a provider", n)
	}
}

// TestResumedContextCarriesTheResolvedDecisionNotTheOpenOne is #508
// acceptance 16 and the B1 review bug directly: communicationContext must
// join against the durable decision_resolutions store, not merely echo
// #473's own OpenDecisions. It is tested at this level - durable state in,
// compiled context out - because it is the exact seam ANY future invocation
// of this run, or a successor run resuming the same work, reads through,
// independent of which run-level mechanism eventually triggers that
// invocation.
func TestResumedContextCarriesTheResolvedDecisionNotTheOpenOne(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", []int{fleetFirstIssue})
	runID := view.Items[0].RunID
	worker.say(runID, decisionWaitDocument(t), false)

	for range 10 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if open, err := fixture.store.OpenDecisionRequestsForRun(runID); err == nil && len(open) == 1 {
			break
		}
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil || len(open) != 1 {
		t.Fatalf("the question was never admitted: open=%v err=%v", open, err)
	}
	requestID := open[0].ID

	run := runRowFor(t, fixture, runID)
	before, err := communicationContext(fixture.store, run)
	if err != nil {
		t.Fatal(err)
	}
	beforeDecoded := decodeCompiledDecisions(t, before)
	if len(beforeDecoded.Decisions) != 1 || beforeDecoded.Decisions[0].ID != requestID || len(beforeDecoded.ResolvedDecisions) != 0 {
		t.Fatalf("before resolution the question should read as open and nothing else: %+v", beforeDecoded)
	}

	outcome := orchestration.DecisionOutcome{Kind: orchestration.DecisionText, Value: "approach B: lower migration risk"}
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, DecisionOutcomeKind: outcome.Kind, DecisionOutcomeValue: outcome.Value, Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}

	after, err := communicationContext(fixture.store, run)
	if err != nil {
		t.Fatal(err)
	}
	afterDecoded := decodeCompiledDecisions(t, after)
	if len(afterDecoded.Decisions) != 0 {
		t.Fatalf("after resolution the question still reads as open: %+v", afterDecoded.Decisions)
	}
	if len(afterDecoded.ResolvedDecisions) != 1 || afterDecoded.ResolvedDecisions[0].RequestID != requestID ||
		afterDecoded.ResolvedDecisions[0].Outcome != outcome {
		t.Fatalf("after resolution the compiled context does not carry the resolution fact: %+v", afterDecoded.ResolvedDecisions)
	}
}

func runRowFor(t *testing.T, fixture *fleetFixture, runID string) EngineeringRun {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("run %s is unreadable: found=%t err=%v", runID, found, err)
	}
	return run
}
