package runtime

// The execution wall budget bounds the time this system spends WORKING. These
// tests pin the distinction that a live run exposed: a pull request awaiting
// human review was being killed by an engineering budget, which made the #63
// review loop unusable at any budget that still bounded runaway work.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func waitEvent(at time.Time, reason string) EngineeringEvent {
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	return EngineeringEvent{Type: EventRunWaiting, OccurredAt: at, Payload: payload}
}

func progressEvent(at time.Time) EngineeringEvent {
	return EngineeringEvent{Type: EventOperationAfter, OccurredAt: at}
}

// operationPair is what the runtime actually writes for one operation: a before
// and an after, sharing an operation id. Work performed during a wait is
// measured between them, so a fixture that emits a lone `after` models nothing
// and would report every poll as instantaneous.
func operationPair(id string, start time.Time, took time.Duration) []EngineeringEvent {
	return []EngineeringEvent{
		{Type: EventOperationBefore, OperationID: id, OccurredAt: start},
		{Type: EventOperationAfter, OperationID: id, OccurredAt: start.Add(took)},
	}
}

// TestWaitingOnAHumanDoesNotSpendTheExecutionBudget is the semantic fix. A run
// that reached its goal and sat awaiting review has done no work in the
// meantime, and an execution budget that counts that time forces an operator to
// size their engineering bounds around how fast people answer.
func TestWaitingOnAHumanDoesNotSpendTheExecutionBudget(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	state := &runState{
		run: EngineeringRun{CreatedAt: start},
		events: []EngineeringEvent{
			progressEvent(start.Add(2 * time.Minute)),                 // real work
			waitEvent(start.Add(3*time.Minute), "goal_state_reached"), // then the human's turn
		},
	}
	// Twelve hours later the human still has not reviewed it.
	now := start.Add(12 * time.Hour)
	active := state.activeElapsed(now)
	if active > 5*time.Minute {
		t.Fatalf("an overnight wait for review spent %s of the execution budget", active)
	}
	// The work before the wait is still counted: this is not "waiting runs are
	// free", it is "waiting is not work".
	if active < 3*time.Minute {
		t.Fatalf("the work performed before the wait was not counted: %s", active)
	}
}

// TestAnInternalWaitStillSpendsTheBudget keeps the exemption honest. The set of
// waits that pause the clock is closed and fail-closed: a reason nobody has
// classified is the system's own problem and burns the budget, so a new wait
// cannot make a run immortal by accident.
func TestAnInternalWaitStillSpendsTheBudget(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"operation_unavailable", "controller_changed", "invented_reason"} {
		state := &runState{
			run:    EngineeringRun{CreatedAt: start},
			events: []EngineeringEvent{waitEvent(start.Add(time.Minute), reason)},
		}
		now := start.Add(2 * time.Hour)
		if active := state.activeElapsed(now); active < 2*time.Hour-time.Second {
			t.Fatalf("wait reason %q paused the execution budget; only classified external waits may: %s", reason, active)
		}
	}
}

// TestObservationDuringAWaitIsStillWork stops the exemption from swallowing the
// polling a waiting run performs. The runtime re-reads the pull request and the
// issue on every tick; that is real work and is counted. Only the idle gap
// between ticks is excluded.
func TestObservationDuringAWaitIsStillWork(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	events := []EngineeringEvent{waitEvent(start, "goal_state_reached")}
	at := start
	// Ten ticks, each: a long idle gap, then a second of observation. The wait
	// event is written ONCE, because the reason never changes - which is what
	// production does.
	for i := 0; i < 10; i++ {
		at = at.Add(10 * time.Minute)
		events = append(events, operationPair(fmt.Sprintf("poll-%d", i), at, time.Second)...)
		at = at.Add(time.Second)
	}
	state := &runState{run: EngineeringRun{CreatedAt: start}, events: events}
	active := state.activeElapsed(at)
	if active > time.Minute {
		t.Fatalf("idle time between ticks was counted as work: %s", active)
	}
	if active < 9*time.Second {
		t.Fatalf("observation performed while waiting was not counted as work: %s", active)
	}
}

// TestActiveAccountingIsDerivedFromDurableEventsOnly is the restart law. Every
// excluded interval is bounded by two recorded events, so a second process
// replaying the same rows reaches the same number. An in-memory stopwatch would
// reset on restart and hand the run a fresh budget.
func TestActiveAccountingIsDerivedFromDurableEventsOnly(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	events := []EngineeringEvent{
		progressEvent(start.Add(time.Minute)),
		waitEvent(start.Add(2*time.Minute), "awaiting_authority"),
		progressEvent(start.Add(90 * time.Minute)),
	}
	now := start.Add(100 * time.Minute)
	first := (&runState{run: EngineeringRun{CreatedAt: start}, events: events}).activeElapsed(now)
	// A "restarted" reader: same rows, new object, nothing carried over.
	second := (&runState{run: EngineeringRun{CreatedAt: start}, events: append([]EngineeringEvent(nil), events...)}).activeElapsed(now)
	if first != second {
		t.Fatalf("the same journal produced two answers across a restart: %s then %s", first, second)
	}
	// 88 minutes of that span was somebody else's turn.
	if first > 15*time.Minute {
		t.Fatalf("the authority wait was charged to the execution budget: %s", first)
	}
}

// conditionsFixture builds the smallest runState the disposition rules read.
// It exists so the tests below exercise conditions() itself rather than the
// helper it calls: an earlier version of this file asserted activeElapsed in
// isolation and passed with the budget gate reverted to raw elapsed time, which
// is a test of arithmetic rather than of the rule.
func conditionsFixture(created time.Time, clock Clock, budgets RunBudgets, events []EngineeringEvent) *runState {
	return &runState{
		rt:     &EngineeringRuntime{deps: Dependencies{Clock: clock, Budgets: budgets}},
		run:    EngineeringRun{CreatedAt: created},
		events: events,
	}
}

// TestTheBudgetGateSpendsActiveTimeNotCalendarTime binds the rule to the
// accounting. A run awaiting review well past its wall limit must not be failed,
// and a run that spent the same span WORKING must be.
func TestTheBudgetGateSpendsActiveTimeNotCalendarTime(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	budgets := RunBudgets{WallLimit: 30 * time.Minute}
	clock := &steppingClock{at: start.Add(12 * time.Hour)}

	awaitingReview := conditionsFixture(start, clock, budgets, []EngineeringEvent{
		progressEvent(start.Add(2 * time.Minute)),
		waitEvent(start.Add(3*time.Minute), "goal_state_reached"),
	})
	if disposition, reason := awaitingReview.conditions(); disposition == Failed {
		t.Fatalf("a run awaiting human review was failed by the execution budget: %s/%s", disposition, reason)
	}

	// The same twelve hours, spent working: the bound still ends it.
	working := conditionsFixture(start, clock, budgets, []EngineeringEvent{
		progressEvent(start.Add(time.Hour)),
	})
	disposition, reason := working.conditions()
	if disposition != Failed || reason != "run_wall_budget_exhausted" {
		t.Fatalf("a run that worked past its wall budget was not stopped: %s/%s", disposition, reason)
	}
}

// TestTheLifecycleDeadlineIsSeparateAndOptional. Bounding total calendar time is
// a legitimate thing to want; it is just not what an execution budget means. It
// is absent by default, so a run waiting on a person waits as long as the person
// takes.
func TestTheLifecycleDeadlineIsSeparateAndOptional(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := &steppingClock{at: start.Add(48 * time.Hour)}
	waiting := []EngineeringEvent{waitEvent(start.Add(time.Minute), "goal_state_reached")}

	unbounded := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute}, waiting)
	if disposition, reason := unbounded.conditions(); disposition == Failed {
		t.Fatalf("with no lifecycle deadline configured a waiting run was still ended: %s/%s", disposition, reason)
	}

	bounded := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute, LifecycleDeadline: 24 * time.Hour}, waiting)
	disposition, reason := bounded.conditions()
	if disposition != Failed || reason != "run_lifecycle_deadline_exhausted" {
		t.Fatalf("a stated lifecycle deadline did not bound total elapsed time: %s/%s", disposition, reason)
	}
}

// TestAWaitSurvivesThePollsTakenDuringIt is the PRODUCTION-PATH regression, and
// it is the one that matters.
//
// recordDisposition appends run.waiting only when the disposition or reason
// CHANGES, so a run that stays in the same wait writes exactly one wait event
// and then keeps emitting operation events as it polls. An accounting model
// that closes the wait at the next event therefore closes it at the first poll
// and charges every hour after that to the execution budget - the original
// defect, returning after one tick.
//
// The earlier unit tests could not see this: they synthesized a fresh
// run.waiting before each observation, which is a journal production never
// writes. This one drives the real reconcile loop and lets the runtime write
// its own journal.
func TestAWaitSurvivesThePollsTakenDuringIt(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	// Drive until the run parks in an external wait, exactly as it would live.
	var reason string
	for pass := 0; pass < 12; pass++ {
		outcome := fixture.reconcile(runID)
		reason = outcome.Reason
		if outcome.Disposition == Waiting && externalWaitReasons[reason] {
			break
		}
		if terminalDisposition(outcome.Disposition) {
			t.Fatalf("the run finished without ever waiting on anything external: %s/%s", outcome.Disposition, reason)
		}
	}
	if !externalWaitReasons[reason] {
		t.Skipf("this fixture never reached an external wait (last reason %q); the unit tests cover the accounting", reason)
	}

	state, err := fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	before := state.activeElapsed(fixture.clock.Now())

	// Hours pass, the runtime polls, the wait reason does not change - so no
	// second run.waiting is written. This is the exact shape production emits.
	waits := countWaitEvents(t, fixture, runID)
	for tick := 0; tick < 3; tick++ {
		fixture.clock.advance(4 * time.Hour)
		fixture.reconcile(runID)
	}
	if after := countWaitEvents(t, fixture, runID); after != waits {
		t.Fatalf("the fixture wrote %d extra run.waiting events, so it is not reproducing the deduplicated journal", after-waits)
	}

	state, err = fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	grew := state.activeElapsed(fixture.clock.Now()) - before
	if grew > 5*time.Minute {
		t.Fatalf("twelve hours of waiting spent %s of the execution budget across polling ticks", grew)
	}
}

// countWaitEvents is how the test proves it is reproducing the deduplicated
// journal rather than a convenient one.
func countWaitEvents(t *testing.T, fixture *phase8Fixture, runID string) int {
	t.Helper()
	state, err := fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range state.events {
		if event.Type == EventRunWaiting {
			n++
		}
	}
	return n
}

// TestEveryWaitRoutedFailureIsClassified closes the gap between the two tables.
// waitReasons names the durable reason each wait-routed failure settles into;
// every one of them is the run waiting on something OUTSIDE itself - a provider
// account, a quota, a rate limit, a dependency the operator must provision,
// disk the operator must free. A reason present in one table and missing from
// the other silently charges the operator for somebody else's backoff.
func TestEveryWaitRoutedFailureIsClassified(t *testing.T) {
	for class, reason := range waitReasons {
		if !externalWaitReasons[reason] {
			t.Errorf("failure class %q settles into wait reason %q, which spends the execution budget", class, reason)
		}
	}
}

// Exercise the historical operation payload, not just the scheduler row: the
// journal is written before Finish clears ActiveSince in the operation store.
func TestParkedObservationStatusSurvivesRestart(t *testing.T) {
	f := newPhase8Fixture(t)
	id := f.start()
	for i := 0; i < 12; i++ {
		out := f.reconcile(id)
		if out.Disposition == Waiting && out.Reason == ReasonGoalStateReached {
			break
		}
	}
	read := func() StatusReport {
		t.Helper()
		s, err := f.runtime.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := read()
	observations := func() map[string]time.Duration {
		t.Helper()
		state, err := f.runtime.load(id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]time.Duration{}
		for id, op := range state.snapshot.Operations {
			if op.Kind == OpGitHubObserve && op.State == Succeeded {
				out[id] = statusOperationElapsed(op, state.events, f.clock.Now())
			}
		}
		return out
	}
	observedBefore := observations()
	if len(observedBefore) == 0 {
		t.Fatal("no succeeded GitHub observation")
	}
	if before.Reason != ReasonGoalStateReached || before.PullRequest == nil || before.Operation == nil || before.Operation.State != Succeeded {
		t.Fatalf("fixture did not park after observing its PR: %+v operation=%+v", before, before.Operation)
	}
	if before.ActiveElapsed <= 0 || before.Operation.Elapsed <= 0 {
		t.Fatal("fixture must perform measurable work")
	}
	f.clock.advance(8 * time.Hour)
	after := read()
	if after.ActiveElapsed != before.ActiveElapsed || after.Operation.Elapsed != before.Operation.Elapsed {
		t.Fatalf("parked wait changed active consumption: before=%+v/%+v after=%+v/%+v", before.ActiveElapsed, before.Operation, after.ActiveElapsed, after.Operation)
	}
	if after.ExternalWaitElapsed <= before.ExternalWaitElapsed || after.Elapsed <= before.Elapsed {
		t.Fatal("wait and lifecycle age did not advance")
	}
	reopen(t, f)
	replayed := read()
	observedAfter := observations()
	for id, elapsed := range observedBefore {
		if observedAfter[id] != elapsed {
			t.Fatalf("replayed observation changed duration: %s", id)
		}
	}
	if replayed.ActiveElapsed != before.ActiveElapsed || replayed.Operation.Elapsed != before.Operation.Elapsed {
		t.Fatal("restart changed consumed time")
	}
	f.reconcile(id)
	polled := read()
	state, err := f.runtime.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if polled.ActiveElapsed != state.activeElapsed(polled.Now) {
		t.Fatal("status differs from budget truth")
	}
	if polled.ActiveElapsed-before.ActiveElapsed > time.Minute {
		t.Fatal("poll charged idle wait")
	}
}

func TestObservationStatusCountsOnlyBoundedWork(t *testing.T) {
	start := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	events := []EngineeringEvent{waitEvent(start.Add(time.Minute), ReasonGoalStateReached)}
	for i := 1; i <= 2; i++ {
		at := start.Add(time.Duration(i) * 4 * time.Hour)
		events = append(events, operationPair("observe", at, 3*time.Second)...)
		op := RunOperation{ID: "observe", Kind: OpGitHubObserve, State: Succeeded, StartedAt: &at, ActiveSince: &at, ConsumedExecution: time.Duration(i-1) * 3 * time.Second}
		now := at.Add(2 * time.Hour)
		if got := statusOperationElapsed(op, events, now); got != time.Duration(i)*3*time.Second {
			t.Fatalf("observation duration=%s", got)
		}
		run := EngineeringRun{CreatedAt: start}
		want := time.Minute + time.Duration(i)*3*time.Second
		if got := ActiveElapsed(run, events, now); got != want {
			t.Fatalf("active=%s want=%s", got, want)
		}
		data, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		var replay []EngineeringEvent
		if err := json.Unmarshal(data, &replay); err != nil {
			t.Fatal(err)
		}
		if got := ActiveElapsed(run, replay, now.Add(8*time.Hour)); got != want {
			t.Fatalf("replayed active=%s want=%s", got, want)
		}
	}
}

func TestOperationElapsedRetainsFinishedConsumption(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, time.Minute)
	clock.advance(3 * time.Second)
	if got := OperationElapsed(op, clock.Now()); got != 3*time.Second {
		t.Fatalf("running elapsed=%s", got)
	}
	finished, err := scheduler.Finish(op.ID, Succeeded)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(8 * time.Hour)
	if got := OperationElapsed(finished, clock.Now()); got != 3*time.Second {
		t.Fatalf("finished elapsed=%s", got)
	}
}

func TestAcceptedReviewBudgetExhaustionUsesPersistedContinuation(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, consumed := range []bool{false, true} {
		t.Run(fmt.Sprint("consumed=", consumed), func(t *testing.T) {
			observed, _ := json.Marshal(FeedbackObservedPayload{FeedbackDecision: FeedbackDecision{Key: "review:123", Admitted: true}})
			events := []EngineeringEvent{{Type: EventFeedbackObserved, OccurredAt: start.Add(time.Minute), Payload: observed}}
			if consumed {
				payload, _ := json.Marshal(FeedbackConsumedPayload{Keys: []string{"review:123"}})
				events = append(events, EngineeringEvent{Type: EventFeedbackConsumed, OccurredAt: start.Add(2 * time.Minute), Payload: payload})
			}
			clock := &steppingClock{at: start.Add(35 * time.Minute)}
			state := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute}, events)
			if disposition, reason := state.conditions(); disposition != Waiting || reason != ReasonReviewBudgetExhausted {
				t.Fatalf("accepted review abandoned: %s/%s", disposition, reason)
			}
			state.events = append(state.events, waitEvent(clock.at, ReasonReviewBudgetExhausted))
			clock.at = start.Add(24 * time.Hour)
			state = conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute}, state.events)
			if state.activeElapsed(clock.at) != 35*time.Minute {
				t.Fatal("budget wait spent idle time")
			}
			if state.feedbackState().Consumed["review:123"] != consumed {
				t.Fatal("budget wait changed delivery identity")
			}
			state.run.Budgets = &RunBudgets{WallLimit: 30 * time.Minute}
			state.rt.deps.Budgets.WallLimit = time.Hour
			if _, reason := state.conditions(); reason != ReasonReviewBudgetExhausted {
				t.Fatal("live configuration widened the persisted budget")
			}
			grant, _ := json.Marshal(ReviewContinuationGrant{ActiveBaseline: 35 * time.Minute, Allowance: 30 * time.Minute, FeedbackDigest: "digest"})
			state.events = append(state.events, EngineeringEvent{Type: EventReviewContinuationGranted, OccurredAt: clock.at, Payload: grant})
			if _, reason := state.conditions(); reason == ReasonReviewBudgetExhausted {
				t.Fatal("durable continuation did not release the wait")
			}
		})
	}
}
