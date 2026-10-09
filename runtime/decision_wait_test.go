package runtime

// The #508 acceptance for a WORKER-EMITTED #473 decision_request - distinct
// from the pre-activation WorkUnitHold proved in workgraph_decision_test.go:
// a run that has asked a live question is held with zero active capacity
// until an authorized operator resolves it, and the compiled context a later
// invocation is given reflects the resolution, never the open question.
//
// #508 review P4a closed two defects this file's own history found along
// the way:
//
//   - A decision_request written by a CHECKPOINTING attempt (the producer
//     mutated but was cut off, #489's shape) is now observed and, once the
//     runtime's own candidate.commit durably journals that exact checkpoint,
//     admitted for its decision_request content alone - see
//     decision_checkpoint_admission_test.go. Before P4a, that message was
//     silently discarded: appendMessageObservation ran only on the
//     COMPLETING path, and a checkpoint is a producer the runtime cut off,
//     never that path.
//   - The wait itself is unconditional now: Reconcile reads
//     OpenDecisionRequestsForRun on EVERY pass, not only when state.plan()
//     happens to want some operation that exact pass. Before P4a, a run with
//     truly nothing else to plan (the shape this file exercises - ask, change
//     nothing) could flip from Waiting/ReasonDecisionPending back to
//     Waiting/ReasonGoalStateReached once whatever UNRELATED binding
//     incidentally kept re-triggering (bindSourceObserve's epoch key, bumped
//     by the message-observed event itself) stopped re-triggering.
//
// #508 review P4b (decision_resumption.go) is what THIS file now also
// exercises directly: resolving the decision no longer merely lifts the
// wait - bindExecutionInvoke's fallback proposes exactly one fresh
// "decision-resumed|..." execution.invoke, a brand new operation on the SAME
// RunID, under its own ordinary attempt ceiling, once this run has nothing
// else left to plan. The zero-capacity test below proves the whole shape
// end to end: zero capacity while open, exactly ONE further invocation once
// resolved, never two. What P4b still does not do: if that one resumed
// invocation also finishes the engineering work and produces no FURTHER
// decision, the run completes through #470/#489's own ordinary lifecycle,
// unmodified; if it needs a THIRD invocation for some OTHER reason (not a
// decision), that remains an explicit operator action (`autonomy resume`)
// or a fresh run, exactly as any other goal-state-reached run requires today.

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

	// ZERO CAPACITY WHILE WAITING, FAR PAST THE POINT ANY UNRELATED BINDING
	// COULD STILL BE RE-TRIGGERING (#508 review P4a, D4): before the fix, this
	// wait was checked only inside "if state.plan() wants an operation", so it
	// depended on bindSourceObserve's own epoch-keyed binding happening to be
	// unsatisfied - which it is, once, right when EventMessagesObserved bumps
	// the epoch, and never again once that one re-observation runs and
	// nothing else changes. 30 ticks is comfortably past that one-time churn;
	// the run must still read Waiting/ReasonDecisionPending here, never having
	// flipped to Waiting/ReasonGoalStateReached in between.
	for range 30 {
		tick()
	}
	if n := engineeringInvocations(); n != 1 {
		t.Fatalf("the worker was invoked %d times while its question stood open, want 1 (zero capacity consumed)", n)
	}
	if run := runRow(); run.Disposition != Waiting || run.Reason != ReasonDecisionPending {
		t.Fatalf("run moved to %s/%s while its own question was still open - the wait must hold even once every unrelated binding has stopped re-triggering", run.Disposition, run.Reason)
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
	// bindExecutionInvoke's #508 P4b fallback proposes exactly one fresh
	// "decision-resumed|..." invocation - a brand new operation, the SAME
	// RunID, under its own ordinary attempt ceiling - since this run has
	// nothing else left to plan. This is the bounded same-RunID resumption
	// P4b builds; before it existed, resolving only lifted the wait and left
	// the run at goal_state_reached with no further invocation at all.
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
	if n := engineeringInvocations(); n != 2 {
		t.Fatalf("the worker was invoked %d times after the gate lifted, want exactly 2 (the original ask, and the one bounded resumption)", n)
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
	before, _, err := communicationContext(fixture.store, run, nil)
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

	after, _, err := communicationContext(fixture.store, run, nil)
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

// TestDecisionWaitPermitsOnlyObservationAndCandidateCommit is #508 review
// P4a's R2-B proof at the unit level: while a decision is open, EVERY other
// operation kind is deferred, and candidate.commit is let through only when
// something was actually wanted (never fabricated).
func TestDecisionWaitPermitsOnlyObservationAndCandidateCommit(t *testing.T) {
	cases := []struct {
		name   string
		wanted bool
		kind   string
		want   bool
	}{
		{"nothing wanted at all", false, OpCandidateCommit, false},
		{"nothing wanted, observation kind named anyway", false, OpSourceObserve, false},
		{"observation is permitted", true, OpSourceObserve, true},
		{"github observation is permitted", true, OpGitHubObserve, true},
		{"candidate.commit is permitted", true, OpCandidateCommit, true},
		{"execution.invoke is deferred", true, OpExecutionInvoke, false},
		{"assurance is deferred", true, OpAssuranceGo, false},
		{"authority evaluation is deferred", true, OpAuthorityEvaluate, false},
		{"candidate push is deferred", true, OpCandidatePush, false},
		{"handoff repair is deferred", true, OpHandoffRepair, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decisionWaitPermits(c.wanted, c.kind); got != c.want {
				t.Fatalf("decisionWaitPermits(%t, %q) = %t, want %t", c.wanted, c.kind, got, c.want)
			}
		})
	}
}

// TestAnUnreadableDecisionStoreNeverAuthorizesDispatch is #508 review P4a's
// R2-E proof: "unknown is not permission." An OpenDecisionRequestsForRun
// error must end the Reconcile pass in error, never fall through as though
// no decision were open.
//
// Closing the whole store (the first version of this test) does not isolate
// that claim: r.load's own Store.Run/Events calls fail first, so Reconcile
// errors regardless of whether the new decision-read guard does anything at
// all. Run and Events are kept perfectly readable here; only the decision
// document itself - corrupted directly in the database, the same technique
// TestEventsPageDoesNotDecodeUnboundedTail already uses for a journal row -
// is unreadable, isolating the guard this test actually means to prove.
func TestAnUnreadableDecisionStoreNeverAuthorizesDispatch(t *testing.T) {
	// Precondition, proved on an otherwise-identical, UNCORRUPTED run: this
	// run shape genuinely has an operation state.plan() would select on its
	// first pass - the corrupted case below is refusing real work, not
	// coincidentally testing a run with nothing to do anyway.
	baseline, _, baselineRunID := newLinearizabilityFixture(t)
	baselineEngine, err := baseline.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baselineEngine.Reconcile(context.Background(), baselineRunID); err != nil {
		t.Fatal(err)
	}
	baselineOps, err := baseline.store.Operations(baselineRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(baselineOps) == 0 {
		t.Fatal("test precondition violated: an uncorrupted run's first Reconcile pass created no operation at all")
	}

	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, fixture.clock.Now())
	if _, err := store.db.Exec(`UPDATE orchestration_messages SET document = '{}' WHERE id = ?`, d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Run(runID); err != nil || !found {
		t.Fatalf("precondition: Run must still read fine despite the corrupt message: found=%t err=%v", found, err)
	}
	if _, err := store.Events(runID); err != nil {
		t.Fatalf("precondition: Events must still read fine despite the corrupt message: %v", err)
	}

	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(context.Background(), runID); err == nil {
		t.Fatal("Reconcile must fail, not silently proceed, when its own decision read is corrupt")
	}
	ops, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Fatalf("an operation was dispatched despite the corrupt decision read: %+v", ops)
	}
}
