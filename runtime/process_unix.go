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
	// THE GUARD IS ARMED BEFORE THERE IS ANYTHING TO GUARD. Arming can fail -
	// a fork returns EAGAIN on a machine out of process slots, and guards are
	// one per bounded process - and refusing to run uncontained is only an
	// honest refusal if it costs nothing that was already working. Armed after
	// the workload, the same refusal had to destroy a healthy provider to
	// honour itself, which punished a transient fork failure exactly as
	// harshly as a hostile process.
	own, stopGuard, err := armOwnerDeathGuard(grace)
	if err != nil {
		return err
	}
	defer stopGuard()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Naming the group is one write on a pipe that is already open. The
	// interval in which this process's death would still orphan the group is
	// therefore that write, not a fork and an exec: sub-millisecond either way,
	// but this is the shorter of the two and it is the one that is not subject
	// to fork pressure.
	own(cmd.Process.Pid)
	owned := newOwnedProcessSet(cmd.Process.Pid)
	defer owned.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
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
//
// It is a var only so a test can point it at nothing and drive the real
// refusal, rather than asserting against a hand-built failure.
var ownerDeathGuardShell = "/bin/sh"

// armOwnerDeathGuard starts a small out-of-process guard that terminates a
// process group when THIS process dies, however it dies. own names the group
// it guards; stop disarms it.
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
// stop sequence for the workload never reaches it. stop disarms it on every
// path this process survives; the pipe is closed only after the guard is dead,
// because closing it first is exactly how the guard is told to fire.
//
// IT IS WEAKER THAN THE IN-PROCESS CONTAINMENT IT STANDS IN FOR, ON LINUX. All
// it can do from another process is signal the group, so a descendant that
// called setsid(2) survives an owner death here, while ownedProcessSet would
// have caught it from /proc with its start-time identity intact. On darwin and
// the BSDs there is no asymmetry at all: process_owned_unix.go performs the
// same bare group kill. Reproducing the /proc walk in shell is not the way to
// close that gap; a guard told which descendants to signal would be.
func armOwnerDeathGuard(grace time.Duration) (own func(int), stop func(), err error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("arming the owner-death guard: %w", err)
	}
	// The first read takes the group id from this process; the second blocks
	// until end-of-file, which only the death of this process can produce. A
	// first read that ends in end-of-file means this process died before it had
	// a group to name, and the guard must then signal nothing at all. The
	// signals go to the negative pgid, so the whole owned group is addressed
	// rather than the single root the workload may have already forked away
	// from. Nothing in the script is interpolated but the grace period, which
	// is formatted from a duration and cannot carry a metacharacter.
	script := "trap '' TERM HUP INT\n" +
		"IFS= read -r pgid <&3 || exit 0\n" +
		"IFS= read -r _ <&3\n" +
		"kill -TERM -\"$pgid\" 2>/dev/null\n" +
		"sleep " + strconv.FormatFloat(grace.Seconds(), 'f', 3, 64) + " 2>/dev/null\n" +
		"kill -KILL -\"$pgid\" 2>/dev/null\n"
	guard := exec.Command(ownerDeathGuardShell, "-c", script)
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	guard.ExtraFiles = []*os.File{read} // fd 3 in the guard
	// A fixed minimal environment: the guard needs to resolve `sleep` and
	// nothing else, and it must not carry this process's environment - which
	// holds provider credentials - into a shell that never needed them.
	guard.Env = []string{"PATH=/usr/bin:/bin"}
	if err := guard.Start(); err != nil {
		_, _ = read.Close(), write.Close()
		return nil, nil, fmt.Errorf("arming the owner-death guard: %w", err)
	}
	// The parent has no use for the read end, and holding it would cost a
	// descriptor per bounded process.
	_ = read.Close()
	own = func(pgid int) {
		// A failed write means the guard is already gone, which is the same
		// loss of containment a failed fork would have been - but the workload
		// is running by now, and killing it here would reintroduce exactly the
		// punishment this ordering exists to remove.
		_, _ = write.WriteString(strconv.Itoa(pgid) + "\n")
	}
	return own, func() {
		_ = guard.Process.Kill()
		_ = guard.Wait()
		_ = write.Close()
	}, nil
}
