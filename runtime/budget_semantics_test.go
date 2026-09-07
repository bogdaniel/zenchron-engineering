package runtime

// The execution wall budget bounds the time this system spends WORKING. These
// tests pin the distinction that a live run exposed: a pull request awaiting
// human review was being killed by an engineering budget, which made the #63
// review loop unusable at any budget that still bounded runaway work.

import (
	"encoding/json"
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
	events := []EngineeringEvent{}
	at := start
	// Ten ticks, each: a long idle gap, then a second of observation.
	for i := 0; i < 10; i++ {
		events = append(events, waitEvent(at, "goal_state_reached"))
		at = at.Add(10 * time.Minute)
		events = append(events, progressEvent(at))
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
