//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #213 termination ownership, proved against REAL process groups: the real
// bounded process, the real owned pipes, the real grace-then-kill stop
// sequence and the real CLI adapter classification. Exactly one event owns
// how a provider ended - the one that actually ended it, or first initiated
// its ending - committed by the executor when it happened.

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// A provider that is running, and when asked to terminate takes 250ms of grace
// before it exits: every event that lands in that window arrives AFTER the
// owner was committed.
const gracefulProviderScript = "trap 'sleep 0.25; exit 143' TERM\nwhile :; do sleep 0.02; done\n"

// runWithStopAt runs script as a real Codex CLI invocation, committing an
// operator stop on the invocation's parent context after `stop`.
func runWithStopAt(t *testing.T, script string, wall, stop time.Duration) (ExecutionResult, time.Duration) {
	t.Helper()
	provider, request, _ := inactivityFixture(t, script)
	provider.Grace = time.Second
	request.Deadline = nil
	if wall > 0 {
		request.Budgets.WallLimit = wall
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	timer := time.AfterFunc(stop, func() { cancel(errRunStopped) })
	defer timer.Stop()
	started := time.Now()
	result, _ := provider.Execute(ctx, request)
	return result, time.Since(started)
}

func terminationOf(result ExecutionResult) string {
	if result.Invocation == nil {
		return ""
	}
	return result.Invocation.TerminationCause
}

// R1: the attempt wall fires, the executor initiates termination for it, the
// provider is still in its grace period when the stop lands, then it exits.
// The deadline owns the ending.
func TestARealDeadlineKillStillDrainingWhenAStopLandsKeepsTheDeadline(t *testing.T) {
	result, _ := runWithStopAt(t, gracefulProviderScript, 100*time.Millisecond, 200*time.Millisecond)
	if result.Failure == nil || result.Failure.Classification != FailureExecutionIncomplete || result.Outcome == OperationCancelled {
		t.Fatalf("outcome %q failure %#v: a deadline kill was reattributed to a later stop", result.Outcome, result.Failure)
	}
	if got := terminationOf(result); got != TerminationDeadlineReached {
		t.Fatalf("termination = %q, want %q", got, TerminationDeadlineReached)
	}
}

// R2: the provider states a quota and exits 1 by itself; a descendant keeps its
// output pipe open; the stop lands after the provider exited but before the
// adapter returned; the stop's cleanup then closes the pipe. The provider owns
// its ending: provider_quota, provider_returned, never run_stopped.
func TestARealProviderThatExitedWithAQuotaKeepsItWhenAStopLandsLater(t *testing.T) {
	const stop = 150 * time.Millisecond
	result, elapsed := runWithStopAt(t, "sleep 5 &\necho \"You've hit your usage limit.\" >&2\nexit 1\n", 0, stop)
	if elapsed < stop {
		t.Fatalf("the adapter returned after %s, before the stop landed: the pipe was not held", elapsed)
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderQuota || result.Outcome == OperationCancelled {
		t.Fatalf("outcome %q failure %#v: the provider's own exit was replaced by the stop", result.Outcome, result.Failure)
	}
	if got := terminationOf(result); got == TerminationRunStopped || got == TerminationDeadlineReached {
		t.Fatalf("termination = %q: the provider exited by itself", got)
	}
}

// The provider exits 0 by itself while a descendant holds its pipe; the stop
// that then clears the descendant did not end the provider.
func TestARealProviderThatExitedCleanlyIsNotRecordedAsStopped(t *testing.T) {
	result, _ := runWithStopAt(t, "sleep 5 &\necho done\nexit 0\n", 0, 150*time.Millisecond)
	if result.Outcome == OperationCancelled || terminationOf(result) == TerminationRunStopped ||
		(result.Failure != nil && (result.Failure.Classification == FailureRunCancelled || result.Failure.Classification == FailureControllerShutdown)) {
		t.Fatalf("outcome %q termination %q failure %#v: a clean exit was attributed to the stop", result.Outcome, terminationOf(result), result.Failure)
	}
}

// Case 5: a provider genuinely running, with no earlier owner, is ended by the
// stop, and the executor initiated that termination because of it.
func TestARealRunningProviderIsEndedByTheStop(t *testing.T) {
	result, _ := runWithStopAt(t, gracefulProviderScript, 0, 100*time.Millisecond)
	if result.Outcome != OperationCancelled || result.Failure == nil || result.Failure.Classification != FailureRunCancelled {
		t.Fatalf("outcome %q failure %#v, want cancelled run_cancelled", result.Outcome, result.Failure)
	}
	if got := terminationOf(result); got != TerminationRunStopped {
		t.Fatalf("termination = %q, want %q", got, TerminationRunStopped)
	}
}

// The adversarial ownership matrix, at the executor: each pair of terminal
// events in both orders, asserting the OWNER the executor committed.
func TestTheExecutorCommitsTheFirstDecisiveTerminalEvent(t *testing.T) {
	requireBoundedProcess(t)
	type event int
	const (
		stop event = iota
		deadline
		inactivity
		shutdown
	)
	at := func(e event, d time.Duration) [2]int64 { return [2]int64{int64(e), int64(d)} }
	ms := time.Millisecond
	for _, c := range []struct {
		name   string
		script string
		events [][2]int64
		want   TerminationOwner
	}{
		{"provider exits first, then stop", "sleep 5 &\nexit 1\n", [][2]int64{at(stop, 100*ms)}, OwnerProviderExited},
		{"provider exits cleanly first, then stop", "sleep 5 &\nexit 0\n", [][2]int64{at(stop, 100*ms)}, OwnerProviderExited},
		{"stop first, provider running", gracefulProviderScript, [][2]int64{at(stop, 50*ms)}, OwnerOperatorStop},
		{"deadline first, then stop", gracefulProviderScript, [][2]int64{at(deadline, 50*ms), at(stop, 150*ms)}, OwnerDeadline},
		{"stop first, then deadline", gracefulProviderScript, [][2]int64{at(stop, 50*ms), at(deadline, 150*ms)}, OwnerOperatorStop},
		{"inactivity first, then stop", gracefulProviderScript, [][2]int64{at(inactivity, 50*ms), at(stop, 150*ms)}, OwnerInactivity},
		{"stop first, then inactivity", gracefulProviderScript, [][2]int64{at(stop, 50*ms), at(inactivity, 150*ms)}, OwnerOperatorStop},
		{"shutdown first, then stop", gracefulProviderScript, [][2]int64{at(shutdown, 50*ms), at(stop, 150*ms)}, OwnerControllerShutdown},
		{"stop first, then shutdown", gracefulProviderScript, [][2]int64{at(stop, 50*ms), at(shutdown, 150*ms)}, OwnerOperatorStop},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The same context chain production builds: controller -> the
			// execution watcher's stop -> the inactivity watchdog -> the
			// invocation deadline.
			controller, shutdownNow := context.WithCancel(context.Background())
			defer shutdownNow()
			execution, stopNow := context.WithCancelCause(controller)
			defer stopNow(nil)
			watched, inactiveNow := context.WithCancelCause(execution)
			defer inactiveNow(nil)
			var deadlineAt time.Duration
			for _, e := range c.events {
				if event(e[0]) == deadline {
					deadlineAt = time.Duration(e[1])
				}
			}
			ctx := watched
			if deadlineAt > 0 {
				bounded, cancel := context.WithTimeout(watched, deadlineAt)
				defer cancel()
				ctx = bounded
			}
			for _, e := range c.events {
				var fire func()
				switch event(e[0]) {
				case stop:
					fire = func() { stopNow(errRunStopped) }
				case inactivity:
					fire = func() { inactiveNow(ErrProviderInactive) }
				case shutdown:
					fire = shutdownNow
				default:
					continue
				}
				timer := time.AfterFunc(time.Duration(e[1]), fire)
				defer timer.Stop()
			}
			var out bytes.Buffer
			cmd := exec.Command("sh", "-c", c.script)
			cmd.Stdout, cmd.Stderr = &out, &out
			owner, _ := runBoundedProcess(ctx, cmd, time.Second)
			if owner != c.want {
				t.Fatalf("owner = %q, want %q", owner, c.want)
			}
		})
	}
}

// The ownership state is single-assignment: under a race, exactly one owner is
// committed and every caller sees that one.
func TestTerminalOwnershipIsCommittedOnce(t *testing.T) {
	owners := []TerminationOwner{OwnerProviderExited, OwnerDeadline, OwnerInactivity, OwnerOperatorStop, OwnerControllerShutdown}
	for round := 0; round < 200; round++ {
		var ownership terminalOwnership
		seen := make([]TerminationOwner, 64)
		var wg sync.WaitGroup
		for i := range seen {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				seen[i] = ownership.settle(owners[i%len(owners)])
			}(i)
		}
		wg.Wait()
		for _, got := range seen {
			if got != seen[0] || got == OwnerUndecided {
				t.Fatalf("round %d: settled %q and %q", round, seen[0], got)
			}
		}
		if again := ownership.settle(OwnerControllerShutdown); again != seen[0] {
			t.Fatalf("a later event replaced the owner: %q -> %q", seen[0], again)
		}
	}
}
