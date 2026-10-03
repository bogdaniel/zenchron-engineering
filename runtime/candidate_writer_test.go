//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shortWriterSettle keeps refusal tests fast; the production bound is 15s.
func shortWriterSettle(t *testing.T) {
	t.Helper()
	restore := candidateWriterSettle
	candidateWriterSettle = 200 * time.Millisecond
	t.Cleanup(func() { candidateWriterSettle = restore })
}

func requirePerl(t *testing.T) {
	t.Helper()
	requireBoundedProcess(t)
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl unavailable")
	}
}

// setsidScript runs cmd in a session of its own, outside the caller's group,
// recording its pid in pidFile.
func setsidScript(pidFile, cmd string) string {
	return "perl -e 'use POSIX; POSIX::setsid(); exec @ARGV' sh -c 'echo $$ > " + pidFile + "; " + cmd + "' >/dev/null 2>&1 &\n"
}

func killPIDFile(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

func candidateUnder(t *testing.T) string {
	t.Helper()
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	return candidate
}

// escapedWriterEnv turns this package's test binary into the OWNER of a
// provider whose tool command left the provider's process group - the shape
// the owner-death guard cannot reach, because it can only signal that group.
const escapedWriterEnv = "ZENCHRON_TEST_ESCAPED_WRITER_DIR"

func TestEscapedWriterOwnerHelper(t *testing.T) {
	dir := os.Getenv(escapedWriterEnv)
	if dir == "" {
		t.Skip("owner helper entry point, driven by TestAWriterThatEscapedItsDeadOwnerBlocksTheNextAttempt")
	}
	lock, err := claimCandidateWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	script := setsidScript("escaped.pid", "while :; do echo x >> escaped.log; sleep 0.05; done") +
		"while :; do sleep 30; done\n"
	// Never returns: the workload runs until this process is killed.
	_, _ = OSCommandExecutor{}.Run(withCandidateWriter(context.Background(), lock), "sh", []string{"-c", script}, dir, os.Environ(), 100*time.Millisecond)
}

// TestAWriterThatEscapedItsDeadOwnerBlocksTheNextAttempt drives #168's
// remaining hole with real processes: a supervisor SIGKILLed while its
// provider's tool command runs outside the provider's process group. The
// owner-death guard stops the group and the tool command survives it, still
// writing. What must not happen is a new attempt dispatched beside it.
func TestAWriterThatEscapedItsDeadOwnerBlocksTheNextAttempt(t *testing.T) {
	requirePerl(t)
	shortWriterSettle(t)
	candidate := candidateUnder(t)
	pidFile := filepath.Join(candidate, "escaped.pid")
	killPIDFile(t, pidFile)

	owner := exec.Command(os.Args[0], "-test.run=^TestEscapedWriterOwnerHelper$", "-test.timeout=60s")
	owner.Env = append(os.Environ(), escapedWriterEnv+"="+candidate)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Process.Wait() }()
	if err := waitForFile(filepath.Join(candidate, "escaped.log"), 10*time.Second); err != nil {
		t.Fatalf("the escaped tool command never started writing: %v", err)
	}
	if err := owner.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	// Past the guard's grace and escalation: the group is gone, the escaped
	// writer is not. That is the precondition, not the property.
	time.Sleep(400 * time.Millisecond)
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatal("precondition: the escaped writer did not survive its owner, so this proves nothing")
	}

	_, err := claimCandidateWriter(candidate)
	if class, ok := candidateGuardFailureClass(err); !ok || class != FailureCandidateWriterAlive || RouteFailure(class) != RouteWait {
		t.Fatalf("a new attempt was allowed beside a dead owner's live writer: %q %v (%v)", class, ok, err)
	}

	// Once the stale writer is gone the candidate is dispatchable again.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := claimCandidateWriter(candidate)
		if err == nil {
			_ = lock.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the candidate stayed refused after the stale writer died: %v", err)
		}
	}
}

// H1: a normal return with a leftover that is still IN the provider's group is
// this attempt's own background command. It is stopped and the next attempt
// proceeds; only one that left the group is reported.
func TestAProvidersOwnLeftoverIsStoppedButAnEscapeeIsAttributed(t *testing.T) {
	requirePerl(t)
	shortWriterSettle(t)

	t.Run("in-group leftover", func(t *testing.T) {
		candidate := candidateUnder(t)
		lock, err := claimCandidateWriter(candidate)
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(candidate, "leftover.pid")
		killPIDFile(t, marker)
		script := "sh -c 'echo $$ > " + marker + "; exec sleep 30' >/dev/null 2>&1 &\nsleep 0.2\n"
		out, err := OSCommandExecutor{}.Run(withCandidateWriter(context.Background(), lock), "sh", []string{"-c", script}, candidate, os.Environ(), 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if out.EscapedWriter {
			t.Fatal("a leftover inside the provider's own group was reported as an escaped writer")
		}
		next, err := claimCandidateWriter(candidate)
		if err != nil {
			t.Fatalf("the previous attempt's own leftover refused the next attempt: %v", err)
		}
		_ = next.Close()
	})

	t.Run("escaped leftover", func(t *testing.T) {
		candidate := candidateUnder(t)
		lock, err := claimCandidateWriter(candidate)
		if err != nil {
			t.Fatal(err)
		}
		pidFile := filepath.Join(candidate, "escaped.pid")
		killPIDFile(t, pidFile)
		script := setsidScript(pidFile, "exec sleep 30") + "sleep 0.2\n"
		out, err := OSCommandExecutor{}.Run(withCandidateWriter(context.Background(), lock), "sh", []string{"-c", script}, candidate, os.Environ(), 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !out.EscapedWriter {
			t.Fatal("a writer that left the provider's group and kept the lock was not reported")
		}
	})
}

// The adapter turns an escaped writer into a finding against THIS attempt.
func TestAnEscapedWriterIsBackgroundWorkThisAttemptAbandoned(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	fake.outputs = []CommandOutput{{EscapedWriter: true}}
	result, _ := provider.Execute(context.Background(), request)
	if result.Failure == nil || result.Failure.Classification != FailureProviderBackgroundWorkUnresolved {
		t.Fatalf("an escaped writer was not attributed to the attempt: %+v", result.Failure)
	}
}

// The adapter refuses a candidate whose lock is held, without invoking it.
func TestAProviderIsNotDispatchedIntoALockedCandidate(t *testing.T) {
	shortWriterSettle(t)
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	held, err := claimCandidateWriter(request.CandidateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = provider.Execute(context.Background(), request)
	var alive *CandidateWriterAliveError
	if !errors.As(err, &alive) {
		t.Fatalf("a provider was dispatched into a candidate another writer holds: %v", err)
	}
	for _, call := range fake.calls {
		if last := call.args[len(call.args)-1]; last != "--help" && last != "--version" {
			t.Fatalf("the provider was invoked: %v", call.args)
		}
	}
}

// M2: the runtime releases its own claim; a second attempt on the same
// candidate is never refused by the first.
func TestASecondExecutionOnOneCandidateIsNotRefused(t *testing.T) {
	shortWriterSettle(t)
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	for i := 0; i < 2; i++ {
		if _, err := provider.Execute(context.Background(), request); err != nil {
			t.Fatalf("execution %d: %v", i+1, err)
		}
	}
}

// L1: a lock that cannot be checked is the controller's own setup failure.
func TestAnUncheckableWriterLockIsAGuardFailureNotALiveWriter(t *testing.T) {
	_, err := claimCandidateWriter(filepath.Join(t.TempDir(), "missing-parent", "candidate"))
	if class, ok := candidateGuardFailureClass(err); !ok || class != FailureCandidateGuardUnavailable {
		t.Fatalf("an uncheckable lock classified as %q %v (%v)", class, ok, err)
	}
}

// B1: the claim comes BEFORE admission. A dead owner's writer can leave a
// candidate the credential scan refuses - #168's oversized cache blob - and
// judging it then is a STOP charged to a run whose attempt never ran.
func TestALiveWriterIsAWaitBeforeTheCandidateIsJudged(t *testing.T) {
	requireBoundedProcess(t)
	shortWriterSettle(t)
	fixture := newPhase8Fixture(t, func(origin string) {
		if err := os.WriteFile(filepath.Join(origin, ".env"), []byte("TOKEN=hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	provider := &countingProvider{}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	candidate := candidateDir(fixture.deps.StateDir, runID)
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	// A live holder that is not this process: what an escaped writer is.
	lock, err := claimCandidateWriter(candidate)
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("sleep", "30")
	holder.ExtraFiles = []*os.File{lock}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	t.Cleanup(func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() })

	for pass := 0; pass < 10; pass++ {
		fixture.reconcile(runID)
		if class, found := executionFailureClass(t, fixture.state(runID)); found {
			if class != FailureCandidateWriterAlive {
				t.Fatalf("execution failure class = %q, want %q", class, FailureCandidateWriterAlive)
			}
			break
		}
	}
	if class, found := executionFailureClass(t, fixture.state(runID)); !found || class != FailureCandidateWriterAlive {
		t.Fatalf("no writer-alive refusal was recorded: %q %v", class, found)
	}
	if provider.calls != 0 {
		t.Fatalf("provider was invoked %d time(s) beside a live writer", provider.calls)
	}
	if disposition := fixture.state(runID).snapshot.Disposition; terminalDisposition(disposition) {
		t.Fatalf("a live writer stopped the run: %q", disposition)
	}
}

// L2: GC does not delete a candidate under a live writer, and removes the
// lock file with the candidate.
func TestGCLeavesACandidateWithALiveWriterAndRemovesItsLock(t *testing.T) {
	shortWriterSettle(t)
	f := newGCFixture(t)
	f.createRun("run-old")
	m := f.material("run-old")
	f.settle("run-old", EventRunCompleted)
	lock, err := claimCandidateWriter(m.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "a candidate a live writer holds", m.candidate)
	_ = lock.Close()
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, "the candidate workspace", m.candidate)
	mustNotExist(t, "the candidate writer lock", candidateWriterLockPath(m.candidate))
}

// M1: a lock released inside the settle bound - a predecessor's guard still
// stopping its group during a fast restart - is waited out, not refused.
func TestAClaimWaitsOutABrieflyHeldLock(t *testing.T) {
	restore := candidateWriterSettle
	candidateWriterSettle = 5 * time.Second
	t.Cleanup(func() { candidateWriterSettle = restore })
	candidate := candidateUnder(t)
	held, err := claimCandidateWriter(candidate)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, func() { _ = held.Close() })
	lock, err := claimCandidateWriter(candidate)
	if err != nil {
		t.Fatalf("a lock released within the settle bound was refused: %v", err)
	}
	_ = lock.Close()
}

// L2: a planning invocation refused by the runtime before dispatch is a typed
// planner refusal, and removing the planning workspace removes its lock.
func TestAPlannerRefusalBeforeDispatchIsTyped(t *testing.T) {
	input, provider := plannerFixture(t, "")
	provider.err = &CandidateWriterAliveError{Lock: candidateWriterLockPath(input.Workspace.Dir)}
	_, err := InvokePlanner(context.Background(), input)
	var refused *PlannerRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Detail, string(FailureCandidateWriterAlive)) {
		t.Fatalf("a pre-dispatch refusal reached the planner untyped: %T %v", err, err)
	}
	lock, err := claimCandidateWriter(input.Workspace.Dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	if err := input.Workspace.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(candidateWriterLockPath(input.Workspace.Dir)); !os.IsNotExist(err) {
		t.Fatalf("the planning workspace's writer lock survived its removal: %v", err)
	}
}
