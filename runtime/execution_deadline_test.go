package runtime

// #139: a provider invocation with an effective 1800-second wall limit ran for
// 54m10s, returned success, and was rejected only afterwards by the run-level
// budget check.
//
// The original event has never been reproduced. What these tests pin is the
// architecture that removes the ambiguity it exposed: a wall budget is POLICY,
// converted exactly once into an absolute deadline that is durable lifecycle
// state, and every later reader - a retry, a re-lease, a rehydrated controller,
// the provider bound, the admission gate - derives its authority from that one
// instant rather than re-deriving a duration from whenever it happens to be
// looking.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func deadlineScheduler(t *testing.T) (Scheduler, *steppingClock) {
	t.Helper()
	clock := &steppingClock{at: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	return Scheduler{Store: NewMemoryOperationStore(), Clock: clock, Owner: "owner", LeaseDuration: time.Minute}, clock
}

// plannedExecution plans an operation and takes it all the way to RUNNING,
// because that is where execution authority - and therefore the deadline -
// begins.
func plannedExecution(t *testing.T, s Scheduler, budget time.Duration) RunOperation {
	t.Helper()
	op, _, err := s.Plan(RunOperation{
		RunID: "run-1", Kind: OpExecutionInvoke, IdempotencyKey: "invoke-1",
		MaxAttempts: 3, WallBudget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	started, err := s.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

// THE PRIMITIVE. One operation, one instant, derived once.
func TestAnExecutionOperationCarriesOneDurableDeadline(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	if op.Deadline == nil {
		t.Fatal("a wall-budgeted operation was planned with no deadline")
	}
	want := op.StartedAt.Add(30 * time.Minute)
	if !op.Deadline.Equal(want) {
		t.Fatalf("deadline %s, want execution start plus the budget %s", op.Deadline, want)
	}
	// An operation with no budget has no deadline to invent.
	none, _, err := scheduler.Plan(RunOperation{RunID: "run-2", Kind: OpExecutionInvoke, IdempotencyKey: "unbudgeted"})
	if err != nil {
		t.Fatal(err)
	}
	if none.Deadline != nil {
		t.Fatalf("an unbudgeted operation invented a deadline: %s", none.Deadline)
	}
	// WAITING IS NOT EXECUTING. An operation that has been planned but has not
	// begun burns nothing, however long the run is parked.
	parked, _, err := scheduler.Plan(RunOperation{
		RunID: "run-3", Kind: OpExecutionInvoke, IdempotencyKey: "parked",
		MaxAttempts: 3, WallBudget: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)
	if parked.Deadline != nil {
		t.Fatalf("an operation that never executed was given a deadline: %s", parked.Deadline)
	}
	// And a later attempt of the operation that DID start inherits the instant
	// rather than minting another envelope.
	retried, err := scheduler.Finish(op.ID, OperationFailed)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Deadline == nil || !retried.Deadline.Equal(want) {
		t.Fatalf("finishing moved the deadline to %v, want %s", retried.Deadline, want)
	}
}

// RECOVERY IS THE CASE THAT MATTERS. A controller that restarts mid-execution
// must not hand the same work another full envelope.
func TestRecoveryPreservesTheOriginalDeadline(t *testing.T) {
	store := NewMemoryOperationStore()
	clock := &steppingClock{at: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	first := Scheduler{Store: store, Clock: clock, Owner: "owner-a", LeaseDuration: time.Minute}
	op := plannedExecution(t, first, 30*time.Minute)
	if op.Deadline == nil {
		t.Fatal("a started operation carries no deadline")
	}
	original := *op.Deadline

	// 25 of the 30 minutes are spent, then the controller is replaced: a new
	// scheduler, a new owner, the same durable store.
	clock.advance(25 * time.Minute)
	recovered := Scheduler{Store: store, Clock: clock, Owner: "owner-b", LeaseDuration: time.Minute}
	rehydrated, _, err := recovered.Plan(RunOperation{
		RunID: "run-1", Kind: OpExecutionInvoke, IdempotencyKey: "invoke-1",
		MaxAttempts: 3, WallBudget: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rehydrated.Deadline == nil || !rehydrated.Deadline.Equal(original) {
		t.Fatalf("recovery minted a fresh deadline %v, want the original %s", rehydrated.Deadline, original)
	}
	if remaining := OperationRemaining(rehydrated, clock.Now()); remaining != 5*time.Minute {
		t.Fatalf("a recovered operation has %s of authority left, want the 5m it had not spent", remaining)
	}
	// Past the instant, nothing remains and the operation is out of time.
	clock.advance(6 * time.Minute)
	if remaining := OperationRemaining(rehydrated, clock.Now()); remaining != 0 {
		t.Fatalf("an expired operation reports %s remaining", remaining)
	}
	if !OperationExpired(rehydrated, clock.Now()) {
		t.Fatal("an operation past its deadline is not expired")
	}
}

// THE BOUNDARY, both directions.
func TestAProviderIsBoundedByWhatRemainsOfItsDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		wall   time.Duration
		finish time.Duration
		bound  bool
	}{
		"finishes before the deadline": {wall: 2 * time.Second, finish: 100 * time.Millisecond, bound: false},
		"crosses the deadline":         {wall: 300 * time.Millisecond, finish: 30 * time.Second, bound: true},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := agentFixture(t, AgentKindCodexCLI)
			fake.block = tc.bound
			request.Budgets = ProviderBudget{WallLimit: tc.wall}
			start := time.Now()
			result, err := provider.Execute(context.Background(), request)
			elapsed := time.Since(start)
			if elapsed > tc.wall+5*time.Second {
				t.Fatalf("the invocation ran %s against a %s bound", elapsed, tc.wall)
			}
			if !tc.bound {
				if err != nil || result.Outcome != Succeeded {
					t.Fatalf("a provider that finished in time did not succeed: %v %v", result.Outcome, err)
				}
				return
			}
			if result.Outcome == Succeeded {
				t.Fatal("a provider stopped at its bound reported success")
			}
			if result.Invocation == nil || result.Invocation.TerminationCause != "deadline_reached" {
				t.Fatalf("the termination cause was not recorded as the deadline: %+v", result.Invocation)
			}
			if result.Invocation.Deadline == nil || result.Invocation.Elapsed <= 0 {
				t.Fatalf("the durable record does not expose the deadline and elapsed time: %+v", result.Invocation)
			}
		})
	}
}

// A STUBBORN PROCESS GROUP DIES. It ignores SIGTERM and forks a child inside
// the runtime-owned group; neither survives the deadline.
func TestAStubbornProcessGroupIsTerminatedAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn.sh")
	marker := filepath.Join(dir, "survived")
	body := "#!/bin/sh\ntrap '' TERM INT\n( sleep 30; echo child > " + marker + ".child ) &\nsleep 30\necho parent > " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	start := time.Now()
	err := runBoundedProcess(ctx, cmd, 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("the bounded process did not return promptly after its deadline: %s", elapsed)
	}
	if err == nil {
		t.Fatal("a process killed at its deadline reported success")
	}
	time.Sleep(2 * time.Second)
	for _, path := range []string{marker, marker + ".child"} {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("%s survived past the deadline", filepath.Base(path))
		}
	}
}

// A DESCENDANT THAT DETACHES ITSELF is where the guarantee has a platform
// boundary, and the boundary is stated rather than assumed.
//
// Linux records descendants while the root lives, so a setsid(2) child cannot
// escape the owned set. The other unixes retain process-GROUP containment only,
// and a process that leaves the group is outside it. What holds EVERYWHERE, and
// is what the lifecycle depends on, is that the INVOCATION still returns at its
// deadline rather than blocking on an escapee holding the inherited pipe.
func TestADetachedDescendantDoesNotBlockTheDeadline(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil && runtime.GOOS == "linux" {
		t.Skip("setsid is unavailable, so escape cannot be provoked")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "escape.sh")
	marker := filepath.Join(dir, "escapee")
	// The child leaves the process group if the platform offers a way to.
	body := "#!/bin/sh\ntrap '' TERM INT\nif command -v setsid >/dev/null 2>&1; then\n  setsid sh -c 'sleep 20; echo out > " + marker + "' &\nelse\n  sh -c 'sleep 20; echo out > " + marker + "' &\nfi\nsleep 20\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	start := time.Now()
	err := runBoundedProcess(ctx, cmd, 300*time.Millisecond)
	elapsed := time.Since(start)
	// THE INVARIANT THAT HOLDS ON EVERY PLATFORM: the runtime does not wait for
	// an escapee. A descendant holding the inherited stdout pipe used to be
	// able to keep Wait blocked indefinitely; WaitDelay is what bounds it.
	if elapsed > 8*time.Second {
		t.Fatalf("a detached descendant kept the invocation alive for %s past a 1s deadline", elapsed)
	}
	if err == nil {
		t.Fatal("an invocation ended at its deadline reported success")
	}
	if runtime.GOOS == "linux" {
		time.Sleep(2 * time.Second)
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatal("on linux the owned set must contain a setsid descendant, and it escaped")
		}
	}
}

// AUTHORITY, NOT TIMING. Even if termination fails entirely and a provider
// returns success after its deadline, the result is not governed output.
func TestAPostDeadlineSuccessCannotProduceACandidate(t *testing.T) {
	fixture := newPhase8Fixture(t)
	// The provider "works" for longer than the whole budget and then reports
	// success - exactly the shape of the observed #139 invocation.
	fixture.provider.mutate = func(dir string) error {
		fixture.clock.advance(2 * time.Hour)
		return os.WriteFile(filepath.Join(dir, "worked.txt"), []byte("late\n"), 0o600)
	}
	runID := fixture.start()
	for pass := 0; pass < 8; pass++ {
		fixture.reconcile(runID)
	}

	events := journalOf(t, fixture.runtime, runID)
	for _, event := range events {
		if event.Type == EventExecutionCompleted {
			t.Fatal("an invocation that returned after its deadline was recorded as a completed execution")
		}
		if event.Type == EventCandidateCommitted {
			t.Fatal("work produced without execution authority became a committed candidate")
		}
	}
	// And it is classified, not merely dropped.
	var classified bool
	operations, opErr := fixture.store.Operations(runID)
	if opErr != nil {
		t.Fatal(opErr)
	}
	for _, op := range operations {
		if op.Kind == OpExecutionInvoke && op.State == OperationFailed {
			classified = true
		}
	}
	if !classified {
		t.Fatal("the overdue execution operation was not failed")
	}
	if RouteFailure(FailureExecutionDeadlineExceeded) != RouteStop {
		t.Fatalf("a deadline overrun routes %q; retrying with no remaining authority is an immediate second expiry",
			RouteFailure(FailureExecutionDeadlineExceeded))
	}
}

// The durable record answers "what authority did this process have" without a
// forensic reconstruction.
func TestTheDurableRecordExposesTheExecutionAuthority(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	fake.outputs = []CommandOutput{{Stdout: []byte("done\n"), ProcessID: 4242}}
	request.Budgets = ProviderBudget{WallLimit: time.Minute}
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	record := result.Invocation
	if record == nil {
		t.Fatal("no invocation provenance was recorded")
	}
	for name, missing := range map[string]bool{
		"execution_deadline":     record.Deadline == nil,
		"execution_started_at":   record.StartedAt == nil,
		"execution_completed_at": record.CompletedAt == nil,
		"termination_cause":      strings.TrimSpace(record.TerminationCause) == "",
		"process_id":             record.ProcessID == 0,
	} {
		if missing {
			t.Fatalf("the durable record does not expose %s: %+v", name, record)
		}
	}
	if record.OverranDeadline {
		t.Fatal("an invocation that finished in time was recorded as overrunning")
	}
	if record.TerminationCause != "provider_returned" {
		t.Fatalf("termination cause %q, want the provider's own return", record.TerminationCause)
	}
}
