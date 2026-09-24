package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stopAtAcquisition forces the ONE interleaving that matters, deterministically
// rather than by timing: the operator's stop commits in full - the run document
// is written and its operations are scanned - and only then does the driver's
// durable acquisition statement run.
//
// That ordering is not contrived. CancelRun is reached from the control
// endpoint's stop-all on its own goroutine while Supervisor.Tick is driving
// runs on others, and Supervisor.mu serializes neither. A driver that read its
// run before the stop is inside a pass whose every decision is already stale.
type stopAtAcquisition struct {
	OperationStore
	t       *testing.T
	fixture *phase8Fixture
	runID   string
	fired   bool
}

func (s *stopAtAcquisition) AcquireOperation(op RunOperation, expected int64, maxRuns int) (int64, bool, error) {
	if !s.fired && op.Kind == OpExecutionInvoke {
		s.fired = true
		if _, err := CancelRun(s.fixture.store, s.fixture.runtime.scheduler, s.fixture.clock.Now(), s.runID, "operator/stop"); err != nil {
			s.t.Fatal(err)
		}
		run, found, err := s.fixture.store.Run(s.runID)
		if err != nil || !found {
			s.t.Fatal(err, found)
		}
		if run.Disposition != Cancelled {
			s.t.Fatalf("the stop did not commit before the acquisition: %q", run.Disposition)
		}
	}
	return s.OperationStore.AcquireOperation(op, expected, maxRuns)
}

// TestAStopAtAcquisitionNeverExecutesTheWorkItStopped is the cancel-versus-
// acquire proof. A run is stopped after its driver has read it and before that
// driver leases the execution operation, and the provider must not be invoked -
// not on that pass, and not on any pass after it.
//
// The name says ACQUISITION deliberately. A stop landing one durable write
// later - after Start - is a different window and is NOT covered: nothing on
// the executing path re-reads the run or the cancellation flag, so that attempt
// finishes. It reproduces identically on main, it is the mid-flight drain case,
// and closing it needs cooperative cancellation rather than a durable condition.
//
// It fails three different ways without the three parts of the repair, which is
// why all three are asserted here rather than in separate tests:
//
//   - with the acquisition statement unguarded, the provider is invoked on THIS
//     pass: nothing between Next and handle re-reads the run;
//   - with only the acquisition guarded, the pass settles on its stale view,
//     appends run.waiting after run.cancelled, writes the run document back to
//     waiting, and the provider is invoked on the NEXT pass;
//   - with only the acquisition guarded and the settle reporting what it was
//     asked for, the operator is told the run is waiting when it is stopped.
func TestAStopAtAcquisitionNeverExecutesTheWorkItStopped(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	stop := &stopAtAcquisition{OperationStore: f.runtime.scheduler.Store, t: t, fixture: f, runID: runID}
	f.runtime.scheduler.Store = stop

	// Passes before the execution operation are ordinary work; the stop arms
	// itself on the operation that actually reaches the provider.
	var outcome Outcome
	for pass := 0; pass < 8 && !stop.fired; pass++ {
		var err error
		outcome, err = f.runtime.Reconcile(context.Background(), runID)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if !stop.fired {
		t.Fatal("the execution operation was never acquired, so nothing was contested")
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("the execution provider was invoked %d time(s) on a run the operator had already stopped", len(f.provider.requests))
	}
	if outcome.Disposition != Cancelled {
		t.Fatalf("the pass reported %q %q over a stopped run", outcome.Disposition, outcome.Reason)
	}
	stopped, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if stopped.Disposition != Cancelled {
		t.Fatalf("the pass settled a stopped run back to %q, returning it to the supervisor's active set", stopped.Disposition)
	}
	// The supervisor would drive this run again on the next tick if anything
	// above had un-stopped it, so the next pass is part of the assertion.
	if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.requests) != 0 {
		t.Fatalf("a later pass invoked the execution provider %d time(s) on a stopped run", len(f.provider.requests))
	}
	// Nothing the repair does may cost the run its slot back: #171's whole
	// point is that a stopped run holds nothing.
	operations, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if op.Lease != nil {
			t.Fatalf("operation %q still holds a lease on a stopped run", op.ID)
		}
	}
}

// TestSQLiteAStoppedRunsOperationIsNeverAcquired states the durable rule on its
// own, across two database handles, because the statement is where the rule has
// to live: the run document and the acquisition are written by different
// processes, and only SQLite serializes them.
//
// The operation is merely PLANNED when the stop lands, which is exactly the case
// CancelRun's own loop cannot answer - it finishes what is leased or running,
// and a pending operation is neither. A driver reaching Next afterwards must be
// refused by the acquisition itself.
func TestSQLiteAStoppedRunsOperationIsNeverAcquired(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	if err := storeA.PutRun(newJournalRun("run-a")); err != nil {
		t.Fatal(err)
	}
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 2}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 2}
	planned := planFor(t, one, "run-a", 2)
	if _, err := CancelRun(storeA, one, c.Now(), "run-a", "operator/stop"); err != nil {
		t.Fatal(err)
	}
	got, err := two.Next("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("a stopped run's operation %q was leased by %q", got.ID, "two")
	}
	stored, _, found, err := storeB.Operation(planned.ID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if stored.Lease != nil || stored.State == Leased || stored.State == Running {
		t.Fatalf("the refused acquisition still wrote the lease: %+v", stored)
	}
}

// TestAStaleWaitNeverUnStopsARun covers the one window the guard in
// recordDisposition cannot close by itself: a stop that commits between that
// guard's read of the run and its own write. Durably that is indistinguishable
// from any other stale settle - the journal carries run.waiting after
// run.cancelled and the run document says waiting - so the state is built here
// exactly as such a race would leave it, and it is also the state a database
// that raced before this repair is already sitting in.
//
// Replay is what remembers. The next pass must refuse on the run being terminal
// before it plans anything, settle the document back, and reach no provider.
func TestAStaleWaitNeverUnStopsARun(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
		t.Fatal(err)
	}
	invocations := len(f.provider.requests)

	// The losing driver's settle, landing after the stop.
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{"operation_unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: runID + "-stale-wait", RunID: runID,
		Type: EventRunWaiting, OccurredAt: f.clock.Now(), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	stale, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	stale.Disposition, stale.Reason = Waiting, "operation_unavailable"
	if err := f.store.PutRun(stale); err != nil {
		t.Fatal(err)
	}

	outcome, err := f.runtime.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Cancelled {
		t.Fatalf("a stale wait un-stopped the run: the next pass reported %q %q", outcome.Disposition, outcome.Reason)
	}
	if len(f.provider.requests) != invocations {
		t.Fatalf("the execution provider was invoked on a run the operator had stopped")
	}
	repaired, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if repaired.Disposition != Cancelled {
		t.Fatalf("the run document was left %q, so the supervisor keeps driving a stopped run", repaired.Disposition)
	}
}

// firingClock fires once, on the Nth Now() call. It is how a stop is landed in
// the middle of another goroutine's durable sequence without a sleep: the write
// under test is bracketed by Clock.Now() calls, so choosing N chooses the gap.
type firingClock struct {
	inner *steppingClock
	at, n int
	fire  func()
}

func (c *firingClock) Now() time.Time {
	c.n++
	if c.n == c.at && c.fire != nil {
		fire := c.fire
		c.fire = nil
		fire()
	}
	return c.inner.Now()
}

// TestAStaleSettleNeverOverwritesAStopInFlight lands the stop in the ONE gap a
// re-read cannot cover: after recordDisposition has read the run and found it
// live, and before it writes its own answer back.
//
// Durably this is the whole race. The run document is what Supervisor.Tick
// reads to build its active set and what the acquisition statement consults, so
// a pass that writes `waiting` over the operator's stop puts the run back in
// the fleet and re-opens acquisition, and replay - which knows better - is not
// consulted again until something loads the run.
func TestAStaleSettleNeverOverwritesAStopInFlight(t *testing.T) {
	for _, disposition := range []Disposition{Waiting, Failed} {
		t.Run(string(disposition), func(t *testing.T) {
			f := newPhase8Fixture(t)
			runID := f.start()
			if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
				t.Fatal(err)
			}
			state, err := f.runtime.load(runID)
			if err != nil {
				t.Fatal(err)
			}
			clock := &firingClock{inner: f.clock, at: 1}
			clock.fire = func() {
				if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
					t.Fatal(err)
				}
			}
			f.runtime.deps.Clock = clock
			if err := f.runtime.recordDisposition(state, disposition, "stale_"+string(disposition)); err != nil {
				t.Fatal(err)
			}
			if clock.fire != nil {
				t.Fatal("the stop never landed inside the write, so nothing was contested")
			}
			document, found, err := f.store.Run(runID)
			if err != nil || !found {
				t.Fatal(err, found)
			}
			if document.Disposition != Cancelled {
				t.Fatalf("a stale %q settle overwrote the operator's stop: the run document is %q", disposition, document.Disposition)
			}
			reloaded, err := f.runtime.load(runID)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.snapshot.Disposition != Cancelled {
				t.Fatalf("replay reports %q for a stopped run", reloaded.snapshot.Disposition)
			}
			// The pass must also KNOW, or it reports a pending stop as still
			// running to the operator who issued it.
			if state.run.Disposition != Cancelled {
				t.Fatalf("the pass went on believing the run was %q", state.run.Disposition)
			}
			// THE CRUX. A pass that loses this write still holds the live
			// value it read before the stop, and Reconcile carries on with it.
			// That buys the pass nothing only if the stale value cannot be
			// turned into a LEASE: handle has exactly one call site, directly
			// after Start, and Start only accepts an operation AcquireOperation
			// leased. So the lease is the single capability every material
			// action - provider invocation, candidate mutation, commit, push,
			// publication - is behind, and it is refused here.
			if _, _, err := f.runtime.scheduler.Plan(RunOperation{
				RunID: runID, Kind: OpExecutionInvoke, IdempotencyKey: "stale-pass-probe", MaxAttempts: 2,
			}); err != nil {
				t.Fatal(err)
			}
			leased, err := f.runtime.scheduler.Next(runID)
			if err != nil {
				t.Fatal(err)
			}
			if leased != nil {
				t.Fatalf("a pass that lost the stop race still leased %q, which is a Start away from the provider", leased.ID)
			}
		})
	}
}

// TestRecordDispositionPreReadGuardSkipsAnAlreadyCancelledRun isolates the
// guard at the very top of recordDisposition, separately from the mid-write
// race TestAStaleSettleNeverOverwritesAStopInFlight covers: here the stop has
// already landed and fully committed BEFORE recordDisposition is even called,
// so the caller is simply holding an in-memory state read before the stop -
// exactly what a losing pass in TestAStopAtAcquisitionNeverExecutesTheWorkItStopped
// does, just driven directly instead of through a contrived interleaving.
//
// Without the guard, the append below still happens: state.snapshot's
// disposition and reason are stale, so the change check on its own would
// write run.waiting straight after run.cancelled into the journal. PutRun's
// own condition still refuses to move the run document off cancelled, so
// nothing durable about the RUN drifts either way - but the hash chain gains
// an event that was never true, and nothing else in this suite reads the
// event count closely enough to notice. That is the whole gap: the guard is
// real, the mutation is observable, and until now nothing observed it.
func TestRecordDispositionPreReadGuardSkipsAnAlreadyCancelledRun(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	state, err := f.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}

	// The stop commits in full here - there is no race left to win or lose by
	// the time recordDisposition is called below.
	if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}

	// A reason no production path ever writes, so the change check that
	// gates the append would fire on it if the pre-read guard did not return
	// first.
	if err := f.runtime.recordDisposition(state, Waiting, "stale_after_stop"); err != nil {
		t.Fatal(err)
	}

	after, err := f.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("recordDisposition appended %d event(s) to the journal of a run the operator had already stopped", len(after)-len(before))
	}
	if state.run.Disposition != Cancelled {
		t.Fatalf("the caller went on believing the run was %q", state.run.Disposition)
	}
	document, found, err := f.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if document.Disposition != Cancelled {
		t.Fatalf("the run document was left %q", document.Disposition)
	}
}

// TestSQLiteAStoppedRunDocumentIsNeverReplaced states the durable rule on its
// own. PutRun is the only update path a run row has, so one condition there is
// the whole guarantee - and it has to be IN the statement, because the writer
// that loses this race lost it by reading first.
func TestSQLiteAStoppedRunDocumentIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	run := newJournalRun("run-a")
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	run.Disposition, run.Reason = Cancelled, "operator/stop"
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []Disposition{Waiting, Failed, Completed, Active} {
		attempt := run
		attempt.Disposition, attempt.Reason = stale, "stale_"+string(stale)
		if err := store.PutRun(attempt); err != nil {
			t.Fatal(err)
		}
		stored, found, err := store.Run("run-a")
		if err != nil || !found {
			t.Fatal(err, found)
		}
		if stored.Disposition != Cancelled || stored.Reason != "operator/stop" {
			t.Fatalf("a %q write replaced the operator's stop: %q %q", stale, stored.Disposition, stored.Reason)
		}
	}
	// A stop is still REPEATABLE: writing cancellation again must land, or the
	// retry path #179 established stops working.
	run.Reason = "operator/stop-again"
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.Run("run-a")
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if stored.Reason != "operator/stop-again" {
		t.Fatalf("a repeated stop was refused: reason is %q", stored.Reason)
	}
}

// chainedHistory builds a valid hash-chained journal of disposition events.
func chainedHistory(t *testing.T, runID string, types ...string) []EngineeringEvent {
	t.Helper()
	var out []EngineeringEvent
	var prev EngineeringEvent
	for i, eventType := range types {
		payload, err := json.Marshal(map[string]string{"reason": eventType})
		if err != nil {
			t.Fatal(err)
		}
		e := EngineeringEvent{
			SchemaVersion: SchemaVersion, ID: fmt.Sprintf("e%d", i+1), RunID: runID,
			Sequence: int64(i + 1), Type: eventType, OccurredAt: time.Unix(int64(i+1), 0), Payload: payload,
		}
		if i > 0 {
			e.PreviousEventID, e.PreviousEventHash = prev.ID, prev.EventHash
		}
		hash, err := EventDigest(e)
		if err != nil {
			t.Fatal(err)
		}
		e.EventHash = hash
		out = append(out, e)
		prev = e
	}
	return out
}

// TestReplayNeverUnCancelsARun is the other half of the same rule, on the other
// authority. The run document can be repaired from replay; replay cannot be
// repaired from anything, so a stale disposition event landing after a stop has
// to be inert there too.
//
// The merged case is NOT symmetric and is asserted as such. A cancelled run
// whose candidate merged genuinely IS completed - conditions() consults
// MergePrecedence before it consults cancellation - so guarding run.completed
// here would contradict a rule the runtime already states elsewhere.
func TestReplayNeverUnCancelsARun(t *testing.T) {
	for _, c := range []struct {
		name    string
		history []string
		want    Disposition
	}{
		{"a wait after a stop", []string{EventRunCancelled, EventRunWaiting}, Cancelled},
		{"a failure after a stop", []string{EventRunCancelled, EventRunFailed}, Cancelled},
		{"both after a stop", []string{EventRunCancelled, EventRunWaiting, EventRunFailed}, Cancelled},
		{"a merge after a stop", []string{EventRunCancelled, EventRunCompleted}, Completed},
		{"an ordinary wait", []string{EventRunWaiting}, Waiting},
		{"an ordinary failure", []string{EventRunFailed}, Failed},
		{"a failure then a wait", []string{EventRunFailed, EventRunWaiting}, Waiting},
		{"a stop after a failure", []string{EventRunFailed, EventRunCancelled}, Cancelled},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := EngineeringRun{SchemaVersion: SchemaVersion, ID: "r", Disposition: Active}
			snapshot, err := Reduce(run, chainedHistory(t, "r", c.history...))
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Disposition != c.want {
				t.Fatalf("replay of %v reports %q, want %q", c.history, snapshot.Disposition, c.want)
			}
		})
	}
}

// TestALateResultFromAStoppedRunGainsNoAuthority is the in-flight half, and it
// is the one the acquisition condition deliberately does NOT cover.
//
// The stop lands while the producer is inside Provider.Execute. That attempt is
// not interrupted - #213 is cooperative process cancellation and reproduces on
// main - and it is allowed to finish and to journal what it did. What is
// asserted is the corollary: finishing must not GIVE IT BACK anything. A late
// result may not become a commit, a push or a pull request merely because the
// external process managed to return.
//
// The mechanism is deliberately NOT a cancellation check in the effect path,
// and that distinction is the point. execution.completed confers authority
// through exactly one route - RunProjection.CandidateComplete, read only by
// this run's own planner and binder - and every stage it makes eligible needs a
// FRESH LEASE. Cross-run consumption goes through the plan reconciler, which
// reads the run's disposition directly. So authority loss has consequences
// rather than deputies, and adding a fifth place that remembers `cancelled`
// would be the mistake this test exists to make unnecessary.
//
// TWO DIFFERENT THINGS REFUSE THAT LEASE, and the test asserts both rather than
// letting the weaker one stand in for the stronger. A later pass reloads, sees
// the stop in replay, and is refused by validate on `run is terminal` - that is
// ordinary and predates this PR. A pass that loaded BEFORE the stop has no such
// knowledge and is refused by the acquisition statement instead. Only the
// second arm is sensitive to this PR's conditions; asserting only the first
// would be a test that passes for a reason it does not name.
func TestALateResultFromAStoppedRunGainsNoAuthority(t *testing.T) {
	f := newPhase8Fixture(t)
	var runID string
	stopped := false
	f.provider.mutate = func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "produced.txt"), []byte("work\n"), 0600); err != nil {
			return err
		}
		if !stopped {
			stopped = true
			if _, err := CancelRun(f.store, f.runtime.scheduler, f.clock.Now(), runID, "operator/stop"); err != nil {
				return err
			}
		}
		return nil
	}
	runID = f.start()
	for pass := 0; pass < 8 && !stopped; pass++ {
		if _, err := f.runtime.Reconcile(context.Background(), runID); err != nil {
			break
		}
	}
	if !stopped {
		t.Fatal("the producer never ran, so no in-flight stop was contested")
	}
	// The attempt was NOT interrupted. That is #213 and is stated, not hidden.
	if len(f.provider.requests) == 0 {
		t.Fatal("the fixture did not actually invoke the producer")
	}
	// TRUTH IS KEPT, AUTHORITY IS NOT - the #140 split, applied to the other
	// revocation cause. The producer mutated the workspace and the journal says
	// so; what it must not say is that an execution COMPLETED, because that is
	// the fact a subject becomes eligible to be committed, pushed and published
	// on.
	after, err := f.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	truth, authority := false, false
	for _, e := range after.events {
		switch e.Type {
		case EventCandidateChanged:
			truth = true
		case EventExecutionCompleted:
			authority = true
		}
	}
	if !truth {
		t.Fatal("the producer's mutation was not journalled; a stop must not make the record less honest")
	}
	if authority {
		t.Fatal("a run stopped mid-flight journalled execution.completed, admitting a result produced without authority")
	}
	if after.projection.CandidateComplete {
		t.Fatal("the stopped run's candidate is marked complete, which is what makes it eligible to be committed")
	}
	// Whatever that attempt journalled, no further pass may turn it into a
	// material effect. Several passes are driven precisely to give it the
	// chance: each one reloads, and each one must refuse.
	for pass := 0; pass < 3; pass++ {
		outcome, err := f.runtime.Reconcile(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Disposition != Cancelled {
			t.Fatalf("a pass after the stop reported %q %q", outcome.Disposition, outcome.Reason)
		}
	}
	state, err := f.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range state.events {
		switch e.Type {
		case EventCandidateCommitted, EventCandidateBaseIntegrated:
			if committedAfterStop(state.events, e) {
				t.Fatalf("a stopped run produced %q after the operator stopped it", e.Type)
			}
		}
	}
	if len(f.forge.PullRequests) != 0 {
		t.Fatalf("a stopped run opened %d pull request(s)", len(f.forge.PullRequests))
	}
	// THE STALE ARM. A driver that read this run before the stop has none of
	// the knowledge the passes above used, so nothing it consults in memory can
	// refuse it. It must still be unable to obtain the one capability that
	// leads to a material effect.
	if _, _, err := f.runtime.scheduler.Plan(RunOperation{
		RunID: runID, Kind: OpCandidateCommit, IdempotencyKey: "late-authority-probe", MaxAttempts: 2,
	}); err != nil {
		t.Fatal(err)
	}
	leased, err := f.runtime.scheduler.Next(runID)
	if err != nil {
		t.Fatal(err)
	}
	if leased != nil {
		t.Fatalf("a stale driver leased %q on a stopped run whose producer had just finished", leased.ID)
	}
	// And it is holding nothing, which is what #171 was about in the first
	// place: the slot came back even though the attempt outlived the stop.
	operations, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if op.Lease != nil {
			t.Fatalf("operation %q still holds a lease on a stopped run", op.ID)
		}
	}
}

// committedAfterStop reports whether one event was journalled after the run was
// cancelled, by position in the chain rather than by wall clock.
func committedAfterStop(events []EngineeringEvent, subject EngineeringEvent) bool {
	stop := -1
	for i, e := range events {
		if e.Type == EventRunCancelled {
			stop = i
			break
		}
	}
	if stop < 0 {
		return false
	}
	for i, e := range events {
		if e.ID == subject.ID {
			return i > stop
		}
	}
	return false
}
