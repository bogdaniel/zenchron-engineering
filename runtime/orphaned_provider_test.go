//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

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

// orphanOwnerEnv names the candidate workspace the owner helper below drives a
// hostile provider against. Its presence is what turns one of this package's
// test binaries into the OWNER process of a real bounded workload.
const orphanOwnerEnv = "ZENCHRON_TEST_ORPHANED_PROVIDER_DIR"

// hostileProviderScript is a provider that behaves like the one in the reported
// failure: it ignores SIGTERM and it writes bulk material into the
// runtime-owned candidate workspace for as long as it is allowed to run.
//
// The volume is deliberate. The candidate credential scan refuses a file above
// credentialScanFileLimit as INCONCLUSIVE, so a provider that outlives its
// owner by a second or two writes a workspace no recovery can ever admit. That
// refusal is correct and is not what this test doubts; what it doubts is that
// anything stops the writing.
func hostileProviderScript(dir string) string {
	cache := filepath.Join(dir, "orphan-cache")
	return "trap '' TERM\n" +
		"echo $$ > " + filepath.Join(dir, "provider.pid") + "\n" +
		"i=0\n" +
		"while [ $i -lt 64 ]; do\n" +
		"  dd if=/dev/zero bs=1048576 count=1 >> " + cache + " 2>/dev/null\n" +
		"  : > " + filepath.Join(dir, "provider.ready") + "\n" +
		"  sleep 0.05\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"while :; do sleep 30; done\n"
}

// TestOrphanedProviderOwnerHelper is not a test. It is the OWNER process of the
// scenario: a runtime instance that has a live provider running against its
// candidate workspace and is then killed with SIGKILL from outside, executing
// no shutdown path of any kind - exactly the `kill -9 serve` in the report.
func TestOrphanedProviderOwnerHelper(t *testing.T) {
	dir := os.Getenv(orphanOwnerEnv)
	if dir == "" {
		t.Skip("owner helper entry point, driven by TestOwnerDeathStopsTheProviderWritingIntoTheCandidate")
	}
	// Never returns: the workload runs until this process is killed.
	_, _ = OSCommandExecutor{}.Run(context.Background(), "sh", []string{"-c", hostileProviderScript(dir)}, "", os.Environ(), 100*time.Millisecond)
}

// TestOwnerDeathStopsTheProviderWritingIntoTheCandidate drives the reported
// sequence rather than a proxy for it: a real owner process, a real provider
// process writing real bytes into a real candidate workspace, a real SIGKILL of
// the owner, and then the only question that matters - does the workspace stop
// changing, and is it still admissible afterwards.
//
// Before the owner-death guard the provider survived its owner and kept
// writing, and the workspace it left behind exceeded the deterministic scan
// ceiling, so recovery refused it and the run lost its last attempt.
func TestOwnerDeathStopsTheProviderWritingIntoTheCandidate(t *testing.T) {
	requireBoundedProcess(t)
	if _, err := exec.LookPath("dd"); err != nil {
		t.Skip("dd unavailable")
	}
	candidate := t.TempDir()
	pidFile := filepath.Join(candidate, "provider.pid")
	t.Cleanup(func() {
		// If the guard did not fire, the provider is still running and still
		// writing. Stop its whole group rather than leaving it to the host.
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	})

	owner := exec.Command(os.Args[0], "-test.run=^TestOrphanedProviderOwnerHelper$", "-test.timeout=60s")
	owner.Env = append(os.Environ(), orphanOwnerEnv+"="+candidate)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Process.Wait() }()
	if err := waitForFile(filepath.Join(candidate, "provider.ready"), 10*time.Second); err != nil {
		t.Fatalf("the provider never started writing into the candidate workspace: %v", err)
	}

	// The owner dies executing nothing at all.
	if err := owner.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Process.Wait(); err != nil {
		t.Fatal(err)
	}

	// Past the guard's own graceful period and its escalation, the workspace
	// must be inert. Two samples half a second apart: a provider still running
	// adds roughly twenty megabytes between them.
	time.Sleep(400 * time.Millisecond)
	first := candidateBytes(t, candidate)
	time.Sleep(500 * time.Millisecond)
	if second := candidateBytes(t, candidate); second != first {
		t.Fatalf("the orphaned provider kept writing into the candidate workspace after its owner died: %d bytes, then %d", first, second)
	}
	if processFromFileAlive(pidFile) {
		t.Fatal("the orphaned provider process outlived its owner")
	}
	// The reported failure, asked directly: recovery must still be able to
	// admit what it finds.
	if err := ScanCandidateForCredentialValues(candidate); err != nil {
		t.Fatalf("the candidate workspace the orphan left is not admissible: %v", err)
	}
}

func candidateBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	if err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// TestAGuardThatCannotBeArmedRefusesBeforeTheWorkloadStarts pins the ORDERING,
// which is the whole property: refusing to run uncontained is only honest if it
// costs nothing that was already working.
//
// Arming can fail transiently - a fork returns EAGAIN on a machine out of
// process slots, and guards are one per bounded process, eight of them for a
// single brokered tool call. Armed after the workload, that transient failure
// destroyed a healthy provider to honour the refusal, and force-killed it
// without even the graceful period an ordinary stop gives. Armed first, there
// is nothing running to destroy.
func TestAGuardThatCannotBeArmedRefusesBeforeTheWorkloadStarts(t *testing.T) {
	requireBoundedProcess(t)
	restore := ownerDeathGuardShell
	ownerDeathGuardShell = filepath.Join(t.TempDir(), "no-such-shell")
	t.Cleanup(func() { ownerDeathGuardShell = restore })

	marker := filepath.Join(t.TempDir(), "workload-ran")
	cmd := exec.Command("sh", "-c", ": > "+marker)
	_, err := runBoundedProcess(context.Background(), cmd, 50*time.Millisecond)
	if err == nil {
		t.Fatal("a workload was run with containment that could not be armed")
	}
	if cmd.Process != nil {
		t.Fatalf("the workload was started and then killed rather than refused before it started: pid %d", cmd.Process.Pid)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the refused workload ran far enough to touch the filesystem")
	}
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
	// The provider starts a tool command in a SESSION of its own and then
	// waits; the tool command keeps writing into the candidate.
	script := "perl -e 'use POSIX; POSIX::setsid(); exec @ARGV' sh -c '" +
		"echo $$ > escaped.pid; while :; do echo x >> escaped.log; sleep 0.05; done' &\n" +
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
	requireBoundedProcess(t)
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl unavailable")
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(candidate, "escaped.pid")
	escapedPID := func() int {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return 0
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return pid
	}
	t.Cleanup(func() {
		if pid := escapedPID(); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

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
	pid := escapedPID()
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatal("precondition: the escaped writer did not survive its owner, so this proves nothing")
	}

	_, err := claimCandidateWriter(candidate)
	var alive *CandidateWriterAliveError
	if !errors.As(err, &alive) || alive.Cause != nil {
		t.Fatalf("a new attempt was allowed to start while a dead owner's writer is still writing the candidate: %v", err)
	}
	if class, ok := candidateGuardFailureClass(err); !ok || RouteFailure(class) != RouteWait {
		t.Fatalf("the refusal is not a typed wait: %q %v", class, ok)
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
		time.Sleep(10 * time.Millisecond)
	}
}

// The provider adapter takes the claim before dispatch: a candidate whose lock
// is held is refused without the provider ever being invoked.
func TestAProviderIsNotDispatchedIntoALockedCandidate(t *testing.T) {
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
