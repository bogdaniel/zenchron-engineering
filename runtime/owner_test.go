package runtime

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOwnershipLockHolderHelper is not a test. It is the child process every
// test below drives: a REAL process that takes a REAL kernel lock, so killing
// it exercises the kernel's release path rather than an in-process fiction.
// It prints the owner identity it claimed and then holds ownership until its
// stdin closes (graceful release) or it is killed (kernel release).
func TestOwnershipLockHolderHelper(t *testing.T) {
	if os.Getenv("ZENCHRON_OWNER_LOCK_HELPER") != "1" {
		t.Skip("helper process, driven by the ownership lock tests")
	}
	owner := os.Getenv("ZENCHRON_OWNER_LOCK_OWNER")
	if owner == "" {
		owner = NewRuntimeOwner()
	}
	lock, err := AcquireOwnershipLock(os.Getenv("ZENCHRON_OWNER_LOCK_STATE_DIR"), owner)
	if err != nil {
		fmt.Fprintln(os.Stdout, "error "+err.Error())
		os.Exit(3)
	}
	fmt.Fprintln(os.Stdout, owner)
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lock.Release(); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

type lockHolder struct {
	owner   string
	command *exec.Cmd
	stdin   io.WriteCloser
}

// startLockHolder returns only once the child has actually taken the lock: the
// owner line it prints is the readiness signal, so no test waits on the clock.
func startLockHolder(t *testing.T, stateDir, owner string) *lockHolder {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=TestOwnershipLockHolderHelper")
	command.Env = append(os.Environ(),
		"ZENCHRON_OWNER_LOCK_HELPER=1",
		"ZENCHRON_OWNER_LOCK_STATE_DIR="+stateDir,
		"ZENCHRON_OWNER_LOCK_OWNER="+owner,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	holder := &lockHolder{command: command, stdin: stdin}
	t.Cleanup(func() { _ = stdin.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the lock holder never reported readiness: %v", err)
	}
	holder.owner = strings.TrimSpace(line)
	if holder.owner == "" || strings.HasPrefix(holder.owner, "error ") {
		t.Fatalf("the lock holder failed to take the lock: %q", holder.owner)
	}
	return holder
}

// kill removes ownership the way a crash does: SIGKILL runs no cleanup code, so
// only the kernel can release the lock. Wait reaps the process, after which its
// descriptors are provably closed - no polling window is needed.
func (h *lockHolder) kill(t *testing.T) {
	t.Helper()
	if err := h.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = h.command.Wait()
}

func (h *lockHolder) shutDown(t *testing.T) {
	t.Helper()
	if err := h.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.command.Wait(); err != nil {
		t.Fatalf("the lock holder did not release ownership cleanly: %v", err)
	}
}

// leaseOperation plans one operation and leases it to owner through the real
// scheduler, so the takeover assertions below run the production path.
func leaseOperation(t *testing.T, store OperationStore, clock Clock, owner string) RunOperation {
	t.Helper()
	scheduler := Scheduler{Store: store, Clock: clock, Owner: owner, LeaseDuration: time.Minute,
		Liveness: OwnerLivenessFunc(func(string) bool { return true })}
	if _, _, err := scheduler.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "key", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	leased, err := scheduler.Next("r")
	if err != nil || leased == nil {
		t.Fatalf("the operation was not leased: %v %v", leased, err)
	}
	if _, err := scheduler.Start(leased.ID); err != nil {
		t.Fatal(err)
	}
	return *leased
}

func takeoverScheduler(store OperationStore, clock Clock, stateDir string) Scheduler {
	return Scheduler{Store: store, Clock: clock, Owner: "successor", LeaseDuration: time.Minute,
		Liveness: NewLockOwnerLiveness(stateDir)}
}

// A live owner keeps its operation both before and after the heartbeat expires.
// Wall time is not death.
func TestLiveLockHolderKeepsItsLease(t *testing.T) {
	stateDir := t.TempDir()
	holder := startLockHolder(t, stateDir, "")
	clock := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	leaseOperation(t, store, clock, holder.owner)
	taker := takeoverScheduler(store, clock, stateDir)

	if got, err := taker.Next("r"); err != nil || got != nil {
		t.Fatalf("an unexpired lease of a live owner was stolen: %v %v", got, err)
	}
	clock.now = clock.now.Add(time.Hour)
	if !taker.Liveness.Alive(holder.owner) {
		t.Fatal("a process holding its ownership lock must be reported alive")
	}
	if got, err := taker.Next("r"); err != nil || got != nil {
		t.Fatalf("an expired lease of a LIVE owner was stolen: %v %v", got, err)
	}
}

// A crash releases ownership with no cooperation from the dead process, which
// is the only thing that makes the expired lease takeable.
func TestKilledLockHolderReleasesOwnership(t *testing.T) {
	stateDir := t.TempDir()
	holder := startLockHolder(t, stateDir, "")
	clock := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	operation := leaseOperation(t, store, clock, holder.owner)
	taker := takeoverScheduler(store, clock, stateDir)

	holder.kill(t)
	if taker.Liveness.Alive(holder.owner) {
		t.Fatal("a killed owner must not be reported alive")
	}
	clock.now = clock.now.Add(time.Hour)
	got, err := taker.Next("r")
	if err != nil || got == nil || got.ID != operation.ID {
		t.Fatalf("the dead owner's expired lease was not taken over: %v %v", got, err)
	}
	if got.Lease == nil || got.Lease.Owner != "successor" {
		t.Fatalf("unexpected lease after takeover: %+v", got.Lease)
	}
	// Death must be provable before expiry too; only CanAcquire adds the clock.
	if CanAcquire(operation, time.Unix(100, 0), false) {
		t.Fatal("a dead owner's unexpired lease must still not be takeable")
	}
}

// Ownership is the lock, never the PID. Two identities sharing one live PID -
// exactly what PID reuse produces - must be judged independently.
func TestPIDReuseProvesNeitherDeathNorLife(t *testing.T) {
	stateDir := t.TempDir()
	holder := startLockHolder(t, stateDir, "")
	host, pid, token, ok := parseOwner(holder.owner)
	if !ok {
		t.Fatalf("the holder reported an unparseable owner: %q", holder.owner)
	}
	recycled := fmt.Sprintf("%s/%d/%s", host, pid, token+"-earlier-process")
	liveness := NewLockOwnerLiveness(stateDir)

	if !liveness.Alive(holder.owner) {
		t.Fatal("the identity that holds the lock must be alive")
	}
	if liveness.Alive(recycled) {
		t.Fatal("a dead identity must not be revived by a live process reusing its PID")
	}
	// Probing the spoof must not have disturbed the real holder's lock.
	if !liveness.Alive(holder.owner) {
		t.Fatal("probing a spoofed identity must not release the real owner's lock")
	}
	holder.shutDown(t)
	if liveness.Alive(holder.owner) {
		t.Fatal("a released lock must not report its owner alive")
	}
}

// The file is not the lock. A file left behind by a crash must never outlive
// the process that made it.
func TestStaleLockFileDoesNotBlockTakeover(t *testing.T) {
	stateDir := t.TempDir()
	dead := fmt.Sprintf("%s/%d/stale", ownerHost(), os.Getpid()+1)
	lock, err := AcquireOwnershipLock(stateDir, dead)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the descriptor without removing the file, as a crash does.
	if err := lock.file.Close(); err != nil {
		t.Fatal(err)
	}
	lock.file = nil
	if _, err := os.Stat(lock.Path()); err != nil {
		t.Fatalf("the stale lock file must still exist for this test: %v", err)
	}

	clock := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	operation := leaseOperation(t, store, clock, dead)
	taker := takeoverScheduler(store, clock, stateDir)
	if taker.Liveness.Alive(dead) {
		t.Fatal("a lock file with no holder must not be read as liveness")
	}
	clock.now = clock.now.Add(time.Hour)
	got, err := taker.Next("r")
	if err != nil || got == nil || got.ID != operation.ID {
		t.Fatalf("a stale lock file blocked takeover: %v %v", got, err)
	}
}

// Nothing observable here proves a remote process is gone, and an unusable lock
// directory answers no question at all. Both must refuse takeover.
func TestUnprovableOwnersAreTreatedAsAlive(t *testing.T) {
	stateDir := t.TempDir()
	foreign := "some-other-host/4242/token"
	// Even a lock file that IS acquirable says nothing about another host.
	if err := os.MkdirAll(filepath.Join(stateDir, "locks", "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerLockPath(stateDir, foreign), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !NewLockOwnerLiveness(stateDir).Alive(foreign) {
		t.Fatal("a foreign-host owner cannot be proven dead and must be treated as alive")
	}

	// An unusable lock location stands in for every platform and filesystem
	// that cannot answer, including one with no advisory lock at all.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "locks"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	local := fmt.Sprintf("%s/%d/token", ownerHost(), os.Getpid()+1)
	if !NewLockOwnerLiveness(blocked).Alive(local) {
		t.Fatal("an unanswerable lock probe must refuse takeover, never fail open")
	}

	clock := &fakeClock{now: time.Unix(100, 0)}
	store := NewMemoryOperationStore()
	leaseOperation(t, store, clock, local)
	clock.now = clock.now.Add(time.Hour)
	if got, err := takeoverScheduler(store, clock, blocked).Next("r"); err != nil || got != nil {
		t.Fatalf("an unanswerable probe allowed a takeover: %v %v", got, err)
	}
}

// Two runtimes cannot share one identity, and a released identity is reusable.
func TestAcquireOwnershipLockRefusesADoubleClaim(t *testing.T) {
	stateDir := t.TempDir()
	holder := startLockHolder(t, stateDir, "")
	if _, err := AcquireOwnershipLock(stateDir, holder.owner); err == nil {
		t.Fatal("a second claim on a held ownership identity must fail")
	}
	holder.shutDown(t)
	lock, err := AcquireOwnershipLock(stateDir, holder.owner)
	if err != nil {
		t.Fatalf("a released identity must be reclaimable: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release must be idempotent: %v", err)
	}
	var absent *OwnershipLock
	if err := absent.Release(); err != nil {
		t.Fatalf("Release must be nil-safe: %v", err)
	}
	if _, err := AcquireOwnershipLock("", NewRuntimeOwner()); err == nil {
		t.Fatal("a lock with no state dir must fail")
	}
	if _, err := AcquireOwnershipLock(stateDir, "nonsense"); err == nil {
		t.Fatal("a lock for a non-identity must fail")
	}
}
