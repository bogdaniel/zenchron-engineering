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
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A provider that is running, and when asked to terminate takes 250ms of grace
// before it exits: every event that lands in that window arrives AFTER the
// owner was committed.
const gracefulProviderScript = "trap 'sleep 0.25; exit 143' TERM\nwhile :; do sleep 0.02; done\n"

// A provider that stops itself (as SIGSTOP, or SIGTTIN/SIGTTOU from its own
// background process group, would): still running, not exited.
const stoppedProviderScript = "kill -STOP $$\necho resumed\n"

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
		{"provider exits by itself, no event", "exit 0\n", nil, OwnerProviderExited},
		{"provider exits first, then stop", "sleep 5 &\nexit 1\n", [][2]int64{at(stop, 100*ms)}, OwnerProviderExited},
		{"provider exits cleanly first, then stop", "sleep 5 &\nexit 0\n", [][2]int64{at(stop, 100*ms)}, OwnerProviderExited},
		{"stop first, provider running", gracefulProviderScript, [][2]int64{at(stop, 50*ms)}, OwnerOperatorStop},
		{"deadline first, then stop", gracefulProviderScript, [][2]int64{at(deadline, 50*ms), at(stop, 150*ms)}, OwnerDeadline},
		{"deadline first, then the process exits", gracefulProviderScript, [][2]int64{at(deadline, 50*ms)}, OwnerDeadline},
		// A STOPPED provider is still running, not exited: every external
		// event wins arbitration before termination is initiated.
		{"stopped, deadline", stoppedProviderScript, [][2]int64{at(deadline, 100*ms)}, OwnerDeadline},
		{"stopped, operator stop", stoppedProviderScript, [][2]int64{at(stop, 100*ms)}, OwnerOperatorStop},
		{"stopped, inactivity", stoppedProviderScript, [][2]int64{at(inactivity, 100*ms)}, OwnerInactivity},
		{"stopped, controller shutdown", stoppedProviderScript, [][2]int64{at(shutdown, 100*ms)}, OwnerControllerShutdown},
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

// The linearization point: the root has exited in the kernel, the reaper has
// not delivered yet, and only then does the context fire. The executor asks
// the kernel without reaping, sees the exit, and the provider owns its ending.
// The held reaper then still reaps it: there is exactly one reaper.
func TestAnExitedRootIsNotTerminatedByALaterContext(t *testing.T) {
	requireBoundedProcess(t)
	release := make(chan struct{})
	restore := holdReaper
	holdReaper = func() { <-release }
	defer func() { holdReaper = restore }()
	marker := filepath.Join(t.TempDir(), "exiting")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cmd := exec.Command("sh", "-c", ": > "+marker+"; exit 3")
	type outcome struct {
		owner TerminationOwner
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		owner, err := runBoundedProcess(ctx, cmd, time.Second)
		done <- outcome{owner, err}
	}()
	if err := waitForFile(marker, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the root has exited; nothing has reaped it
	cancel(errRunStopped)
	time.Sleep(100 * time.Millisecond)
	close(release)
	got := <-done
	if got.owner != OwnerProviderExited {
		t.Fatalf("owner = %q: a root that had already exited was attributed to a later stop", got.owner)
	}
	var exit *exec.ExitError
	if !errors.As(got.err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the reaper did not reap the root's own exit status: %v", got.err)
	}
}

// rootExited never reaps: a running child reads as running, an exited child
// reads as exited, and the real Wait afterwards still gets its status.
func TestRootExitedDoesNotReap(t *testing.T) {
	requireBoundedProcess(t)
	running := exec.Command("sleep", "5")
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	if rootExited(running.Process.Pid) {
		t.Fatal("a running child read as exited")
	}
	// darwin's waitid reports a STOPPED child with si_signo SIGCHLD; only an
	// exit si_code may read as exited.
	if err := running.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if rootExited(running.Process.Pid) {
		t.Fatal("a stopped child read as exited")
	}
	if err := running.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if rootExited(running.Process.Pid) {
		t.Fatal("a continued child read as exited")
	}
	_ = running.Process.Kill()
	_ = running.Wait()
	exited := exec.Command("sh", "-c", "exit 7")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !rootExited(exited.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("an exited child never read as exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var exit *exec.ExitError
	if err := exited.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("the probe reaped the child: Wait = %v", err)
	}
}

// A context already ended when the executor would start the process: no
// process exists, so the executor settles "not started" - never a provider
// termination - and the adapter invents no provenance for it.
func TestAStopBeforeStartIsNotAProviderTermination(t *testing.T) {
	requireBoundedProcess(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errRunStopped)
	owner, err := runBoundedProcess(ctx, exec.CommandContext(ctx, "sh", "-c", "exit 0"), time.Second)
	var notStarted *ProviderNotStartedError
	if owner != OwnerNotStarted || !errors.As(err, &notStarted) || !errors.Is(notStarted.Cause, errRunStopped) {
		t.Fatalf("owner %q err %v, want not_started with the stop as its cause", owner, err)
	}
	provider, request, _ := inactivityFixture(t, "exit 0\n")
	result, runErr := provider.Execute(ctx, request)
	if !errors.As(runErr, &notStarted) {
		t.Fatalf("Execute = %v, want the executor's not-started answer", runErr)
	}
	if result.Invocation != nil || len(result.Artifacts) != 0 {
		t.Fatalf("a provider that never started was given provenance %#v / artifacts %v", result.Invocation, result.Artifacts)
	}
	if result.Failure == nil || result.Failure.Classification != FailureRunCancelled {
		t.Fatalf("failure %#v, want run_cancelled for a stop before start", result.Failure)
	}
}

// Defence in depth: the kernel reported a terminal state, so provider_exited
// is linearized - and the reap then stalls. The executor cleans the group up
// within about one grace period, and the owner is NOT rewritten.
func TestAStalledReapAfterALinearizedExitKeepsTheOwner(t *testing.T) {
	requireBoundedProcess(t)
	restore := probeRootExited
	probeRootExited = func(int) bool { return true }
	defer func() { probeRootExited = restore }()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	timer := time.AfterFunc(50*time.Millisecond, func() { cancel(errRunStopped) })
	defer timer.Stop()
	started := time.Now()
	owner, _ := runBoundedProcess(ctx, exec.Command("sh", "-c", "while :; do sleep 0.02; done"), 300*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a stalled reap held the executor for %s", elapsed)
	}
	if owner != OwnerProviderExited {
		t.Fatalf("owner = %q: the bounded cleanup rewrote a linearized provider_exited", owner)
	}
}

// Only the terminal si_code values read as exited; stopped, continued and
// trapped children do not.
func TestOnlyTerminalChildCodesReadAsExited(t *testing.T) {
	for code, want := range map[int32]bool{0: false, 1: true, 2: true, 3: true, 4: false, 5: false, 6: false} {
		if got := terminalChildCode(code); got != want {
			t.Errorf("terminalChildCode(%d) = %v, want %v", code, got, want)
		}
	}
}
