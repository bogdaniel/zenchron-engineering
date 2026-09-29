//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #355: a structured-tool suspension EDGE - a main-thread tool opening or
// closing - is not ordinary coalescible progress. The durable row, and so
// status, follows each edge within the edge spacing instead of waiting out the
// ordinary interval, and no older write can put back a suspension a newer
// observation ended.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// edgeStream is one structured process's stream and watch, started, with the
// watch's context and its joining stop.
func edgeStream(limit time.Duration, record func(ProviderProgress)) (*claudeStream, context.Context, func()) {
	ctx, release := withProviderInactivity(context.Background(), limit, record)
	watch := armInactivityWatch(ctx)
	stream := newClaudeStream(1)
	stream.attach(watch)
	stop := watch.watchUntilComplete()
	return stream, ctx, func() { stop(); release() }
}

// eventually fails unless cond holds within d, and reports how long it took.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	started := time.Now()
	for !cond() {
		if time.Since(started) > d {
			t.Fatalf("%s: not within %s", what, d)
		}
		time.Sleep(time.Millisecond)
	}
	return time.Since(started)
}

func statusSuspension(t *testing.T, f *phase8Fixture, runID string) string {
	t.Helper()
	return liveStatus(t, f, runID).InactivitySuspension
}

// 1. A SHORT TOOL CLEARS AT ONCE. The tool opens and closes inside one
// ordinary interval; the durable row - and status, which reads it - stops
// claiming a suspension within the edge spacing, not at the next ordinary
// slot. This is the dogfood F6 shape: the open was written at once, the close
// waited up to an interval behind it.
func TestAShortToolClearsItsSuspensionWithoutWaitingForTheSlot(t *testing.T) {
	const limit = 4 * time.Second // a one-second interval, a ~17ms edge spacing
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		defer ownerLiveness(f, true)()
		stream, _, stop := edgeStream(limit, func(p ProviderProgress) {
			_, _ = f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity, p)
		})
		defer stop()
		feed(stream, claudeAssistant("M1", "", claudeToolUse("A")))
		eventually(t, time.Second, "the open tool reached status", func() bool { return statusSuspension(t, f, runID) == "active" })
		time.Sleep(20 * time.Millisecond)
		feed(stream, claudeToolResult("A", ""))
		took := eventually(t, progressRecordInterval(limit)/4, "status stopped claiming the closed tool's suspension",
			func() bool { return statusSuspension(t, f, runID) == "" })
		t.Logf("suspension cleared %s after the tool closed", took)
		if r := row(t, f.runtime.scheduler, journal.ID); r.InactivitySuspension != nil || r.NoProgressKey != "1:2" {
			t.Fatalf("durable row after the close: %+v", r)
		}
	})
}

// longTool holds one structured tool open for three windows of tool_progress,
// sampling status throughout, and returns once the tool is closed and status
// says so. With precede, an ordinary write has just started when the tool
// opens, so the OPEN edge is the one that must not wait for the slot.
func longTool(t *testing.T, precede bool) {
	const limit = 800 * time.Millisecond // a 200ms interval, a ~3ms edge spacing
	const prompt = 100 * time.Millisecond
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		defer ownerLiveness(f, true)()
		stream, ctx, stop := edgeStream(limit, func(p ProviderProgress) {
			_, _ = f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity, p)
		})
		defer stop()
		if precede {
			feed(stream, claudeAssistant("M0", "", claudeText))
			eventually(t, time.Second, "the ordinary write", func() bool {
				return row(t, f.runtime.scheduler, journal.ID).NoProgressKey == "1:1"
			})
		}
		feed(stream, claudeAssistant("M1", "", claudeToolUse("X")))
		eventually(t, prompt, "status reported the open tool", func() bool { return statusSuspension(t, f, runID) == "active" })
		for held := time.Now(); time.Since(held) < 3*limit; time.Sleep(20 * time.Millisecond) {
			feed(stream, claudeToolProgress("X"))
			if got := liveStatus(t, f, runID); got.InactivitySuspension != "active" || got.InactivitySuspendedSince == nil {
				t.Fatalf("%s into a healthy open tool, status is %q: it reads as silence approaching the kill",
					time.Since(held), got.InactivitySuspension)
			}
			if ctx.Err() != nil {
				t.Fatalf("the watchdog ended a healthy open tool: %v", context.Cause(ctx))
			}
		}
		feed(stream, claudeToolResult("X", ""))
		eventually(t, prompt, "status cleared the closed tool", func() bool { return statusSuspension(t, f, runID) == "" })
	})
}

// 2. A LONG TOOL is suspended for as long as it is open - across three windows
// - and the suspension clears promptly on close.
func TestALongToolIsSuspendedThroughoutAndClearsOnClose(t *testing.T) { longTool(t, false) }

// 6. A1 (#352) STAYS FIXED, on the edge path: a long tool opening just after
// an ordinary write is reported suspended at once rather than as growing
// silence, never approaches the kill, and the watchdog does not expire it.
func TestAHealthyLongToolOpenedMidSlotIsNeverShownApproachingTheKill(t *testing.T) {
	longTool(t, true)
}

// 3. BACK-TO-BACK TOOLS. Tool A closes and tool B opens. Whether the two
// events share one pipe read or not, no write after A closed carries A's
// suspension, and the row settles on B's, since B's own open instant. A brief
// unsuspended write between them is acceptable only because it is true: the
// watchdog was unsuspended between the two events.
func TestBackToBackToolsNeverShowTheStaleTool(t *testing.T) {
	const limit = 4 * time.Second
	const skew = 5 * time.Millisecond
	for name, together := range map[string]bool{"one read": true, "two reads": false} {
		t.Run(name, func(t *testing.T) {
			scheduler := liveScheduler()
			op := plannedExecution(t, scheduler, time.Hour)
			log := &progressLog{}
			stream, _, stop := edgeStream(limit, boundRecorder(scheduler, op, log))
			defer stop()
			aOpened := time.Now()
			feed(stream, claudeAssistant("M1", "", claudeToolUse("A")))
			log.waitFor(t, 1)
			time.Sleep(30 * time.Millisecond)
			aClosed := time.Now()
			if together {
				_, _ = stream.Write([]byte(claudeToolResult("A", "") + "\n" + claudeAssistant("M2", "", claudeToolUse("B")) + "\n"))
			} else {
				feed(stream, claudeToolResult("A", ""))
				time.Sleep(time.Millisecond)
				feed(stream, claudeAssistant("M2", "", claudeToolUse("B")))
			}
			bOpened := time.Now()
			eventually(t, progressRecordInterval(limit)/4, "the row carries B's suspension", func() bool {
				s := row(t, scheduler, op.ID).InactivitySuspension
				return s != nil && !s.Since.Before(aClosed)
			})
			if s := row(t, scheduler, op.ID).InactivitySuspension; s.Since.Before(aClosed) || s.Since.After(bOpened.Add(skew)) {
				t.Fatalf("suspended since %s: want B's open instant, in [%s, %s]", s.Since, aClosed, bOpened)
			}
			for _, w := range log.snapshot() {
				if w.wroteAt.After(bOpened) && w.Suspended {
					since := w.wroteAt.Add(-w.SuspendedAge)
					if since.Sub(aOpened) < 25*time.Millisecond {
						t.Fatalf("a write after A closed carries A's suspension (since %s, A opened %s)", since, aOpened)
					}
				}
			}
		})
	}
}

// 4. NO STALE RESURRECTION. A write carrying the OPEN state is still in the
// store when the tool closes. It lands - it is older, and it lands first - and
// the close follows it promptly: no write after the close carries a
// suspension, and the row ends unsuspended within the edge spacing of the
// stuck write returning, not an interval after it started.
func TestAnOlderQueuedWriteCannotRestoreAnEndedSuspension(t *testing.T) {
	const limit = 4 * time.Second
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var blocked atomic.Bool
	var mu sync.Mutex
	var order []bool // Suspended, in the order the store applied the writes
	record := func(p ProviderProgress) {
		if p.Suspended && !blocked.Swap(true) {
			<-gate // the first open-state write is stuck in the store
		}
		_, _ = scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, p)
		mu.Lock()
		order = append(order, p.Suspended)
		mu.Unlock()
	}
	stream, _, stop := edgeStream(limit, record)
	defer stop()
	feed(stream, claudeAssistant("M1", "", claudeToolUse("X")))
	eventually(t, time.Second, "the open write is in flight", blocked.Load)
	// While it is stuck the tool reports progress, then closes: both pending,
	// the newest (closed) replacing the older.
	feed(stream, claudeToolProgress("X"), claudeToolResult("X", ""))
	time.Sleep(50 * time.Millisecond)
	release()
	released := time.Now()
	eventually(t, progressRecordInterval(limit)/4, "the close followed the stuck write", func() bool {
		return row(t, scheduler, op.ID).InactivitySuspension == nil && row(t, scheduler, op.ID).NoProgressKey == "1:2"
	})
	t.Logf("row unsuspended %s after the stuck write was released", time.Since(released))
	time.Sleep(progressRecordInterval(limit) + 100*time.Millisecond) // anything still queued has landed
	if r := row(t, scheduler, op.ID); r.InactivitySuspension != nil {
		t.Fatalf("an older write restored the ended suspension: %+v", r.InactivitySuspension)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || !order[0] || order[len(order)-1] {
		t.Fatalf("writes applied as %v: want the stuck open first and the close last", order)
	}
	for i, suspended := range order[1:] {
		if suspended {
			t.Fatalf("write %d after the stuck one carries a suspension: %v", i+1, order)
		}
	}
}

// 5. CONTINUATION BOUNDARY. An attempt checkpoints with a tool open and a
// write still in the store; its operation settles and a continuation
// operation starts. Nothing of the old attempt - neither its stuck write nor
// its recorder's closing write - puts a suspension on either row, and the
// continuation's own row starts without one.
func TestNoSuspensionLeaksAcrossAContinuation(t *testing.T) {
	const limit = 4 * time.Second
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var blocked atomic.Bool
	stream, _, stop := edgeStream(limit, func(p ProviderProgress) {
		if p.Suspended {
			blocked.Store(true)
			<-gate
		}
		_, _ = scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, p)
	})
	feed(stream, claudeAssistant("M1", "", claudeToolUse("X")))
	eventually(t, time.Second, "the open write is in flight", blocked.Load)
	// The attempt checkpoints: its operation settles, the run waits, and the
	// continuation is planned and started.
	if _, err := scheduler.Finish(op.ID, Succeeded); err != nil {
		t.Fatal(err)
	}
	continuation, _, err := scheduler.Plan(RunOperation{
		RunID: op.RunID, Kind: OpExecutionInvoke, IdempotencyKey: "continuation|checkpoint",
		MaxAttempts: 3, WallBudget: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	if continuation, err = scheduler.Start(continuation.ID); err != nil {
		t.Fatal(err)
	}
	release()
	stop()
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{op.ID, continuation.ID} {
		if r := row(t, scheduler, id); r.InactivitySuspension != nil {
			t.Fatalf("row %s carries a suspension across the continuation: %+v", id, r.InactivitySuspension)
		}
	}
	if r := row(t, scheduler, continuation.ID); r.NoProgressKey != continuation.NoProgressKey || r.ProgressRecorderOpen {
		t.Fatalf("the checkpointed attempt wrote into its continuation: %+v", r)
	}
}

// TOOL STORM. Many tiny tools open and close as fast as the stream can carry
// them. Edge writes are bounded - every write starts at least the edge spacing
// after the previous one - and the FINAL state is never dropped: once the
// storm ends with no tool open, the row says so within the edge spacing.
func TestAToolStormIsRateBoundAndEndsOnItsFinalState(t *testing.T) {
	const limit = 4 * time.Second
	edge := progressEdgeSpacing(progressRecordInterval(limit))
	scheduler := liveScheduler()
	op := plannedExecution(t, scheduler, time.Hour)
	log := &progressLog{}
	stream, _, stop := edgeStream(limit, boundRecorder(scheduler, op, log))
	defer stop()
	started := time.Now()
	for i := 0; time.Since(started) < 300*time.Millisecond; i++ {
		id := fmt.Sprint("T", i)
		feed(stream, claudeAssistant(fmt.Sprint("M", i), "", claudeToolUse(id)))
		feed(stream, claudeToolResult(id, ""))
		time.Sleep(200 * time.Microsecond)
	}
	storm := time.Since(started)
	eventually(t, 10*edge+50*time.Millisecond, "the storm's final, unsuspended state reached the row", func() bool {
		return row(t, scheduler, op.ID).InactivitySuspension == nil
	})
	// Progress that did not change the suspension is ordinary: it lands at
	// the next ordinary slot, newest first, never dropped.
	eventually(t, progressRecordInterval(limit)+100*time.Millisecond, "the storm's last progress reached the row", func() bool {
		return row(t, scheduler, op.ID).NoProgressKey == stream.key()
	})
	writes := log.snapshot()
	for i := 1; i < len(writes); i++ {
		if gap := writes[i].wroteAt.Sub(writes[i-1].wroteAt); gap < edge-2*time.Millisecond {
			t.Fatalf("writes %d and %d are %s apart, under the %s edge spacing", i-1, i, gap, edge)
		}
	}
	// One per edge spacing across the storm and the one spacing after it
	// that can still carry its last edge, plus the trailing ordinary write.
	if len(writes) > int(storm/edge)+3 {
		t.Fatalf("%d writes for a %s storm, want at most one per %s edge spacing plus the trailing ordinary one",
			len(writes), storm, edge)
	}
	t.Logf("%d writes for a %s storm (key %s); bound one per %s", len(writes), storm, stream.key(), edge)
}

// key is the stream's current progress key, as the recorder is told it.
func (s *claudeStream) key() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%d:%d", s.attempt, s.accepted)
}
