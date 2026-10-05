package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// secondStoreHandle is another process's view of the fixture's database.
func secondStoreHandle(t *testing.T, f *phase8Fixture) *SQLiteOperationStore {
	t.Helper()
	other, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	return other
}

func eventsOf(t *testing.T, store *SQLiteOperationStore, runID string) []EngineeringEvent {
	t.Helper()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// afterPause is every event journalled after the run's latest run.paused.
func afterPause(t *testing.T, events []EngineeringEvent) []EngineeringEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventRunPaused {
			return events[i+1:]
		}
	}
	t.Fatal("no run.paused in the journal")
	return nil
}

// pauseAtAcquisition lands a pause, written through a SECOND database handle,
// after the driver passed Reconcile's pause check and immediately before its
// durable acquisition of the execution operation.
type pauseAtAcquisition struct {
	OperationStore
	t     *testing.T
	other *SQLiteOperationStore
	f     *phase8Fixture
	runID string
	fired bool
}

func (p *pauseAtAcquisition) AcquireOperation(op RunOperation, expected int64, maxRuns, maxObservations, maxVerifications int) (int64, bool, error) {
	if !p.fired && op.Kind == OpExecutionInvoke {
		p.fired = true
		if _, err := PauseRun(p.other, p.f.clock.Now(), p.runID, "investigating", "bogdan"); err != nil {
			p.t.Fatal(err)
		}
	}
	return p.OperationStore.AcquireOperation(op, expected, maxRuns, maxObservations, maxVerifications)
}

// TestAPauseAtAcquisitionIsRefusedByTheStore is the decisive case (#86): the
// store, not Reconcile, is the gate. Mutation: deleting the runPausedSQL
// conjunct from AcquireOperation lets the provider run.
func TestAPauseAtAcquisitionIsRefusedByTheStore(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	gate := &pauseAtAcquisition{OperationStore: f.runtime.scheduler.Store, t: t, other: secondStoreHandle(t, f), f: f, runID: runID}
	f.runtime.scheduler.Store = gate
	var outcome Outcome
	for pass := 0; pass < 8 && !gate.fired; pass++ {
		outcome = f.reconcile(runID)
	}
	if !gate.fired {
		t.Fatal("the execution operation was never acquired, so nothing was contested")
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("the provider was invoked %d time(s) on a paused run", len(f.provider.requests))
	}
	if tail := afterPause(t, eventsOf(t, f.store, runID)); len(tail) != 0 {
		t.Fatalf("the refused pass journalled %d event(s) after run.paused: %s", len(tail), tail[0].Type)
	}
	if outcome.Disposition == Cancelled || outcome.Reason == "operation_unavailable" {
		t.Fatalf("the pause changed the run's outcome: %+v", outcome)
	}
	operations, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if op.Lease != nil {
			t.Fatalf("operation %q holds a lease on a paused run", op.ID)
		}
	}
}

// TestAnIdlePausedRunJournalsNothing: passes over a paused run plan, settle and
// journal nothing, and status counts it Paused. Mutation: deleting Reconcile's
// pause exit journals operation.planned.
func TestAnIdlePausedRunJournalsNothing(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	before := len(eventsOf(t, f.store, runID))
	if _, err := PauseRun(f.store, f.clock.Now(), runID, "idle", "bogdan"); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		f.reconcile(runID)
	}
	if after := eventsOf(t, f.store, runID); len(after) != before+1 {
		t.Fatalf("passes over a paused run journalled %d event(s) beyond run.paused", len(after)-before-1)
	}
	if len(f.provider.requests) != 0 {
		t.Fatal("a paused run reached the provider")
	}
}

// TestPausedCountsPartitionActive: the six counts still sum to Active, a
// paused idle run is Paused (not Runnable or Waiting), and an unpaused one is
// not. Mutation: deleting the Paused case in fleetStatus.
func TestPausedCountsPartitionActive(t *testing.T) {
	f := newPhase8Fixture(t)
	paused := f.start()
	if _, err := PauseRun(f.store, f.clock.Now(), paused, "count me", "bogdan"); err != nil {
		t.Fatal(err)
	}
	fleet, err := FleetStatus(f.store, f.stateDir, 1, 2, 2, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	sum := fleet.Working + fleet.Observing + fleet.Runnable + fleet.Waiting + fleet.Paused + fleet.Unavailable
	if fleet.Active != 1 || sum != fleet.Active || fleet.Paused != 1 {
		t.Fatalf("paused run miscounted: %+v", fleet)
	}
	if fleet.Runs[0].Paused == nil || fleet.Runs[0].Paused.Operator != "bogdan" {
		t.Fatalf("the run summary does not show the pause: %+v", fleet.Runs[0])
	}
	if _, err := UnpauseRun(f.store, f.clock.Now(), paused, "bogdan"); err != nil {
		t.Fatal(err)
	}
	fleet, err = FleetStatus(f.store, f.stateDir, 1, 2, 2, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum := fleet.Working + fleet.Observing + fleet.Runnable + fleet.Waiting + fleet.Paused + fleet.Unavailable; fleet.Paused != 0 || sum != fleet.Active {
		t.Fatalf("an unpaused run miscounted: %+v", fleet)
	}
}

// TestAPauseLetsTheInFlightAttemptFinish: an execution already leased finishes
// and is journalled; nothing after it is acquired; unpause continues the same
// run. Nothing is cancelled and the disposition is untouched.
func TestAPauseLetsTheInFlightAttemptFinish(t *testing.T) {
	f := newPhase8Fixture(t)
	other := secondStoreHandle(t, f)
	var runID string
	paused := false
	f.provider.mutate = func(dir string) error {
		if !paused {
			paused = true
			if _, err := PauseRun(other, f.clock.Now(), runID, "mid-flight", "bogdan"); err != nil {
				return err
			}
		}
		return nil
	}
	runID = f.start()
	for pass := 0; pass < 8 && !paused; pass++ {
		f.reconcile(runID)
	}
	if !paused {
		t.Fatal("the provider never ran")
	}
	events := eventsOf(t, f.store, runID)
	tail := afterPause(t, events)
	invoke := ""
	for _, e := range events {
		if e.Type == EventOperationBefore {
			invoke = e.OperationID
		}
	}
	finished := false
	for _, e := range tail {
		if e.OperationID != invoke {
			t.Fatalf("a paused run journalled %s for %q, beyond the in-flight %q", e.Type, e.OperationID, invoke)
		}
		finished = finished || e.Type == EventOperationAfter
		if e.Type == EventRunCancelled || e.Type == EventRunWaiting || e.Type == EventRunFailed {
			t.Fatalf("pause journalled %s", e.Type)
		}
	}
	if !finished {
		t.Fatal("the in-flight attempt's result was not journalled")
	}
	op, _, found, err := f.store.Operation(invoke)
	if err != nil || !found || op.State != Succeeded {
		t.Fatalf("the in-flight operation did not settle: %+v %v", op, err)
	}
	// More passes while paused change nothing.
	f.reconcile(runID)
	if again := eventsOf(t, f.store, runID); len(again) != len(events) {
		t.Fatal("a pass over the paused run journalled")
	}
	if _, err := UnpauseRun(f.store, f.clock.Now(), runID, "bogdan"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(runID)
	resumed := afterPause(t, eventsOf(t, f.store, runID))
	progressed := false
	for _, e := range resumed {
		// operation.before, not any event: operation.planned precedes the
		// acquisition, so it would count a refused lease as progress.
		progressed = progressed || (e.Type == EventOperationBefore && e.OperationID != invoke)
	}
	if !progressed {
		t.Fatal("unpause did not let the run continue")
	}
	if len(f.provider.requests) != 1 {
		t.Fatalf("the provider ran %d times; the in-flight attempt must not be repeated", len(f.provider.requests))
	}
}

// TestAPauseSurvivesARestart: a new store handle and a new runtime still see
// the pause and are still refused at acquisition.
func TestAPauseSurvivesARestart(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	if _, err := PauseRun(f.store, f.clock.Now(), runID, "overnight", "bogdan"); err != nil {
		t.Fatal(err)
	}
	reopened := secondStoreHandle(t, f)
	deps := f.deps
	deps.Store = reopened
	restarted := f.newRuntime(deps)
	before := len(eventsOf(t, reopened, runID))
	if _, err := restarted.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if after := len(eventsOf(t, reopened, runID)); after != before {
		t.Fatalf("a restarted driver journalled %d event(s) on a paused run", after-before)
	}
	view, err := PauseStatus(reopened, runID)
	if err != nil || !view.Paused || view.Reason != "overnight" {
		t.Fatalf("the pause did not survive: %+v %v", view, err)
	}
	if _, _, err := restarted.scheduler.Plan(RunOperation{RunID: runID, Kind: OpExecutionInvoke, IdempotencyKey: "restart-probe", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	if leased, err := restarted.scheduler.Next(runID); err != nil || leased != nil {
		t.Fatalf("a restarted scheduler leased %v on a paused run (%v)", leased, err)
	}
}

// TestPauseAndUnpauseAreIdempotent: each appends at most one event, and a
// repeated pause reports the original.
func TestPauseAndUnpauseAreIdempotent(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	count := func(eventType string) int {
		n := 0
		for _, e := range eventsOf(t, f.store, runID) {
			if e.Type == eventType {
				n++
			}
		}
		return n
	}
	if _, err := UnpauseRun(f.store, f.clock.Now(), runID, "bogdan"); err != nil || count(EventRunUnpaused) != 0 {
		t.Fatalf("unpausing an unpaused run appended (%v)", err)
	}
	first, err := PauseRun(f.store, f.clock.Now(), runID, "first", "bogdan")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PauseRun(f.store, f.clock.Now(), runID, "second", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if count(EventRunPaused) != 1 || second.Reason != "first" || second.Operator != "bogdan" || !second.Since.Equal(first.Since) {
		t.Fatalf("a repeated pause appended or replaced the original: %+v", second)
	}
	for range 2 {
		if _, err := UnpauseRun(f.store, f.clock.Now(), runID, "bogdan"); err != nil {
			t.Fatal(err)
		}
	}
	if count(EventRunUnpaused) != 1 {
		t.Fatalf("a repeated unpause appended %d events", count(EventRunUnpaused))
	}
	run, _, err := f.store.Run(runID)
	if err != nil || run.Disposition == Cancelled {
		t.Fatalf("pause moved the disposition: %+v %v", run, err)
	}
}

// TestStopWinsOverPause: a paused run can be stopped, and a stopped run can be
// neither paused nor unpaused.
func TestStopWinsOverPause(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	if _, err := PauseRun(f.store, f.clock.Now(), runID, "hold on", "bogdan"); err != nil {
		t.Fatal(err)
	}
	outcome, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator_stop")
	if err != nil || outcome.Disposition != Cancelled {
		t.Fatalf("stop on a paused run: %+v %v", outcome, err)
	}
	events := eventsOf(t, f.store, runID)
	if events[len(events)-1].Type != EventRunCancelled {
		t.Fatal("stop did not journal run.cancelled")
	}
	for _, change := range []func() error{
		func() error { _, err := UnpauseRun(f.store, f.clock.Now(), runID, "bogdan"); return err },
		func() error { _, err := PauseRun(f.store, f.clock.Now(), runID, "again", "bogdan"); return err },
	} {
		if err := change(); !IsRunTerminal(err) || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("a stopped run accepted a pause change: %v", err)
		}
	}
	if len(eventsOf(t, f.store, runID)) != len(events) {
		t.Fatal("a refused pause change was journalled")
	}
}

// TestANeverPausedSnapshotDigestIsUnchanged: the paused member is omitted when
// empty, so every existing run replays to the digest it always had.
func TestANeverPausedSnapshotDigestIsUnchanged(t *testing.T) {
	snapshot, err := Reduce(newJournalRun("r"), nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), `"paused"`) {
		t.Fatalf("a never-paused snapshot carries a paused member: %s", canonical)
	}
}

// TestSQLAndReplayAgreeOnTheLatestPauseEvent pins "the latest of run.paused
// and run.unpaused wins" in BOTH readers - the acquisition predicate and
// replay - over every prefix of an alternating history, so neither can drift
// into a permanent wedge where unpause never re-admits a lease.
func TestSQLAndReplayAgreeOnTheLatestPauseEvent(t *testing.T) {
	_, store, _ := openPair(t)
	run := newJournalRun("run-a")
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	for i, want := range []bool{false, true, false, true, false, true, false} {
		if i > 0 {
			now = now.Add(time.Second)
			var err error
			if want {
				_, err = PauseRun(store, now, run.ID, "step", "bogdan")
			} else {
				_, err = UnpauseRun(store, now, run.ID, "bogdan")
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		sqlPaused, err := store.RunPaused(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		replayed := JournalPause(eventsOf(t, store, run.ID)) != nil
		if sqlPaused != want || replayed != want {
			t.Fatalf("step %d: want paused=%v, SQL says %v, replay says %v", i, want, sqlPaused, replayed)
		}
	}
}

// TestASettlingPausedRunCountsAsItsActiveClass: a run paused while it holds a
// lease is still consuming that slot, so it counts Working or Observing, not
// Paused, and the counts still partition Active.
func TestASettlingPausedRunCountsAsItsActiveClass(t *testing.T) {
	for _, c := range []struct {
		kind string
		got  func(Fleet) int
	}{
		{"k", func(f Fleet) int { return f.Working }},
		{OpSourceObserve, func(f Fleet) int { return f.Observing }},
	} {
		t.Run(c.kind, func(t *testing.T) {
			dir, store, _ := openPair(t)
			run := newJournalRun("run-a")
			if err := store.PutRun(run); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(100, 0).UTC()
			s := Scheduler{Store: store, Clock: &fakeClock{now: now}, Owner: "driver", LeaseDuration: time.Minute,
				Liveness: alwaysAlive(), MaxConcurrentRuns: 2, MaxConcurrentObservations: 2}
			if _, _, err := s.Plan(RunOperation{RunID: run.ID, Kind: c.kind, IdempotencyKey: "settling", MaxAttempts: 2}); err != nil {
				t.Fatal(err)
			}
			if leased, err := s.Next(run.ID); err != nil || leased == nil {
				t.Fatalf("lease before the pause: %v %v", leased, err)
			}
			view, err := PauseRun(store, now, run.ID, "settle", "bogdan")
			if err != nil || view.Settling == nil {
				t.Fatalf("the pause does not report the settling operation: %+v %v", view, err)
			}
			fleet, err := FleetStatus(store, dir, 2, 2, 2, now)
			if err != nil {
				t.Fatal(err)
			}
			sum := fleet.Working + fleet.Observing + fleet.Runnable + fleet.Waiting + fleet.Paused + fleet.Unavailable
			if c.got(fleet) != 1 || fleet.Paused != 0 || fleet.Active != 1 || sum != fleet.Active {
				t.Fatalf("a settling paused %s run miscounted: %+v", c.kind, fleet)
			}
		})
	}
}
