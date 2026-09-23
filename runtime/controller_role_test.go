package runtime

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// roleHolderEnv makes this test binary re-executable as a process that takes the
// controller role and then waits to be killed. It is the only way to prove what
// happens when a HOLDER DIES rather than releases: a goroutine cannot be
// SIGKILLed out from under a lock.
const roleHolderEnv = "ZENCHRON_TEST_HOLD_CONTROLLER_ROLE"

func TestMain(m *testing.M) {
	// The crash suite's child controller. It is dispatched before anything
	// else for the same reason the role holder is: this process is not running
	// tests, it is being a controller for a parent that is.
	if os.Getenv(controllerChildEnv) != "" {
		runControllerChild()
		return
	}
	if stateDir := os.Getenv(roleHolderEnv); stateDir != "" {
		if _, err := AcquireControllerRole(stateDir); err != nil {
			os.Stderr.WriteString("holder could not acquire: " + err.Error())
			os.Exit(2)
		}
		os.Stdout.WriteString("held\n")
		_ = os.Stdout.Sync()
		// Wait to be killed by blocking on stdin, which the parent holds open.
		// NOT select{}: with no other goroutines that is a deadlock the Go
		// runtime detects and panics on, so the holder would exit on its own
		// and free the role while a test still believed it was held - a flaky
		// harness that looks like a flaky lock.
		//
		// The lock is deliberately never released here: what this process
		// exists to prove is that the kernel releases it anyway.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// holdRoleInAnotherProcess starts a real process holding the role and returns
// it once the role is provably taken.
func holdRoleInAnotherProcess(t *testing.T, stateDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(), roleHolderEnv+"="+stateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The holder blocks reading this pipe, so it stays alive until the test
	// kills it rather than until the Go runtime notices it has nothing to do.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	ready := make([]byte, len("held\n"))
	if _, err := stdout.Read(ready); err != nil {
		t.Fatalf("the holder never reported taking the role: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if held, decided := ControllerRoleHeld(stateDir); held && decided {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the role never read as held while another process held it")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TWO CONTROLLERS, ONE ROLE. This is the property the control socket was
// standing in for, and it is now the kernel's answer rather than a filesystem
// coincidence.
func TestControllerRoleExcludesASecondLiveProcess(t *testing.T) {
	state := t.TempDir()
	holdRoleInAnotherProcess(t, state)

	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("a second process took the controller role")
	} else if !strings.Contains(err.Error(), "another live process") {
		t.Fatalf("error = %v, want one naming the live holder", err)
	}
}

// A HOLDER THAT DIES RELEASES. No metadata is consulted, no pid is compared,
// and nothing has to be cleaned up: the descriptor dies with the process.
func TestControllerRoleIsFreedWhenTheHolderIsKilled(t *testing.T) {
	state := t.TempDir()
	holder := holdRoleInAnotherProcess(t, state)

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lock, err := AcquireControllerRole(state)
		if err == nil {
			t.Cleanup(func() { _ = lock.Release() })
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the role was never freed by the holder's death: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// THE FILE IS NOT THE ROLE. A lock path left behind by a previous controller -
// or created by anything else - grants and denies nothing.
func TestControllerRoleFileExistenceIsNotEvidence(t *testing.T) {
	state := t.TempDir()
	path := ControllerRoleLockPath(state)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("left over"), 0o600); err != nil {
		t.Fatal(err)
	}
	if held, decided := ControllerRoleHeld(state); held || !decided {
		t.Fatalf("an unheld lock file read as held=%v decided=%v", held, decided)
	}
	lock, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatalf("a leftover file blocked the role: %v", err)
	}
	// And release does not unlink it: a second process creating a fresh file at
	// the same name while a third holds the unlinked inode would be two holders
	// of one role.
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release removed the lock inode: %v", err)
	}
}

// Release then acquire is the handoff's shape, and it works in the one order
// the protocol uses.
func TestControllerRoleTransfersOnlyAfterRelease(t *testing.T) {
	state := t.TempDir()
	predecessor, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("the role was taken twice inside one process")
	}
	if err := predecessor.Release(); err != nil {
		t.Fatal(err)
	}
	// A released lease authorizes nothing, and that is the only question
	// worth asking about it: there is no Held() to consult.
	if err := predecessor.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("a released lease still authorized an operation")
	}
	successor, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatalf("the successor could not take the released role: %v", err)
	}
	t.Cleanup(func() { _ = successor.Release() })
	// Idempotent release, so a deferred shutdown path is safe.
	if err := predecessor.Release(); err != nil {
		t.Fatal(err)
	}
}

// Racing acquirers: exactly one wins, whatever the interleaving.
func TestOnlyOneAcquirerWinsARace(t *testing.T) {
	state := t.TempDir()
	const racers = 16
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	won := make(chan *ControllerRoleLease, racers)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if lock, err := AcquireControllerRole(state); err == nil {
				won <- lock
			}
		}()
	}
	start.Done()
	done.Wait()
	close(won)
	holders := 0
	for lock := range won {
		holders++
		_ = lock.Release()
	}
	if holders != 1 {
		t.Fatalf("%d racers took the controller role, want exactly 1", holders)
	}
}

// THE ROLE IS NOT THE SOCKET AND NOT ACTIVATION. Holding it grants neither the
// right to serve nor any claim about which generation is active.
func TestControllerRoleGrantsNeitherServiceNorActivation(t *testing.T) {
	state := t.TempDir()
	lock, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	// A supervisor constructed for the successor's half holds the role and
	// still admits nothing.
	gate := newWorkAdmissionGate(false)
	if gate.permitted() {
		t.Fatal("holding the controller role opened work admission")
	}
	// A control socket file is likewise not the role: nothing here consults it.
	socket := ControlSocketPath(state)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if held, decided := ControllerRoleHeld(state); !held || !decided {
		t.Fatal("the role stopped reading as held because a socket file appeared")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if held, decided := ControllerRoleHeld(state); !held || !decided {
		t.Fatal("removing a socket file released the controller role")
	}
}

// The anchor is refused rather than followed when it is a symlink: taking the
// role on whatever inode a link names is the split-inode failure by another
// route.
func TestControllerRoleRefusesASymlinkedAnchor(t *testing.T) {
	state := t.TempDir()
	path := ControllerRoleLockPath(state)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "somewhere-else")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("the role was taken through a symlinked anchor")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want one naming the symlink", err)
	}
}

// The two locks answer different questions, and the instance lock never
// answered the role's. Two controllers with different instance identities take
// two different instance locks and exclude each other from nothing.
func TestInstanceLockIsNotTheRoleLock(t *testing.T) {
	state := t.TempDir()
	// Two controller processes on one host: different pids, therefore
	// different owner identities, therefore DIFFERENT instance lock paths.
	first, err := AcquireControllerInstanceLock(state, "host/1111/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })
	second, err := AcquireControllerInstanceLock(state, "host/2222/")
	if err != nil {
		t.Fatalf("two instances could not both take their own instance locks: %v", err)
	}
	t.Cleanup(func() { _ = second.Release() })
	if first.Path() == second.Path() {
		t.Fatal("two instance identities shared one lock path")
	}

	// Meanwhile the role admits exactly one.
	role, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = role.Release() })
	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("the role admitted a second holder while two instance locks were held")
	}
}
