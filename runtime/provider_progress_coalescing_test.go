//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #352: durable provider progress is coalesced rather than dropped, is bound
// to its physical attempt, survives a crash with a stated bound, and carries
// the structured open-tool suspension status needs. The lettered tests follow
// the issue's test requirements.

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type progressWrite struct {
	ProviderProgress
	wroteAt time.Time
}

// progressLog is a recorder that remembers every write handed to the store.
type progressLog struct {
	mu     sync.Mutex
	writes []progressWrite
}

func (l *progressLog) record(p ProviderProgress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, progressWrite{p, time.Now()})
}

func (l *progressLog) snapshot() []progressWrite {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]progressWrite(nil), l.writes...)
}

func (l *progressLog) waitFor(t *testing.T, n int) []progressWrite {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if got := l.snapshot(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d durable writes, want %d: %+v", len(l.snapshot()), n, l.snapshot())
		}
	}
}

// armedWatch is one process's watch, started, with its joining stop.
func armedWatch(limit time.Duration, record func(ProviderProgress)) (*inactivityWatch, func()) {
	ctx, release := withProviderInactivity(context.Background(), limit, record)
	watch := armInactivityWatch(ctx)
	stop := watch.watchUntilComplete()
	return watch, func() { stop(); release() }
}

// liveScheduler is a scheduler on the real clock, so the instants the watch
// observes and the rows the scheduler writes share one time line.
func liveScheduler() Scheduler {
	return Scheduler{Store: NewMemoryOperationStore(), Clock: RealClock{}, Owner: "owner", LeaseDuration: time.Minute}
}

func row(t *testing.T, s Scheduler, id string) RunOperation {
	t.Helper()
	op, _, ok, err := s.Store.Operation(id)
	if err != nil || !ok {
		t.Fatalf("operation %s: %v", id, err)
	}
	return op
}

// boundRecorder is operations.go's recorder: every write bound to one
// operation and one physical attempt.
func boundRecorder(s Scheduler, op RunOperation, log *progressLog) func(ProviderProgress) {
	return func(p ProviderProgress) {
		log.record(p)
		_, _ = s.RecordProviderProgress(op.ID, op.AttemptIdentity, p)
	}
}

// A. Burst then quiet: the first write is immediate, the burst is coalesced
// into ONE trailing write of the newest key at the instant it was observed, no
// two writes start less than an interval apart, and the closing write
// persists the last observation.
func TestCoalescedProgressPersistsTheNewestObservationAtItsInstant(t *testing.T) {
	const limit = 400 * time.Millisecond
	interval := progressRecordInterval(limit)
	log := &progressLog{}
	watch, stop := armedWatch(limit, log.record)
	defer stop()

	watch.progress(1)
	first := log.waitFor(t, 1)[0]
	if first.Key != "1" || first.Final {
		t.Fatalf("first write = %+v, want key 1 at once", first)
	}
	var lastAt time.Time
	for range 5 {
		time.Sleep(5 * time.Millisecond)
		lastAt = time.Now()
		watch.progress(1)
	}
	writes := log.waitFor(t, 2)
	trailing := writes[1]
	if trailing.Key != "6" {
		t.Fatalf("trailing key %q, want the newest (6): an older pending value survived", trailing.Key)
	}
	if gap := trailing.wroteAt.Sub(first.wroteAt); gap < interval-5*time.Millisecond {
		t.Fatalf("writes %s apart, want at least the %s interval: the maximum write rate rose", gap, interval)
	}
	observed := trailing.wroteAt.Add(-trailing.Age)
	if skew := observed.Sub(lastAt); skew < -5*time.Millisecond || skew > 5*time.Millisecond {
		t.Fatalf("persisted instant is %s from the observation: the timer's instant was recorded", skew)
	}
	time.Sleep(2 * interval)
	if n := len(log.snapshot()); n != 2 {
		t.Fatalf("%d writes with no new progress, want 2", n)
	}
	stop()
	final := log.snapshot()
	if len(final) != 3 || !final[2].Final || final[2].Key != "6" || final[2].Suspended {
		t.Fatalf("closing write = %+v, want one Final write of key 6 and no suspension", final)
	}
}

// A (rate). Sustained output never writes faster than once per interval.
func TestSustainedProgressKeepsTheMaximumWriteRate(t *testing.T) {
	const limit = 400 * time.Millisecond
	interval := progressRecordInterval(limit)
	log := &progressLog{}
	watch, stop := armedWatch(limit, log.record)
	started := time.Now()
	for time.Since(started) < 500*time.Millisecond {
		watch.progress(1)
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	writes := log.snapshot()
	running := writes[:len(writes)-1]
	for i := 1; i < len(running); i++ {
		if gap := running[i].wroteAt.Sub(running[i-1].wroteAt); gap < interval-5*time.Millisecond {
			t.Fatalf("writes %d and %d are %s apart, under the %s interval", i-1, i, gap, interval)
		}
	}
	if bound := int(time.Since(started)/interval) + 2; len(running) > bound {
		t.Fatalf("%d writes in %s, want at most %d", len(running), time.Since(started), bound)
	}
	if closing := writes[len(writes)-1]; !closing.Final || closing.Key != fmt.Sprint(watch.bytes.Load()) {
		t.Fatalf("closing write %+v, want the last count %d", closing, watch.bytes.Load())
	}
}

// B. A deferred write of attempt N cannot mutate attempt N+1, no key,
// instant, suspension or recorder flag crosses the succession, and nothing of
// N's recorder runs after its stop returns.
func TestADeferredWriteCannotCrossIntoTheSuccessorAttempt(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	log := &progressLog{}
	watch, stop := armedWatch(400*time.Millisecond, boundRecorder(scheduler, op, log))

	watch.record("1:1", time.Now(), time.Time{})
	log.waitFor(t, 1)
	if got := row(t, scheduler, op.ID); got.NoProgressKey != "1:1" || !got.ProgressRecorderOpen {
		t.Fatalf("attempt N's own write did not land: %+v", got)
	}
	// A deferred write, suspension and all, pending when N settles.
	watch.record("1:2", time.Now(), time.Now())
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	successor, err := scheduler.Start(op.ID)
	if err != nil || successor.AttemptIdentity == op.AttemptIdentity {
		t.Fatalf("no successor attempt: %+v %v", successor, err)
	}
	if n := len(log.snapshot()); n != 1 {
		t.Fatalf("the deferred write fired before the succession it is meant to race (%d writes)", n)
	}
	log.waitFor(t, 2) // N's deferred write fires into N+1's row
	stop()            // and N's closing write
	after := row(t, scheduler, op.ID)
	if after.NoProgressKey != successor.NoProgressKey || !after.LastProgressAt.Equal(*successor.LastProgressAt) ||
		after.ProgressRecorderOpen || after.InactivitySuspension != nil {
		t.Fatalf("attempt N wrote into N+1: before %+v after %+v", successor, after)
	}
	if n := len(log.snapshot()); n != 3 {
		t.Fatalf("%d writes, want N's first, deferred and closing", n)
	}
	// Stale work after stop is a no-op, and nothing of N is still running.
	watch.record("1:3", time.Now(), time.Time{})
	watch.progress(1)
	time.Sleep(2 * progressRecordInterval(400*time.Millisecond))
	if n := len(log.snapshot()); n != 3 {
		t.Fatalf("N's recorder wrote after its stop returned (%d writes)", n)
	}
	for deadline := time.Now().Add(2 * time.Second); goruntime.NumGoroutine() > baseline; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, baseline %d: a watcher or flush outlived the attempt", goruntime.NumGoroutine(), baseline)
		}
	}
}

// claudeToolProgress is a tool heartbeat: neither progress nor a suspension.
func claudeToolProgress(id string) string {
	return `{"type":"tool_progress","tool_use_id":"` + id + `","elapsed_time_seconds":1}`
}

// toolProgressFor is a shell loop emitting only tool heartbeats for d.
func toolProgressFor(d time.Duration) string {
	return fmt.Sprintf("i=0\nwhile [ $i -lt %d ]; do %s; sleep 0.05; i=$((i+1)); done\n",
		int(d/(50*time.Millisecond)), strings.TrimSuffix(emit(claudeToolProgress("X")), "\n"))
}

// C. A structured tool open far longer than the window: the watch does not
// expire it, the durable row carries the suspension of exactly this attempt
// while it is open, tool_progress moves nothing, and the suspension ends with
// the tool.
func TestALongStructuredToolIsSuspendedDurably(t *testing.T) {
	const toolRuns = 1200 * time.Millisecond // three windows
	provider, request := claudeProcess(t,
		emit(claudeAssistant("m1", "", claudeText), claudeAssistant("M2", "", claudeToolUse("X")))+
			toolProgressFor(toolRuns)+
			emit(claudeToolResult("X", ""), claudeAssistant("m3", "", claudeText), claudeResult(false, "success", 0)))
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	log := &progressLog{}
	var rows []RunOperation
	var mu sync.Mutex
	ctx := withProviderProgressRecorder(context.Background(), func(p ProviderProgress) {
		boundRecorder(scheduler, op, log)(p)
		current, _, _, _ := scheduler.Store.Operation(op.ID)
		mu.Lock()
		rows = append(rows, current)
		mu.Unlock()
	})
	started := time.Now()
	result, err := provider.Execute(ctx, request)
	if err != nil || result.Outcome != Succeeded {
		t.Fatalf("a healthy long tool was ended: %v %#v %+v", err, result.Failure, result.Invocation)
	}
	if elapsed := time.Since(started); elapsed < toolRuns {
		t.Fatalf("finished in %s: the tool never outlived the window", elapsed)
	}
	suspended := 0
	for _, r := range rows {
		if s := r.InactivitySuspension; s != nil {
			suspended++
			if s.AttemptIdentity != op.AttemptIdentity || s.Since.Before(started) || s.Since.After(started.Add(toolRuns)) {
				t.Fatalf("suspension %+v does not belong to this attempt's tool", s)
			}
			if r.NoProgressKey != "1:2" {
				t.Fatalf("key %q while suspended: tool_progress was counted as progress", r.NoProgressKey)
			}
		}
	}
	if suspended == 0 {
		t.Fatalf("the open tool never reached the durable row: %+v", log.snapshot())
	}
	// D. The tool's result is progress and closes the suspension.
	writes := log.snapshot()
	sawClose := false
	for _, w := range writes {
		if !w.Suspended && !w.Final && w.Key > "1:2" {
			sawClose = true
		}
	}
	if !sawClose {
		t.Fatalf("no durable write closed the suspension: %+v", writes)
	}
	end := row(t, scheduler, op.ID)
	if end.InactivitySuspension != nil || end.ProgressRecorderOpen || end.NoProgressKey != "1:4" {
		t.Fatalf("after the attempt: %+v", end)
	}
	if result.Invocation.Deadline == nil {
		t.Fatal("the absolute deadline is not recorded for the suspended attempt")
	}
}

// D. Once the tool closes the window runs again, and the durable row says so.
func TestClosingTheToolResumesInactivityDurably(t *testing.T) {
	provider, request := claudeProcess(t,
		emit(claudeAssistant("M1", "", claudeToolUse("X")))+toolProgressFor(600*time.Millisecond)+
			emit(claudeToolResult("X", ""))+"while :; do sleep 30; done\n")
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	log := &progressLog{}
	ctx := withProviderProgressRecorder(context.Background(), boundRecorder(scheduler, op, log))
	result, _ := provider.Execute(ctx, request)
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure = %#v, want inactivity once the tool closed", result.Failure)
	}
	writes := log.snapshot()
	if len(writes) < 2 || !writes[0].Suspended {
		t.Fatalf("writes %+v, want the open tool first", writes)
	}
	closed := false
	for _, w := range writes[1:] {
		closed = closed || (!w.Suspended && w.Key == "1:2")
	}
	if !closed {
		t.Fatalf("writes %+v: the tool result did not clear the suspension", writes)
	}
}

// E. However the attempt ends with a tool open, its suspension ends with it
// and cannot govern a later attempt.
func TestASuspensionEndsWithItsAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		wall   time.Duration
	}{
		"return":       {emit(claudeAssistant("M1", "", claudeToolUse("X"))) + "sleep 0.2\n" + emit(claudeResult(false, "success", 0)), time.Hour},
		"fail":         {emit(claudeAssistant("M1", "", claudeToolUse("X"))) + "exit 3\n", time.Hour},
		"attempt_wall": {emit(claudeAssistant("M1", "", claudeToolUse("X"))) + "while :; do sleep 30; done\n", 1200 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request := claudeProcess(t, tc.script)
			request.Budgets.WallLimit = tc.wall
			scheduler := liveScheduler()
			op := plannedExecution(t, scheduler, time.Hour)
			log := &progressLog{}
			ctx := withProviderProgressRecorder(context.Background(), boundRecorder(scheduler, op, log))
			started := time.Now()
			result, _ := provider.Execute(ctx, request)
			if name == "attempt_wall" {
				// H. The absolute deadline still ends a suspended attempt.
				if time.Since(started) < tc.wall || result.Invocation.TerminationCause != "deadline_reached" || result.Invocation.OpenToolsAtExit != 1 {
					t.Fatalf("provenance %+v: the deadline did not bound the open tool", result.Invocation)
				}
			}
			writes := waitForClosingWrite(t, log)
			if !writes[0].Suspended {
				t.Fatalf("writes %+v, want the open tool recorded first", writes)
			}
			if closing := writes[len(writes)-1]; closing.Suspended {
				t.Fatalf("closing write %+v still claims the tool open", closing)
			}
			if r := row(t, scheduler, op.ID); r.InactivitySuspension != nil || r.ProgressRecorderOpen {
				t.Fatalf("row after the attempt: %+v", r)
			}
		})
	}
	t.Run("abandon", func(t *testing.T) {
		scheduler := liveScheduler()
		op := plannedExecution(t, scheduler, time.Hour)
		if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1:1", Suspended: true}); err != nil {
			t.Fatal(err)
		}
		// The controller dies with the tool open: nothing closed it.
		if r := row(t, scheduler, op.ID); r.InactivitySuspension == nil {
			t.Fatal("precondition: the suspension is not durable")
		}
		abandonExecution(t, scheduler, op.ID)
		successor, err := scheduler.Start(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if successor.InactivitySuspension != nil {
			t.Fatalf("the successor inherited a dead attempt's suspension: %+v", successor.InactivitySuspension)
		}
		// A late write of the dead attempt cannot put it back.
		if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1:2", Suspended: true}); err != nil {
			t.Fatal(err)
		}
		if r := row(t, scheduler, op.ID); r.InactivitySuspension != nil {
			t.Fatalf("attempt %d's stale suspension landed on attempt %d", op.AttemptIdentity, r.AttemptIdentity)
		}
		// And a settlement clears whatever the attempt still carried.
		if _, err := scheduler.RecordProviderProgress(op.ID, successor.AttemptIdentity, ProviderProgress{Key: "2:1", Suspended: true}); err != nil {
			t.Fatal(err)
		}
		settled, err := scheduler.Finish(op.ID, OperationFailed)
		if err != nil || settled.InactivitySuspension != nil || settled.ProgressRecorderOpen {
			t.Fatalf("settled row %+v %v", settled, err)
		}
	})
}

// C (status). The suspension is reported structurally for the live attempt,
// silence past the limit is not presented as a pending kill, and the absolute
// deadline stays visible. Another attempt's suspension is never reported.
func TestStatusReportsTheLiveAttemptsSuspension(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		defer ownerLiveness(f, true)()
		recorded, err := f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity,
			ProviderProgress{Key: "1:2", Suspended: true})
		if err != nil || recorded.InactivitySuspension == nil {
			t.Fatalf("the open tool was not recorded: %+v %v", recorded, err)
		}
		opened := recorded.InactivitySuspension.Since
		f.clock.advance(25 * time.Minute)
		status := liveStatus(t, f, runID)
		if status.InactivitySuspension != "active" || status.InactivitySuspendedSince == nil ||
			!status.InactivitySuspendedSince.Equal(opened) || status.ProgressSource != "row" {
			t.Fatalf("status %+v does not report the open tool since %s", status, opened)
		}
		if journal.Deadline == nil || status.Deadline == nil || !status.Deadline.Equal(*journal.Deadline) {
			t.Fatalf("deadline %v, want the attempt's absolute %v", status.Deadline, journal.Deadline)
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"inactivity_suspension":"active"`, `"inactivity_suspended_since"`, `"deadline"`} {
			if !strings.Contains(string(encoded), field) {
				t.Fatalf("status JSON %s lacks %s", encoded, field)
			}
		}
		restore := rewriteRow(t, f, journal.ID, func(op *RunOperation) { op.InactivitySuspension.AttemptIdentity++ })
		if got := liveStatus(t, f, runID); got.InactivitySuspension != "" || got.InactivitySuspendedSince != nil {
			t.Fatalf("another attempt's suspension was reported: %v", got.InactivitySuspendedSince)
		}
		restore()
		clear := rewriteRow(t, f, journal.ID, func(op *RunOperation) { op.InactivitySuspension = nil })
		if got := liveStatus(t, f, runID); got.InactivitySuspension != "" || got.InactivitySuspendedSince != nil {
			t.Fatal("a closed tool is still reported open")
		}
		clear()
	})
}

// F. A byte-output process ends under an observing controller and the
// controller then restarts before settling: the closing write made the row
// exact, so the successor's window is the true remainder - no false
// provider_no_progress from a burst the recorder had not yet written, and no
// allowance either.
func TestAnEndedByteOutputProcessLeavesAnExactInactivityDatum(t *testing.T) {
	const limit = 2 * time.Second // a 500ms interval: the whole burst fits inside it
	provider, request, _ := inactivityFixture(t, "echo a; sleep 0.2; echo b; sleep 0.2; echo c\n")
	request.Budgets.InactivityLimit = limit
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	log := &progressLog{}
	ctx := withProviderProgressRecorder(context.Background(), boundRecorder(scheduler, op, log))
	if _, err := provider.Execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	ended := row(t, scheduler, op.ID)
	if ended.ProgressRecorderOpen || ended.LastProgressAt.Sub(*ended.StartedAt) < 300*time.Millisecond {
		t.Fatalf("row %+v: the last byte, 400ms in, is not the datum", ended)
	}
	abandonExecution(t, scheduler, op.ID)
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	at := resumed.LastProgressAt.Add(1800 * time.Millisecond)
	if got := dispatchInactivityWindow(limit, resumed, at, codex); got != 200*time.Millisecond {
		t.Fatalf("window %s, want exactly the 200ms the last byte leaves", got)
	}
}

// G. THE CRASH RULE. A controller that dies with an observation still pending
// leaves LastProgressAt behind the truth and the row's recorder open. The
// successor's window charges the recorded silence less L, where L is
// progressRecorderLag: two record intervals, half the window.

// crashedRecorderRow runs a real recorder into a real row and "kills the
// controller" - nothing more becomes durable - right after the ADVERSARIAL
// pattern: an observation just after a write, carried by the trailing write
// with its own older instant, then another observation just before the next
// write is due. It returns the crashed row and the lost observation's instant.
func crashedRecorderRow(t *testing.T, scheduler Scheduler, op RunOperation, limit time.Duration) (RunOperation, time.Time) {
	t.Helper()
	interval := progressRecordInterval(limit)
	var dead atomic.Bool
	log := &progressLog{}
	watch, stop := armedWatch(limit, func(p ProviderProgress) {
		if !dead.Load() {
			boundRecorder(scheduler, op, log)(p)
		}
	})
	watch.progress(1)
	log.waitFor(t, 1)
	time.Sleep(5 * time.Millisecond)
	watch.progress(1) // just after the first write: pending for an interval
	trailing := log.waitFor(t, 2)[1]
	time.Sleep(interval * 3 / 4)
	lost := time.Now()
	watch.progress(1) // just before the next write is due
	dead.Store(true)  // the controller dies
	stop()
	crashed := row(t, scheduler, op.ID)
	if crashed.NoProgressKey != "2" || !crashed.ProgressRecorderOpen || trailing.Key != "2" {
		t.Fatalf("precondition: row %+v should hold the trailing write, recorder open", crashed)
	}
	return crashed, lost
}

// G1. The lost observation trails the durable datum by MORE than one interval
// - the trailing write carried an older instant - so an allowance of one
// interval (limit/4) would still refuse a provider whose true silence is
// inside the window. L covers it: no false provider_no_progress.
func TestACrashJustBeforeTheTrailingWriteIsNotRefused(t *testing.T) {
	const limit = 800 * time.Millisecond
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	crashed, lost := crashedRecorderRow(t, scheduler, op, limit)
	recorded := *crashed.LastProgressAt
	if lag := lost.Sub(recorded); lag <= progressRecordInterval(limit) || lag >= 2*progressRecordInterval(limit) {
		t.Fatalf("precondition: the lost progress trails the datum by %s, want between one interval and L", lag)
	}
	abandonExecution(t, scheduler, op.ID)
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	restart := lost.Add(limit - 20*time.Millisecond) // true silence: inside the window
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	got := dispatchInactivityWindow(limit, resumed, restart, codex)
	if got <= 0 {
		t.Fatalf("refused as provider_no_progress after %s of true silence in a %s window", restart.Sub(lost), limit)
	}
	if want := limit - (restart.Sub(recorded) - progressRecorderLag(limit)); got != want {
		t.Fatalf("window %s, want %s: recorded silence less L", got, want)
	}
}

// G2. When nothing was actually lost, the allowance is exactly L - never more,
// and never a fresh window. L is half the window: 300s for the shipped 600s.
func TestTheCrashAllowanceIsExactlyL(t *testing.T) {
	const limit = 10 * time.Minute
	const L = 5 * time.Minute
	if got := progressRecorderLag(limit); got != L {
		t.Fatalf("L = %s for a %s window, want %s", got, limit, L)
	}
	if got := progressRecorderLag(DefaultProviderInactivitySeconds * time.Second); got != 300*time.Second {
		t.Fatalf("L = %s for the shipped window, want 300s", got)
	}
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	for _, final := range []bool{false, true} {
		scheduler, clock := deadlineScheduler(t)
		op := plannedExecution(t, scheduler, time.Hour)
		if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1", Final: final}); err != nil {
			t.Fatal(err)
		}
		datum := clock.Now()
		clock.advance(time.Minute)
		abandonExecution(t, scheduler, op.ID)
		resumed, err := scheduler.Start(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, silence := range []time.Duration{0, time.Minute, L, L + time.Minute, limit, limit + L - time.Second, limit + L, 2 * limit} {
			got := dispatchInactivityWindow(limit, resumed, datum.Add(silence), codex)
			exact := max(limit-silence, 0)
			want := exact
			if !final {
				want = max(limit+L-silence, 0)
				if want > limit {
					want = limit
				}
			}
			if got != want {
				t.Fatalf("final=%t silence %s: window %s, want %s", final, silence, got, want)
			}
			if grant := got - exact; grant > L || (final && grant != 0) {
				t.Fatalf("final=%t silence %s: granted %s beyond the recorded silence, over L=%s", final, silence, grant, L)
			}
			if silence >= L && silence <= limit && !final && got-exact != L {
				t.Fatalf("silence %s: granted %s, want exactly L up to the window's end", silence, got-exact)
			}
		}
	}
}

// G3. Repeated crashes do not accumulate the allowance: it is a function of
// the one durable datum, which a restart never moves. However many successors
// are dispatched against it, none may run past LastProgressAt + limit + L, and
// the attempt ceiling, consumed execution and identity all keep converging.
func TestRepeatedCrashesDoNotAccumulateTheAllowance(t *testing.T) {
	const limit = 10 * time.Minute
	L := progressRecorderLag(limit)
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	scheduler, clock := deadlineScheduler(t)
	planned, _, err := scheduler.Plan(RunOperation{
		RunID: "run-g3", Kind: OpExecutionInvoke, IdempotencyKey: "invoke-g3", MaxAttempts: 20, WallBudget: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(planned.RunID); err != nil {
		t.Fatal(err)
	}
	op, err := scheduler.Start(planned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1"}); err != nil {
		t.Fatal(err)
	}
	datum := clock.Now()
	bound := datum.Add(limit + L)
	consumed, identity := op.ConsumedExecution, op.AttemptIdentity
	for cycle := 1; cycle <= 7; cycle++ {
		clock.advance(3 * time.Minute) // each successor silent, then its controller dies
		abandonExecution(t, scheduler, op.ID)
		if op, err = scheduler.Start(op.ID); err != nil {
			t.Fatal(err)
		}
		now := clock.Now()
		if !op.LastProgressAt.Equal(datum) || !op.ProgressRecorderOpen {
			t.Fatalf("cycle %d: a restart moved the datum to %v (open %t)", cycle, op.LastProgressAt, op.ProgressRecorderOpen)
		}
		window := dispatchInactivityWindow(limit, op, now, codex)
		if expiry := now.Add(window); window > 0 && expiry.After(bound) {
			t.Fatalf("cycle %d: the successor may run silent until %s, past the one-allowance bound %s", cycle, expiry, bound)
		}
		if silence := now.Sub(datum); silence >= limit+L && window != 0 {
			t.Fatalf("cycle %d: %s of silence still left %s", cycle, silence, window)
		}
		if op.ConsumedExecution < consumed || op.AttemptIdentity <= identity {
			t.Fatalf("cycle %d: consumed %s (was %s), identity %d (was %d)", cycle, op.ConsumedExecution, consumed, op.AttemptIdentity, identity)
		}
		consumed, identity = op.ConsumedExecution, op.AttemptIdentity
	}
}

// G4. Enough real silence still ends the provider: the allowance delays
// provider_no_progress by at most L, it never prevents it. The window a
// crashed datum leaves is the finite bound the next process runs under, and a
// silent process is killed by it.
func TestEnoughSilenceStillEndsTheProviderDespiteTheAllowance(t *testing.T) {
	const limit = 800 * time.Millisecond
	L := progressRecorderLag(limit)
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, time.Hour)
	if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1"}); err != nil {
		t.Fatal(err)
	}
	datum := clock.Now()
	abandonExecution(t, scheduler, op.ID)
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	// At limit + L it is exhausted: invokeExecution refuses before dispatch.
	if got := dispatchInactivityWindow(limit, resumed, datum.Add(limit+L), codex); got != 0 {
		t.Fatalf("window %s after limit plus L of silence", got)
	}
	// Short of it, the remainder is a real, finite bound on a silent process.
	window := dispatchInactivityWindow(limit, resumed, datum.Add(limit+L-300*time.Millisecond), codex)
	if window != 300*time.Millisecond {
		t.Fatalf("window %s, want the 300ms left before limit plus L", window)
	}
	provider, request, _ := inactivityFixture(t, "sleep 30\n")
	request.Budgets.InactivityLimit = window
	started := time.Now()
	result, _ := provider.Execute(context.Background(), request)
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure %#v, want provider_no_progress", result.Failure)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the silent successor ran %s under a %s window", elapsed, window)
	}
}

// G5. Crash uncertainty never extends the attempt wall or the run's active
// work: the deadline a successor starts under is the same whether its
// inherited datum carries an allowance or not.
func TestCrashUncertaintyNeverExtendsTheAttemptWall(t *testing.T) {
	clock := &steppingClock{at: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	var started []RunOperation
	for _, final := range []bool{false, true} {
		scheduler := Scheduler{Store: NewMemoryOperationStore(), Clock: clock, Owner: "owner", LeaseDuration: time.Minute}
		op := plannedExecution(t, scheduler, 30*time.Minute)
		if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, ProviderProgress{Key: "1", Final: final}); err != nil {
			t.Fatal(err)
		}
		abandonExecution(t, scheduler, op.ID)
		resumed, err := scheduler.StartWithin(op.ID, &AttemptLimit{Within: 20 * time.Minute, Bound: BoundAttemptWall})
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, resumed)
	}
	open, exact := started[0], started[1]
	if !open.ProgressRecorderOpen || exact.ProgressRecorderOpen {
		t.Fatalf("precondition: open %t, exact %t", open.ProgressRecorderOpen, exact.ProgressRecorderOpen)
	}
	if open.Deadline == nil || !open.Deadline.Equal(*exact.Deadline) || open.DeadlineBound != exact.DeadlineBound ||
		open.ConsumedExecution != exact.ConsumedExecution {
		t.Fatalf("the allowance moved execution authority: open %v %s %s, exact %v %s %s",
			open.Deadline, open.DeadlineBound, open.ConsumedExecution, exact.Deadline, exact.DeadlineBound, exact.ConsumedExecution)
	}
	if want := clock.Now().Add(20 * time.Minute); !open.Deadline.Equal(want) || open.DeadlineBound != BoundAttemptWall {
		t.Fatalf("deadline %v (%s), want the attempt wall at %v", open.Deadline, open.DeadlineBound, want)
	}
}

// H. Guards: chatter and raw bytes are never durable progress, and a tool
// heartbeat without an open tool creates no suspension.
func TestNonProgressNeverReachesTheDurableRecorder(t *testing.T) {
	const limit = 400 * time.Millisecond
	t.Run("codex transport chatter", func(t *testing.T) {
		log := &progressLog{}
		watch, stop := armedWatch(limit, log.record)
		filter := &chatterFilter{progress: watch.progress, patterns: []string{"reconnecting"}}
		_, _ = filter.Write([]byte("Reconnecting... 1/5\n"))
		time.Sleep(2 * progressRecordInterval(limit))
		if n := len(log.snapshot()); n != 0 {
			t.Fatalf("chatter was recorded as progress (%d writes)", n)
		}
		_, _ = filter.Write([]byte("real output\n"))
		log.waitFor(t, 1)
		stop()
	})
	t.Run("claude raw bytes and tool heartbeats", func(t *testing.T) {
		log := &progressLog{}
		watch, stop := armedWatch(limit, log.record)
		stream := newClaudeStream(1)
		stream.attach(watch)
		feed(stream, "not-json", "warning: something", claudeToolProgress("X"), claudeInit)
		time.Sleep(2 * progressRecordInterval(limit))
		if n := len(log.snapshot()); n != 0 {
			t.Fatalf("non-events were recorded (%d writes)", n)
		}
		feed(stream, claudeAssistant("m1", "", claudeText), claudeToolProgress("X"))
		if w := log.waitFor(t, 1)[0]; w.Key != "1:1" || w.Suspended {
			t.Fatalf("write %+v, want one accepted event and no suspension", w)
		}
		stop()
	})
}

// waitForClosingWrite returns the writes once the recorder's closing one has
// landed. An invocation whose context already ended returns without waiting
// for it, so it may land just after.
func waitForClosingWrite(t *testing.T, log *progressLog) []progressWrite {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if writes := log.snapshot(); len(writes) > 0 && writes[len(writes)-1].Final {
			return writes
		}
		if time.Now().After(deadline) {
			t.Fatalf("no closing write: %+v", log.snapshot())
		}
	}
}

// ownerLiveness sets what status's liveness evidence says about every owner,
// and returns the restore.
func ownerLiveness(f *phase8Fixture, alive bool) func() {
	previous := f.runtime.scheduler.Liveness
	f.runtime.scheduler.Liveness = OwnerLivenessFunc(func(string) bool { return alive })
	return func() { f.runtime.scheduler.Liveness = previous }
}

// B (stale suspension). A recorded suspension is ACTIVE only while the process
// that recorded it still owns the attempt. A controller that crashed with a
// tool open leaves the row Running on the same attempt, suspension and all,
// until something reclaims it; status must not keep reporting a suspension for
// a dead process, and says "unverified" when liveness cannot be decided.
func TestARecordedSuspensionIsActiveOnlyForItsLiveOwner(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		if _, err := f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity,
			ProviderProgress{Key: "1:2", Suspended: true}); err != nil {
			t.Fatal(err)
		}
		f.clock.advance(25 * time.Minute)
		state := func() OperationStatus { return liveStatus(t, f, runID) }

		restore := ownerLiveness(f, true)
		if got := state(); got.InactivitySuspension != "active" {
			t.Fatalf("a live owner's open tool is %q, want active", got.InactivitySuspension)
		}
		restore()

		// The controller died: the lock evidence proves its owner gone, and
		// nothing has reclaimed the row yet.
		restore = ownerLiveness(f, false)
		got := state()
		if got.InactivitySuspension != "" || got.InactivitySuspendedSince != nil || got.SilentFor < 25*time.Minute {
			t.Fatalf("a dead owner's suspension was reported: %q since %v, silent %s",
				got.InactivitySuspension, got.InactivitySuspendedSince, got.SilentFor)
		}
		restore()

		// Evidence that cannot decide - here an owner identity no liveness
		// probe can parse - is unverified, never active.
		previous := f.runtime.scheduler.Liveness
		f.runtime.scheduler.Liveness = ProcessOwnerLiveness{Host: ownerHost()}
		if got := state(); got.InactivitySuspension != "unverified" || got.InactivitySuspendedSince == nil {
			t.Fatalf("undecidable liveness reported %q", got.InactivitySuspension)
		}
		f.runtime.scheduler.Liveness = previous

		// A takeover or a reclaim ends it too, whatever the old owner's state.
		defer ownerLiveness(f, true)()
		for name, edit := range map[string]func(*RunOperation){
			"takeover": func(op *RunOperation) { op.Lease.Owner = "owner-2" },
			"reclaim":  func(op *RunOperation) { op.Lease = nil },
		} {
			restoreRow := rewriteRow(t, f, journal.ID, edit)
			if got := state(); got.InactivitySuspension != "" {
				t.Fatalf("%s: the previous owner's suspension is still %q", name, got.InactivitySuspension)
			}
			restoreRow()
		}
	})
}

// C (hanging write). A durable write that never returns cannot hold the
// invocation past its bound: closing the recorder waits at most one record
// interval on a completed process, and not at all once the process's context
// has ended. The late write stays bound to its attempt, so landing after the
// operation settled changes nothing.
func TestAStuckDurableWriteCannotHoldTheInvocation(t *testing.T) {
	stuck := func(t *testing.T, then func(ProviderProgress)) (func(ProviderProgress), func(), *atomic.Bool) {
		gate, blocked := make(chan struct{}), &atomic.Bool{}
		var once sync.Once
		release := func() { once.Do(func() { close(gate) }) }
		t.Cleanup(release)
		return func(p ProviderProgress) {
			blocked.Store(true)
			<-gate
			blocked.Store(false)
			then(p)
		}, release, blocked
	}
	run := func(t *testing.T, provider CLIAgentProvider, ctx context.Context, request ExecutionRequest, within time.Duration) ExecutionResult {
		t.Helper()
		done := make(chan ExecutionResult, 1)
		started := time.Now()
		go func() {
			result, _ := provider.Execute(ctx, request)
			done <- result
		}()
		select {
		case result := <-done:
			if elapsed := time.Since(started); elapsed > within {
				t.Fatalf("the invocation returned after %s, past its %s bound", elapsed, within)
			}
			return result
		case <-time.After(10 * time.Second):
			t.Fatal("a stuck durable write held the invocation")
			return ExecutionResult{}
		}
	}

	t.Run("completed process", func(t *testing.T) {
		scheduler := liveScheduler()
		op := plannedExecution(t, scheduler, time.Hour)
		record, release, blocked := stuck(t, func(p ProviderProgress) {
			_, _ = scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, p)
		})
		provider, request := claudeProcess(t, emit(claudeAssistant("M1", "", claudeToolUse("X")))+"sleep 0.3\n"+
			emit(claudeToolResult("X", ""), claudeResult(false, "success", 0)))
		ctx := withProviderProgressRecorder(context.Background(), record)
		result := run(t, provider, ctx, request, 300*time.Millisecond+progressRecordInterval(inactivityWindow)+time.Second)
		if result.Outcome != Succeeded || !blocked.Load() {
			t.Fatalf("outcome %q, write blocked %t", result.Outcome, blocked.Load())
		}
		// The attempt settles; the stuck write - a suspension - lands after.
		if _, err := scheduler.Finish(op.ID, Succeeded); err != nil {
			t.Fatal(err)
		}
		settled := row(t, scheduler, op.ID)
		release()
		for deadline := time.Now().Add(2 * time.Second); blocked.Load(); time.Sleep(2 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the released write never returned")
			}
		}
		time.Sleep(50 * time.Millisecond)
		if after := row(t, scheduler, op.ID); after.InactivitySuspension != nil || after.ProgressRecorderOpen ||
			after.NoProgressKey != settled.NoProgressKey {
			t.Fatalf("a late write changed the settled attempt: %+v", after)
		}
	})

	t.Run("attempt deadline", func(t *testing.T) {
		record, _, blocked := stuck(t, func(ProviderProgress) {})
		provider, request, _ := inactivityFixture(t, "while :; do echo working; sleep 0.05; done\n")
		request.Budgets.InactivityLimit = 4 * time.Second // a one-second interval the bound must not wait out
		request.Budgets.WallLimit = 500 * time.Millisecond
		ctx := withProviderProgressRecorder(context.Background(), record)
		result := run(t, provider, ctx, request, 500*time.Millisecond+provider.Grace+600*time.Millisecond)
		if result.Invocation == nil || result.Invocation.TerminationCause != TerminationDeadlineReached || !blocked.Load() {
			t.Fatalf("provenance %+v, write blocked %t", result.Invocation, blocked.Load())
		}
	})
}
