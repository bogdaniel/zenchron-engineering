package runtime

// A provider subprocess being alive is not evidence of progress.
//
// Before this file existed the only thing bounding a live coding CLI was the
// operation's execution deadline, which is derived from the run's whole wall
// budget. A laptop that lost its network therefore kept a Codex process alive
// and silent while the runtime charged every minute of it to active
// engineering work: run-07b3a0390329eb86beb9bdf3d0ed6342 consumed 8h55m16s of
// active budget, zero external wait, and died of run_wall_budget_exhausted
// having discovered nothing about its dead provider. The outer wall budget was
// acting as the stall detector.
//
// What this adds is a THIRD bound, beside the run's active-work budget and the
// operation's total wall bound: a provider invocation may not go longer than
// the configured interval without producing observable output. It is not a
// second scheduler and not a second accounting system - the clock it stops is
// the same process-group cancellation the deadline already uses, and the
// budget it comes from is persisted beside its neighbours in RunBudgets.
//
// WHAT COUNTS AS PROGRESS, and deliberately what does not:
//
//   - bytes arriving on the child's stdout or stderr: progress, for the
//     byte_output adapters (Codex, Gemini, Qwen). Claude Code is supervised by
//     its structured stream-json events instead, and its raw bytes are not
//     progress at all; see claude_stream.go (#322).
//   - the child process existing: NOT progress. That is the defect.
//   - a scheduler lease heartbeat: NOT progress. It proves a controller is
//     alive, which is a different claim from the work advancing.
//   - a clock tick: NOT progress, obviously, and naming it here is the point:
//     the previous no-progress scaffolding in this package was never wired to
//     anything, so "no progress" had no observation behind it at all.
//
// Output is a conservative signal rather than a complete one. A CLI can
// legitimately think in silence, which is exactly why the bound is a
// configurable window of minutes rather than an immediate failure, and why
// reaching it routes to a bounded retry rather than terminalizing the run.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrProviderInactive is the cause a cancelled provider context carries when
// the inactivity policy - not the deadline, not a shutdown, not the operator -
// is what ended the invocation.
//
// It is a CAUSE rather than a return value because the thing that observes the
// silence is the process runner, several frames below the adapter that has to
// classify it, and context.Cause is the one channel that already crosses that
// boundary. Without it an inactivity kill was indistinguishable from a
// supervisor shutdown through ctx.Err() alone, and those two mean opposite
// things to a run: one is a dead provider, the other is a resumable pause.
var ErrProviderInactive = errors.New("the provider produced no output within its inactivity bound")

// DefaultProviderInactivitySeconds is the shipped no-progress window.
//
// It is ten minutes, and it is derived from this repository's own conventions
// rather than picked:
//
//   - the documented operator wall limit is 1800s (docs/configuration.md), so
//     600s bites at a third of the budget it is protecting instead of after
//     it;
//   - 600s is already this repository's stated bound for "a bounded operation
//     that has produced nothing is stuck" - it is the Go test timeout the
//     suite runs under, and the same 600s hang appears in the #238 evidence;
//   - it is ten times the 60s default watch interval, so an ordinary
//     supervisor pass cannot be mistaken for silence;
//   - it is long enough for a coding CLI to reason, plan, or run a slow test
//     inside one invocation without being killed for thinking.
//
// An operator tightens it; a repository may tighten it further. It is finite
// on purpose and there is no value that disables it: an unattended CLI worker
// with no inactivity bound is the exact configuration #238 was filed about.
const DefaultProviderInactivitySeconds = 600

// inactivityPolicy is the per-invocation no-progress bound, carried on the
// context because that is where Go already carries the deadline it sits beside.
//
// It holds the cancel FUNCTION rather than being consulted by a caller: the
// observation happens where output lands, which is inside the process runner,
// and the decision has to reach the process group from there.
type inactivityPolicy struct {
	limit  time.Duration
	cancel context.CancelCauseFunc
	// record makes observed progress DURABLE, so status can say how long a
	// live invocation has actually been silent instead of reporting the whole
	// attempt as silence, and so a controller that dies mid-invocation leaves
	// behind when progress was last seen. It is nil where the caller keeps no
	// durable operation state - a probe, a doctor check, a test.
	record func(ProviderProgress)
}

type inactivityPolicyKey struct{}

// withProviderInactivity binds a no-progress bound to ctx and returns the
// derived context plus the release that detaches the cause.
//
// A zero or negative limit binds nothing and returns ctx unchanged, which is
// what a run persisted before this budget existed gets: the previous
// behaviour, exactly, rather than a default invented at read time.
func withProviderInactivity(ctx context.Context, limit time.Duration, record func(ProviderProgress)) (context.Context, func()) {
	if limit <= 0 {
		return ctx, func() {}
	}
	bounded, cancel := context.WithCancelCause(ctx)
	policy := &inactivityPolicy{limit: limit, cancel: cancel, record: record}
	return context.WithValue(bounded, inactivityPolicyKey{}, policy), func() { cancel(nil) }
}

type progressRecorderKey struct{}

// withProviderProgressRecorder supplies the DURABLE half of the policy: where
// observed progress is written so that status can report it and a restart can
// read it back.
//
// It is separate from the bound itself because the two come from different
// places. The bound is a budget and travels with the request, like every other
// budget; the recorder is the caller's own durable operation state, which an
// adapter must not know the shape of. A caller that keeps none - the planner,
// a probe, a test - supplies none, and the bound still applies.
func withProviderProgressRecorder(ctx context.Context, record func(ProviderProgress)) context.Context {
	if record == nil {
		return ctx
	}
	return context.WithValue(ctx, progressRecorderKey{}, record)
}

// providerProgressRecorder returns the caller's durable progress recorder, or
// nil when none was supplied.
func providerProgressRecorder(ctx context.Context) func(ProviderProgress) {
	record, _ := ctx.Value(progressRecorderKey{}).(func(ProviderProgress))
	return record
}

// providerInactivityCause reports whether this context was ended by the
// inactivity policy. It is the adapter's question, and it is asked of the
// CAUSE so that a deadline, a shutdown and a stall stay three answers.
func providerInactivityCause(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrProviderInactive)
}

// providerInactivityLimit reports the bound in force for ctx, or zero.
func providerInactivityLimit(ctx context.Context) time.Duration {
	if policy, ok := ctx.Value(inactivityPolicyKey{}).(*inactivityPolicy); ok {
		return policy.limit
	}
	return 0
}

// inactivityWatch is ONE process's silence clock.
//
// It is per-process rather than per-policy because a capability probe and the
// invocation it precedes are different processes, and a probe's output is not
// evidence that the invocation is moving. The cancel and the durable recorder
// are shared; the timing is not.
type inactivityWatch struct {
	policy *inactivityPolicy
	// start is the monotonic origin, so every duration below survives a
	// wall-clock adjustment. last is nanoseconds since start.
	start time.Time
	last  atomic.Int64
	bytes atomic.Int64

	// quit is closed once the process is done; stopped is closed by watch as
	// it returns, so the stop returned by watchUntilComplete can JOIN the
	// watcher rather than merely signalling it.
	quit      chan struct{}
	stopped   chan struct{}
	closeQuit sync.Once

	// stream is the structured progress oracle, when the provider has one
	// (#322). It is set before the watcher starts and is only read after.
	stream *claudeStream

	mu sync.Mutex
	// finished records that the PROCESS COMPLETED FIRST. It is read and
	// written under mu, on the same side of the same lock as the decision to
	// cancel, so completion and expiry can never both win.
	finished bool

	// recorder makes this process's progress durable (#352). It is nil when
	// the caller keeps no durable state. ended is the process context's Done -
	// cancellation, the attempt deadline or the inactivity kill - which ends
	// any wait on it.
	recorder *progressRecorder
	ended    <-chan struct{}
}

// observation is one accepted progress state as the process observed it.
// Instants carry the monotonic clock; ages are taken only at write time.
type observation struct {
	key string
	at  time.Time
	// suspendedSince is when a structured main-thread tool opened, or zero.
	suspendedSince time.Time
}

// ProviderProgress is one durable observation of a live provider invocation,
// as Scheduler.RecordProviderProgress receives it.
type ProviderProgress struct {
	// Key is the progress fingerprint; the durable instant moves only when it
	// changes.
	Key string
	// Age is how long before this write the progress was OBSERVED. The row
	// must say when the work moved, not when the coalescer wrote it (#352).
	Age time.Duration
	// Suspended reports a structured main-thread tool held open (#322), and
	// SuspendedAge how long before this write it opened.
	Suspended    bool
	SuspendedAge time.Duration
	// Final is the recorder's closing write: the process has ended under an
	// observing controller, so nothing it observed is still unwritten.
	Final bool
}

// progressRecordInterval is how often one process may write durable progress:
// a quarter of the window. A chatty provider must not turn its own output into
// a write storm on the operation row.
func progressRecordInterval(limit time.Duration) time.Duration { return limit / 4 }

// progressRecorderLag is L, the most LastProgressAt can trail the last
// progress a process whose recorder never closed actually made. It is what a
// crash can hide, so it is the most ProviderInactivityRemaining ever gives back.
//
// It is TWO intervals, not one, and no recorder that keeps the write rate and
// eventually persists the newest observation at its true instant can do
// better: progress observed just after a write is written one interval later
// carrying its own, older instant, and progress observed just before the next
// write is due is lost by a crash - almost two intervals apart. Store write
// latency and timer delay add to it, are not bounded here, and only ever push
// the error towards refusing, never towards granting.
func progressRecorderLag(limit time.Duration) time.Duration {
	return 2 * progressRecordInterval(limit)
}

// armInactivityWatch returns the watch bound to ctx, or nil when no bound
// applies. A nil watch's methods are no-ops, so the process runner needs no
// branch of its own.
func armInactivityWatch(ctx context.Context) *inactivityWatch {
	policy, ok := ctx.Value(inactivityPolicyKey{}).(*inactivityPolicy)
	if !ok || policy.limit <= 0 {
		return nil
	}
	w := &inactivityWatch{
		policy: policy, start: time.Now(),
		quit: make(chan struct{}), stopped: make(chan struct{}), ended: ctx.Done(),
	}
	if policy.record != nil {
		w.recorder = newProgressRecorder(policy.record, progressRecordInterval(policy.limit))
	}
	return w
}

// progress records n observed output bytes. It is called on the copy
// goroutines os/exec owns, so it never blocks on the durable write: that is
// handed to the coalescing recorder.
func (w *inactivityWatch) progress(n int) {
	if w == nil || n <= 0 {
		return
	}
	w.touch()
	// The KEY is the cumulative byte count, so the durable record advances
	// only when the provider actually said something new. That is the same
	// rule the scheduler's progress fingerprint already applies, and it is
	// what keeps "progress" from meaning "we asked again".
	w.record(strconv.FormatInt(w.bytes.Add(int64(n)), 10), time.Now(), time.Time{})
}

// touch refreshes the in-memory window. It is what every recognized progress
// signal does, on every signal.
func (w *inactivityWatch) touch() {
	if w == nil {
		return
	}
	w.last.Store(int64(time.Since(w.start)))
}

// record hands one accepted observation to the durable recorder. It never
// blocks on the store.
func (w *inactivityWatch) record(key string, at, suspendedSince time.Time) {
	if w == nil || w.recorder == nil {
		return
	}
	w.recorder.observe(observation{key: key, at: at, suspendedSince: suspendedSince})
}

// progressRecorder COALESCES one process's accepted observations into the
// durable row (#352). The previous shape dropped any observation that arrived
// before the next write was due, so a burst followed by quiet left the row at
// the burst's first event.
//
// One goroutine owns every write, so writes are serialized and land in the
// order they were observed. The first observation is written at once; one
// arriving inside the interval REPLACES whatever is pending - newest wins,
// nothing newer is ever dropped - and is written when the interval since the
// previous write STARTED has passed, carrying the instant it was observed. So
// write starts stay at least one interval apart: the maximum rate is unchanged.
type progressRecorder struct {
	write    func(ProviderProgress)
	interval time.Duration

	mu      sync.Mutex
	pending *observation
	closed  bool

	wake      chan struct{} // one buffered token: something is pending
	closing   chan struct{} // closed once, by close
	closeOnce sync.Once
	done      chan struct{} // closed when the writer returns
}

func newProgressRecorder(write func(ProviderProgress), interval time.Duration) *progressRecorder {
	r := &progressRecorder{
		write: write, interval: interval,
		wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *progressRecorder) observe(o observation) {
	r.mu.Lock()
	// Two copy goroutines can race here; the older never replaces the newer.
	if r.closed || (r.pending != nil && o.at.Before(r.pending.at)) {
		r.mu.Unlock()
		return
	}
	r.pending = &o
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *progressRecorder) take() *observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.pending
	r.pending = nil
	return next
}

// run is the recorder's only writer. Its last write is the CLOSING one: the
// process ended under an observing controller, so its newest observation is
// written at once, with no tool open and Final set, which tells a successor
// there is no unwritten tail to allow for. That is at most one write beyond the
// rate, once per process.
func (r *progressRecorder) run() {
	defer close(r.done)
	var last *observation
	var lastStart time.Time
	for {
		select {
		case <-r.wake:
		case <-r.closing:
		}
		closing := false
		select {
		case <-r.closing:
			closing = true
		default:
			if wait := time.Until(lastStart.Add(r.interval)); !lastStart.IsZero() && wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-r.closing:
					timer.Stop()
					closing = true
				}
			}
		}
		next := r.take()
		if closing {
			if next == nil {
				next = last
			}
			if next != nil {
				final := *next
				final.suspendedSince = time.Time{}
				r.write(final.progress(true))
			}
			return
		}
		if next != nil {
			lastStart, last = time.Now(), next
			r.write(next.progress(false))
		}
	}
}

// close ends the recorder and waits for its closing write - BOUNDED. A durable
// write that does not return must not hold the invocation: the wait ends at
// the first of the writer finishing, the process context ending (cancelled,
// its deadline passed, or the inactivity kill), or one record interval, which
// a healthy write never approaches. A write still in flight then lands late or
// never. Either way it is bound to this operation and physical attempt, so it
// cannot touch a successor, and a row it never reached still says its recorder
// is open, which is the crash rule's case: ProviderInactivityRemaining allows
// at most progressRecorderLag for it.
func (r *progressRecorder) close(cancelled <-chan struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.closeOnce.Do(func() { close(r.closing) })
	bound := time.NewTimer(r.interval)
	defer bound.Stop()
	select {
	case <-r.done:
	case <-cancelled:
	case <-bound.C:
	}
}

// progress is the observation as the store receives it, aged at write time.
func (o observation) progress(final bool) ProviderProgress {
	p := ProviderProgress{Key: o.key, Age: time.Since(o.at), Final: final}
	if !o.suspendedSince.IsZero() {
		p.Suspended, p.SuspendedAge = true, time.Since(o.suspendedSince)
	}
	return p
}

// silent reports how long it has been since output arrived.
func (w *inactivityWatch) silent() time.Duration {
	return time.Since(w.start) - time.Duration(w.last.Load())
}

// complete records that the PROCESS FINISHED FIRST.
//
// It is the whole of the race fix, and it is one flag set under the same mutex
// the cancellation is taken under - so completion and expiry are totally
// ordered and cannot both win. The previous shape had no such flag: the watcher
// computed its remaining window and cancelled before ever consulting its stop
// channel, so a watcher scheduled after a natural exit cancelled
// unconditionally, and the caller - reading context.Cause a few statements
// later - reported a clean exit as provider_no_progress.
//
// It is idempotent, because the executor's normal path and its failure paths
// must both be able to say "the process is done" without coordinating.
func (w *inactivityWatch) complete() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.finished = true
	w.mu.Unlock()
}

// watchUntilComplete starts the watcher and returns the stop that JOINS it.
//
// The join is the property the caller depends on. Signalling without waiting
// leaves the watcher free to publish a cancellation after the executor has
// returned, and `select` chooses uniformly when both of its arms are ready - so
// a fire-and-forget close is not enough on its own, however the loop is
// written. Once the returned function has returned, no cancellation can be
// published, which is what makes classifying context.Cause afterwards sound.
//
// A nil watch hands back a no-op, so the process runner needs no branch.
func (w *inactivityWatch) watchUntilComplete() func() {
	if w == nil {
		return func() {}
	}
	go w.watch()
	return func() {
		w.complete()
		w.closeQuit.Do(func() { close(w.quit) })
		<-w.stopped
		w.recorder.close(w.ended)
	}
}

// expire cancels the invocation unless the process already completed, and
// reports whether the watcher is done.
//
// The check and the cancel are ONE critical section. Checking outside it would
// reintroduce the race in a smaller window rather than removing it: stop could
// set finished between the check and the cancel, and the caller would then be
// classifying a cause that was published after completion was recorded.
//
// It declines, and the watcher looks again, when a structured stream holds a
// main-thread tool open (#322: a long `go test` is not a stall; the absolute
// deadline still bounds it) or when progress landed after the watcher measured.
// The open-tool check comes FIRST: the stream closes a tool and refreshes the
// window in one step under its own lock, so once it reports nothing open,
// silent() already reflects the refresh that closed it.
func (w *inactivityWatch) expire() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return true
	}
	if w.stream.holdsOpenTool() || w.silent() < w.policy.limit {
		return false
	}
	w.policy.cancel(ErrProviderInactive)
	return true
}

// watch cancels the invocation's context, with ErrProviderInactive as the
// cause, once the window passes with no output. It returns when stop is called,
// and closing `stopped` on the way out is what lets stop join it.
//
// It is a recomputing loop rather than a timer that is Reset on every write:
// Reset from two copy goroutines per byte of output is a lock convoy on the
// hot path of a noisy provider, and the loop re-arms at most once per silent
// window.
func (w *inactivityWatch) watch() {
	if w == nil {
		return
	}
	defer close(w.stopped)
	for {
		// STOP IS CHECKED BEFORE EXPIRY, and expiry re-checks it under the
		// lock. A watcher that reached its window at the same instant the
		// process exited must not cancel: this arm is the fast path for that,
		// and expire is the one that makes it airtight.
		select {
		case <-w.quit:
			return
		default:
		}
		remaining := w.policy.limit - w.silent()
		if remaining <= 0 {
			if w.expire() {
				return
			}
			// Held by an open tool, or refreshed meanwhile: measure again.
			if remaining = w.policy.limit - w.silent(); remaining <= 0 {
				remaining = w.policy.limit
			}
		}
		timer := time.NewTimer(remaining)
		select {
		case <-w.quit:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ProviderInactivityRemaining is the no-progress authority left for the next
// invocation of op: the run's persisted window less the silence already
// durably recorded against the operation.
//
// It is derived from durable facts - the run's budget document and the
// operation row - rather than from an in-memory stopwatch, so a restart
// reconstructs the same number the controller that died would have used. A
// window that a restart recomputed from a fresh default would be the original
// defect in a narrower place: silence would become free every time the
// supervisor bounced.
//
// It never returns more than the configured limit and never less than zero. A
// zero limit means no bound was configured, and returns zero: absence stays
// absence rather than resolving to a default here.
//
// THE CRASH GAP (#352). Durable progress is coalesced, so a controller that
// died with an observation still pending left LastProgressAt behind the truth,
// by less than L = progressRecorderLag (half the window), and only when
// ProgressRecorderOpen says the dead process's recorder never closed. The
// silence charged is then the recorded silence less L, floored at zero. So a
// successor is never refused on the recorder's own lag, and is granted at most
// L beyond what the recorded silence leaves - never a fresh window. A closed
// recorder carries an exact datum and gets no allowance.
//
// The allowance cannot ACCUMULATE across repeated crashes. It is a pure
// function of the durable datum, which only a real observed write moves - a
// restart never does: StartWithin keeps an abandoned attempt's LastProgressAt
// and flag as they are. Every successor dispatched against the same datum
// therefore gets the same absolute bound, LastProgressAt + limit + L, however
// many times the controller dies before it.
func ProviderInactivityRemaining(limit time.Duration, op RunOperation, now time.Time) time.Duration {
	if limit <= 0 {
		return 0
	}
	if op.LastProgressAt == nil || now.Before(*op.LastProgressAt) {
		return limit
	}
	silence := now.Sub(*op.LastProgressAt)
	if op.ProgressRecorderOpen {
		silence = max(silence-progressRecorderLag(limit), 0)
	}
	if remaining := limit - silence; remaining > 0 {
		return remaining
	}
	return 0
}

// perAttemptInactivityProvider is implemented by a provider whose inactivity
// window belongs to ONE physical process rather than to the operation.
type perAttemptInactivityProvider interface{ inactivityPerAttempt() bool }

// dispatchInactivityWindow is the no-progress window the next physical
// invocation of op is dispatched with.
//
// For a byte_output provider it is ProviderInactivityRemaining, unchanged: an
// abandoned attempt's silence carries into its successor, so a bouncing
// supervisor cannot make silence free.
//
// A structured_claude_events provider gets the full window per physical attempt
// (#322). Its durable progress is throttled evidence about a dead process's
// event stream, and misreading that as silence of the NEW process refused a
// valid attempt before Claude even started. Repeated succession is still
// finite: the orphaned interval stays charged to the wall budget, the attempt
// identity advances, and neither is ever refunded.
func dispatchInactivityWindow(limit time.Duration, op RunOperation, now time.Time, provider ExecutionProvider) time.Duration {
	if perAttempt, ok := provider.(perAttemptInactivityProvider); ok && limit > 0 && perAttempt.inactivityPerAttempt() {
		return limit
	}
	return ProviderInactivityRemaining(limit, op, now)
}

// ProviderSilence is how long op has gone without recognized progress, or zero
// when nothing has been recorded. It is observation for status, never a bound.
func ProviderSilence(op RunOperation, now time.Time) time.Duration {
	if op.LastProgressAt == nil || now.Before(*op.LastProgressAt) {
		return 0
	}
	return now.Sub(*op.LastProgressAt)
}
