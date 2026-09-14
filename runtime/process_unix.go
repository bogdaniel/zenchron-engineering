//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// runBoundedProcess owns a new child process group. On Linux it additionally
// records descendants while the root is alive so that a child which calls
// setsid(2) cannot escape the runtime-owned execution merely by leaving that
// process group. The recorded identity includes the process start time, so a
// recycled host PID is never signalled.
//
// Everything below is containment THIS process performs. It therefore covers
// only the deaths this process survives - a cancelled context, an expired
// authority, a drained shutdown. It cannot cover the death of this process
// itself, which is what armOwnerDeathGuard exists for.
func runBoundedProcess(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	cmd.Cancel = nil // CommandContext's single-child kill is insufficient here.
	// Last-resort unblock: if a descendant escapes the owned-set kill below and
	// keeps holding the inherited stdout/stderr pipe, os/exec's WaitDelay closes
	// those pipes so Wait returns. Not the primary containment mechanism — the
	// explicit stop sequence below can itself take up to ~2*grace.
	cmd.WaitDelay = 3 * grace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	owned := newOwnedProcessSet(cmd.Process.Pid)
	defer owned.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Arming is not atomic with spawning: a SIGKILL delivered to this process
	// between Start and here still orphans the group. The window is the cost of
	// one fork/exec rather than the whole lifetime of the workload.
	stopGuard, err := armOwnerDeathGuard(cmd.Process.Pid, grace)
	if err != nil {
		// Containment that could not be armed must not be claimed. The workload
		// is stopped rather than allowed to run unguarded, which is the same
		// refusal this file already makes on Windows.
		owned.ForceKill()
		<-done
		return err
	}
	defer stopGuard()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Snapshot before signalling the root: otherwise a detached descendant
		// could be reparented before it is identified. Signal errors are benign
		// here because the root may have won the race and exited naturally.
		owned.GracefulStop(grace)
		select {
		case err := <-done:
			return err
		case <-time.After(grace):
			owned.ForceKill()
			return <-done
		}
	}
}

// ownerDeathGuardShell is an absolute path on purpose. The guard is a
// containment mechanism, and resolving it through PATH would let the
// environment this runtime was started in choose what contains the runtime's
// own children. /bin/sh is the path POSIX requires a shell to be at.
const ownerDeathGuardShell = "/bin/sh"

// armOwnerDeathGuard starts a small out-of-process guard that terminates pgid
// when THIS process dies, however it dies.
//
// A process killed with SIGKILL executes nothing: no defer, no signal handler,
// no shutdown path. So the runtime cannot be the thing that stops its own
// provider when the runtime is the thing that died, and every handler-based
// repair is wrong for that case by construction. What the kernel does perform
// unconditionally for a dead process is close its descriptors, and that close
// is the signal used here: the guard blocks reading the read end of a pipe
// whose only write end lives in this process, so the guard is released the
// instant this process ceases to exist - drained, crashed, or killed -9.
//
// The guard sits in its OWN process group and ignores SIGTERM, so the group
// kill it performs cannot silence it before it escalates, and the runtime's own
// stop sequence for the workload never reaches it. The returned stop disarms it
// on every path this process survives; the pipe is closed only after the guard
// is dead, because closing it first is exactly how the guard is told to fire.
func armOwnerDeathGuard(pgid int, grace time.Duration) (func(), error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("arming the owner-death guard: %w", err)
	}
	group := strconv.Itoa(pgid)
	// read blocks until end-of-file, which only the death of this process can
	// produce. The signals then go to the negative pgid, so the whole owned
	// group is addressed rather than the single root the orphan may have
	// already forked away from.
	script := "trap '' TERM HUP INT\n" +
		"IFS= read -r _ <&3\n" +
		"kill -TERM -" + group + " 2>/dev/null\n" +
		"sleep " + strconv.FormatFloat(grace.Seconds(), 'f', 3, 64) + " 2>/dev/null\n" +
		"kill -KILL -" + group + " 2>/dev/null\n"
	guard := exec.Command(ownerDeathGuardShell, "-c", script)
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	guard.ExtraFiles = []*os.File{read} // fd 3 in the guard
	// A fixed minimal environment: the guard needs to resolve `sleep` and
	// nothing else, and it must not carry this process's environment - which
	// holds provider credentials - into a shell it did not need them for.
	guard.Env = []string{"PATH=/usr/bin:/bin"}
	if err := guard.Start(); err != nil {
		_, _ = read.Close(), write.Close()
		return nil, fmt.Errorf("arming the owner-death guard: %w", err)
	}
	// The parent must not retain the read end: while any writer-visible copy of
	// the read end exists here it changes nothing, but holding it keeps a
	// descriptor per bounded process for no reason.
	_ = read.Close()
	return func() {
		_ = guard.Process.Kill()
		_ = guard.Wait()
		_ = write.Close()
	}, nil
}
