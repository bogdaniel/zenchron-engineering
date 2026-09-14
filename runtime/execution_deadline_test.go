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
	"encoding/json"
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

// THE PRIMITIVE. One cumulative active-execution budget per operation.
func TestAnExecutionOperationSpendsOneCumulativeBudget(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	if op.ActiveSince == nil || op.Deadline == nil {
		t.Fatal("a started operation is not active and carries no attempt deadline")
	}
	if want := op.ActiveSince.Add(30 * time.Minute); !op.Deadline.Equal(want) {
		t.Fatalf("attempt deadline %s, want the whole budget from execution start %s", op.Deadline, want)
	}
	// An unbudgeted operation has no authority to run out of.
	none, _, err := scheduler.Plan(RunOperation{RunID: "run-2", Kind: OpExecutionInvoke, IdempotencyKey: "unbudgeted"})
	if err != nil {
		t.Fatal(err)
	}
	if OperationExpired(none, clock.Now()) || OperationRemaining(none, clock.Now()) != 0 {
		t.Fatal("an unbudgeted operation invented a limit")
	}
	// WAITING IS NOT EXECUTING: an operation that never started burns nothing.
	parked, _, err := scheduler.Plan(RunOperation{
		RunID: "run-3", Kind: OpExecutionInvoke, IdempotencyKey: "parked",
		MaxAttempts: 3, WallBudget: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)
	if OperationExpired(parked, clock.Now()) {
		t.Fatal("an operation that never executed ran out of execution authority")
	}
}

// PARTIAL EXECUTION THEN RETRY. The second attempt inherits the remainder.
func TestARetryInheritsTheRemainderRatherThanAFreshBudget(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	clock.advance(20 * time.Minute)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	second, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(second, clock.Now()); got != 10*time.Minute {
		t.Fatalf("the second attempt received %s, want the 10m the first did not spend", got)
	}
	if want := clock.Now().Add(10 * time.Minute); !second.Deadline.Equal(want) {
		t.Fatalf("attempt deadline %s, want %s", second.Deadline, want)
	}
}

// PARTIAL EXECUTION, A FOUR-HOUR EXTERNAL WAIT, THEN RESUME.
//
// This is the case an immutable wall-clock instant cannot express. Wall-clock
// time from the first start to the resume is 4h5m against a 30m budget; active
// execution is 5m. The operation must resume with 25m.
func TestAnExternalWaitDoesNotConsumeExecutionAuthority(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	clock.advance(5 * time.Minute)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	// The provider declined at its account boundary: no work happened, so the
	// attempt and its budget are given back.
	if _, err := scheduler.RestoreAttempt(op.ID, true); err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)

	waiting, _, ok, err := scheduler.Store.Operation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if OperationExpired(waiting, clock.Now()) {
		t.Fatal("a four-hour external wait consumed the execution budget")
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 30*time.Minute {
		t.Fatalf("the resumed attempt received %s; a refusal that ran no work costs nothing", got)
	}
}

// The same, without the attempt being restored: work DID happen, so its 5m is
// charged and only that.
func TestOnlyExecutedTimeIsCharged(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	clock.advance(5 * time.Minute)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 25*time.Minute {
		t.Fatalf("the resumed attempt received %s, want 25m: 5m executed, 4h waited", got)
	}
}

// CONTROLLER RESTART MID-ATTEMPT. Same remaining authority, same instant.
func TestRecoveryPreservesTheAttemptDeadline(t *testing.T) {
	store := NewMemoryOperationStore()
	clock := &steppingClock{at: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	first := Scheduler{Store: store, Clock: clock, Owner: "owner-a", LeaseDuration: time.Minute}
	op := plannedExecution(t, first, 30*time.Minute)
	if op.Deadline == nil {
		t.Fatal("a started operation carries no attempt deadline")
	}
	original := *op.Deadline

	clock.advance(25 * time.Minute)
	before := OperationRemaining(op, clock.Now())

	// A different controller, a different owner, the same durable store.
	recovered := Scheduler{Store: store, Clock: clock, Owner: "owner-b", LeaseDuration: time.Minute}
	rehydrated, _, ok, err := recovered.Store.Operation(op.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if rehydrated.Deadline == nil || !rehydrated.Deadline.Equal(original) {
		t.Fatalf("recovery minted a fresh deadline %v, want the original %s", rehydrated.Deadline, original)
	}
	if after := OperationRemaining(rehydrated, clock.Now()); after != before || after != 5*time.Minute {
		t.Fatalf("remaining authority moved across recovery: %s before, %s after", before, after)
	}
	clock.advance(6 * time.Minute)
	if !OperationExpired(rehydrated, clock.Now()) {
		t.Fatal("an attempt past its authority is not expired")
	}
}

// THE BOUNDARY, both directions, and the identity that matters: what bounds
// the process is the operation's remaining authority, and what is RECORDED as
// its authority is that same instant.
func TestAProviderIsBoundedByTheOperationsRemainingAuthority(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 2*time.Second)
	// Most of the budget is already spent by an earlier attempt.
	clock.advance(1700 * time.Millisecond)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	second, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(second, clock.Now()); got != 300*time.Millisecond {
		t.Fatalf("the second attempt has %s of authority, want 300ms", got)
	}

	// The invocation is handed that instant, not a fresh full duration.
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	fake.block = true
	request.Budgets = ProviderBudget{WallLimit: OperationRemaining(second, clock.Now())}
	request.Deadline = second.Deadline

	start := time.Now()
	// An invocation stopped at its bound reports the stop; the error is the
	// bound being reached, not a harness failure.
	result, _ := provider.Execute(context.Background(), request)
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("the invocation ran %s against 300ms of remaining authority", elapsed)
	}
	if result.Outcome == Succeeded {
		t.Fatal("a provider stopped at its bound reported success")
	}
	if result.Invocation == nil {
		t.Fatal("no provenance was recorded")
	}
	// PROVENANCE EQUALS LIFECYCLE AUTHORITY. Not approximately, and not
	// recomputed: the same instant the operation carries.
	if result.Invocation.Deadline == nil || !result.Invocation.Deadline.Equal(*second.Deadline) {
		t.Fatalf("recorded deadline %v is not the operation's authority %s",
			result.Invocation.Deadline, second.Deadline)
	}
	if result.Invocation.TerminationCause != "deadline_reached" {
		t.Fatalf("termination cause %q, want deadline_reached", result.Invocation.TerminationCause)
	}
}

// A provider that finishes inside its authority succeeds.
func TestAProviderFinishingWithinItsAuthoritySucceeds(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	deadline := time.Now().Add(5 * time.Second)
	request.Budgets = ProviderBudget{WallLimit: 5 * time.Second}
	request.Deadline = &deadline
	result, err := provider.Execute(context.Background(), request)
	if err != nil || result.Outcome != Succeeded {
		t.Fatalf("a provider that finished in time did not succeed: %v %v", result.Outcome, err)
	}
	if result.Invocation == nil || result.Invocation.OverranDeadline {
		t.Fatalf("an invocation that finished in time was recorded as overrunning: %+v", result.Invocation)
	}
}

// REAL RECOVERY, not a re-read of the row. A controller that dies mid-attempt
// never reaches Finish, so nothing folded that attempt's elapsed execution into
// the counter. Resuming must not hand the work a fresh envelope.
func TestAResumedAttemptDoesNotRegainTheTimeACrashedAttemptSpent(t *testing.T) {
	store := NewMemoryOperationStore()
	clock := &steppingClock{at: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	dead := OwnerLivenessFunc(func(string) bool { return false })
	first := Scheduler{Store: store, Clock: clock, Owner: "owner-a", LeaseDuration: time.Minute, Liveness: dead}
	op := plannedExecution(t, first, 30*time.Minute)

	// 25 minutes of real execution, then the controller dies: no Finish, no
	// RestoreAttempt, the lease simply stops being renewed.
	clock.advance(25 * time.Minute)

	// A new controller takes the abandoned operation through the REAL
	// acquisition path and resumes it.
	second := Scheduler{Store: store, Clock: clock, Owner: "owner-b", LeaseDuration: time.Minute, Liveness: dead}
	next, err := second.Next(op.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("the abandoned operation was never reacquired")
	}
	resumed, err := second.Start(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	remaining := OperationRemaining(resumed, clock.Now())
	if remaining > 5*time.Minute {
		t.Fatalf("the resumed attempt has %s of authority; the crashed attempt spent 25m of a 30m budget", remaining)
	}
	if want := clock.Now().Add(remaining); !resumed.Deadline.Equal(want) {
		t.Fatalf("attempt deadline %s does not match the %s remaining", resumed.Deadline, remaining)
	}
}

// A LATE EXTERNAL REFUSAL DOES NOT MAKE THE WORK BEFORE IT FREE.
func TestWorkPerformedBeforeAWaitStaysConsumed(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	// The provider reasons, edits and calls tools for twenty minutes and only
	// then meets a rate limit that routes to a wait.
	clock.advance(20 * time.Minute)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RestoreAttempt(op.ID, false); err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 10*time.Minute {
		t.Fatalf("the resumed attempt received %s; twenty minutes of real provider work must stay consumed", got)
	}
}

// AND THE LEGITIMATE ZERO-COST CASE IS PRESERVED: refused before execution
// began, so nothing was spent.
func TestARefusalBeforeExecutionCostsNothing(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	clock.advance(2 * time.Second)
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RestoreAttempt(op.ID, true); err != nil {
		t.Fatal(err)
	}
	clock.advance(4 * time.Hour)
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 30*time.Minute {
		t.Fatalf("the resumed attempt received %s; a refusal that ran no work costs nothing", got)
	}
}

// The runtime decides which of those two it was from EVIDENCE, not from the
// route: an attempt that reached a worker is charged even when the failure it
// reported routes to a wait.
func TestTheRefundDecisionComesFromWhetherAWorkerRan(t *testing.T) {
	executed, err := json.Marshal(mutationResult{FailureClass: FailureProviderRateLimited, ProviderExecuted: true})
	if err != nil {
		t.Fatal(err)
	}
	refused, err := json.Marshal(mutationResult{FailureClass: FailureProviderAccountUnavailable})
	if err != nil {
		t.Fatal(err)
	}
	if providerExecuted(executed) != true {
		t.Fatal("an attempt that reached a worker was treated as having run nothing")
	}
	if providerExecuted(refused) != false {
		t.Fatal("a pre-execution refusal was treated as having run work")
	}
	// An unreadable result is charged rather than refunded: guessing generously
	// about an unknown is how a bounded budget stops being one.
	if providerExecuted(nil) != true {
		t.Fatal("an unreadable result was refunded")
	}
}

// A STUBBORN PROCESS GROUP DIES. It ignores SIGTERM and forks a child inside
// the runtime-owned group; neither survives the deadline.
func TestAStubbornProcessGroupIsTerminatedAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn.sh")
	marker := filepath.Join(dir, "survived")
	// GENUINELY stubborn. Trapping TERM and then sleeping once is not: the
	// sleep is a separate process, killing it lets the trapping shell resume
	// and exit zero, and the fixture reports a clean completion it never had.
	// Looping means only SIGKILL ends either process.
	loop := "i=0; while [ $i -lt 60 ]; do sleep 1; i=$((i+1)); done"
	body := "#!/bin/sh\ntrap '' TERM INT\n( trap '' TERM INT; " + loop + "; echo child > " + marker + ".child ) &\n" + loop + "\necho parent > " + marker + "\n"
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
	// The escapee is its OWN script. Passing it as `sh -c "...$i..."` let the
	// writing shell expand the loop variable, the loop collapsed, and its final
	// echo ran immediately - which read exactly like an escape that had not
	// happened.
	child := filepath.Join(dir, "escapee.sh")
	loop := "i=0; while [ $i -lt 40 ]; do sleep 1; i=$((i+1)); done"
	if err := os.WriteFile(child, []byte("#!/bin/sh\ntrap '' TERM INT\n"+loop+"\necho out > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ntrap '' TERM INT\nif command -v setsid >/dev/null 2>&1; then\n  setsid /bin/sh " + child + " &\nelse\n  /bin/sh " + child + " &\nfi\n" + loop + "\n"
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
