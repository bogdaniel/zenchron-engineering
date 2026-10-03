//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
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
//
// It returns the TerminationOwner, committed exactly once (#213):
//
//   - the root process exiting before any external event initiated its
//     termination is OwnerProviderExited, however long descendants then hold
//     its output pipes open;
//   - otherwise the owner is the cause of the context at the instant THIS
//     function began terminating the group (ownerOfCancellation), and it
//     stays the owner through the grace period whatever arrives meanwhile.
//
// PROCESS EXIT IS OBSERVED SEPARATELY FROM PIPE DRAIN. With a non-*os.File
// Stdout/Stderr, os/exec creates the pipes itself and Cmd.Wait returns only
// after its copy goroutines finish, so a background descendant holding the
// pipe delays Wait past the root's exit - and a stop landing in that window
// would look like the thing that ended the process. ownOutputPipes therefore
// hands the child *os.File pipe ends: Cmd.Wait then waits only for the root
// (one reaper, no double wait), and this function drains the pipes itself,
// after exit is known, with the same bound WaitDelay applied before.
func runBoundedProcess(ctx context.Context, cmd *exec.Cmd, grace time.Duration) (TerminationOwner, error) {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	cmd.Cancel = nil // CommandContext's single-child kill is insufficient here.
	// Last-resort unblock for the root itself: the pipes are drained below.
	cmd.WaitDelay = 3 * grace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pipes, err := ownOutputPipes(cmd)
	if err != nil {
		return OwnerUndecided, err
	}
	defer pipes.closeReaders()
	// THE GUARD IS ARMED BEFORE THERE IS ANYTHING TO GUARD. Arming can fail -
	// a fork returns EAGAIN on a machine out of process slots, and guards are
	// one per bounded process - and refusing to run uncontained is only an
	// honest refusal if it costs nothing that was already working. Armed after
	// the workload, the same refusal had to destroy a healthy provider to
	// honour itself, which punished a transient fork failure exactly as
	// harshly as a hostile process.
	own, stopGuard, err := armOwnerDeathGuard(grace)
	if err != nil {
		pipes.closeWriters()
		return OwnerUndecided, err
	}
	defer stopGuard()
	if err := cmd.Start(); err != nil {
		pipes.closeWriters()
		// A context already ended refuses the start (os/exec returns its
		// Err before forking). No process existed, so no provider
		// termination is attributed: the executor settles "not started" and
		// records the cause as it stands at the refusal.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return OwnerNotStarted, &ProviderNotStartedError{Cause: context.Cause(ctx)}
		}
		return OwnerUndecided, err
	}
	pipes.start()
	// Naming the group is one write on a pipe that is already open. The
	// interval in which this process's death would still orphan the group is
	// therefore that write, not a fork and an exec: sub-millisecond either way,
	// but this is the shorter of the two and it is the one that is not subject
	// to fork pressure.
	own(cmd.Process.Pid)
	owned := newOwnedProcessSet(cmd.Process.Pid)
	defer owned.Close()
	exited := make(chan error, 1)
	// THE ONLY REAPER. Everything else observes exit without reaping.
	go func() {
		holdReaper()
		exited <- cmd.Wait()
	}()
	stop := func() error {
		// Snapshot before signalling the root: otherwise a detached descendant
		// could be reparented before it is identified. Signal errors are benign
		// here because the root may have won the race and exited naturally.
		owned.GracefulStop(grace)
		select {
		case err := <-exited:
			return err
		case <-time.After(grace):
			owned.ForceKill()
			return <-exited
		}
	}
	// settle is called on exactly one of the paths below, once, before any
	// signal is sent; the final settle(OwnerUndecided) only reads it back.
	var ownership terminalOwnership
	var waitErr error
	select {
	case waitErr = <-exited:
		ownership.settle(OwnerProviderExited)
	case <-ctx.Done():
		// THE LINEARIZATION POINT. The reaper may not have delivered yet, so
		// the KERNEL is asked, without reaping, whether the root has already
		// exited. Only a root still running is terminated for the context's
		// cause; one that already exited owns its own ending.
		//
		// A stopped root is NOT exited (rootExited accepts only a terminal
		// si_code), so the context's cause wins arbitration before any
		// signal is sent.
		if probeRootExited(cmd.Process.Pid) {
			// provider_exited is linearized here and is never rewritten. The
			// wait on the reaper is still BOUNDED: if it has not delivered
			// within grace, the group is terminated and reaped as CLEANUP
			// only, so a stalled reap can never hold the controller.
			ownership.settle(OwnerProviderExited)
			select {
			case waitErr = <-exited:
			case <-time.After(grace):
				waitErr = stop()
			}
		} else {
			ownership.settle(ownerOfCancellation(ctx))
			waitErr = stop()
		}
	}
	// The owner is committed. What remains is draining the pipes, which a
	// descendant may still hold; a cancellation now only cleans up.
	cleanup := func() {
		owned.GracefulStop(grace)
		select {
		case <-pipes.done:
		case <-time.After(grace):
			owned.ForceKill()
		}
	}
	if err := pipes.drain(ctx, 3*grace, cleanup); err != nil && waitErr == nil {
		waitErr = err
	}
	return ownership.settle(OwnerUndecided), waitErr
}

// ownedPipes are the child's stdout/stderr pipes, created here rather than by
// os/exec so that Cmd.Wait observes the root's exit without also waiting for
// every descendant that inherited the write end (#213).
type ownedPipes struct {
	readers, writers []*os.File
	targets          []io.Writer
	done             chan struct{}
}

// ownOutputPipes replaces each non-file Stdout/Stderr writer with the write end
// of a pipe this function owns. A writer shared by both streams shares one
// pipe, as os/exec does, so the target never sees concurrent writes.
func ownOutputPipes(cmd *exec.Cmd) (*ownedPipes, error) {
	p := &ownedPipes{done: make(chan struct{})}
	var shared *os.File
	for i, slot := range []*io.Writer{&cmd.Stdout, &cmd.Stderr} {
		target := *slot
		if target == nil {
			continue
		}
		if _, isFile := target.(*os.File); isFile {
			continue
		}
		if i == 1 && shared != nil && sameWriter(cmd.Stderr, p.targets[0]) {
			*slot = shared
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			p.closeWriters()
			p.closeReaders()
			return nil, err
		}
		p.readers, p.writers, p.targets = append(p.readers, r), append(p.writers, w), append(p.targets, target)
		*slot = w
		if i == 0 {
			shared = w
		}
	}
	return p, nil
}

func sameWriter(a, b io.Writer) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// start closes this process's copies of the write ends - the child holds its
// own - and copies each pipe into its target until every writer has closed.
func (p *ownedPipes) start() {
	p.closeWriters()
	var copying sync.WaitGroup
	for i := range p.readers {
		copying.Add(1)
		go func(r *os.File, target io.Writer) {
			defer copying.Done()
			// A failing target closes the read end, as os/exec does, so the
			// child gets EPIPE instead of blocking on a full pipe.
			if _, err := io.Copy(target, r); err != nil {
				_ = r.Close()
			}
		}(p.readers[i], p.targets[i])
	}
	go func() { copying.Wait(); close(p.done) }()
}

// drain waits for the copies to finish. A descendant may hold the pipes past
// the root's exit: a cancellation meanwhile runs cleanup (it ends the
// descendants, it owns nothing), and after bound the pipes are closed and
// exec.ErrWaitDelay reported, exactly as os/exec's WaitDelay did.
func (p *ownedPipes) drain(ctx context.Context, bound time.Duration, cleanup func()) error {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	cancelled := ctx.Done()
	for {
		select {
		case <-p.done:
			return nil
		case <-cancelled:
			cancelled = nil
			cleanup()
		case <-timer.C:
			p.closeReaders()
			<-p.done
			return exec.ErrWaitDelay
		}
	}
}

func (p *ownedPipes) closeWriters() {
	for _, w := range p.writers {
		_ = w.Close()
	}
	p.writers = nil
}

func (p *ownedPipes) closeReaders() {
	for _, r := range p.readers {
		_ = r.Close()
	}
}

// releaseCandidateWriter ends this process's hold on the candidate writer lock
// once the provider has returned, and reports whether anything still holds it.
// A holder still in the provider's process group is this attempt's own
// leftover - a backgrounded command - and is stopped with the same TERM then
// KILL sequence as any other stop, so it never refuses the next attempt. Only
// a holder that survives that left the group, and that is reported.
//
// A group that no longer exists (ESRCH) is not signalled at all: its holder is
// necessarily outside it, and its id may already be someone else's.
//
// ponytail: a group id reused while the old group still had members cannot be
// told apart here. Linux's owned set is already closed; carrying it through is
// the upgrade if that matters.
func releaseCandidateWriter(lock *os.File, pgid int, grace time.Duration) (escaped bool) {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	path := lock.Name()
	_ = lock.Close()
	if candidateWriterFree(path) {
		return false
	}
	freed := func() bool {
		for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if candidateWriterFree(path) {
				return true
			}
		}
		return false
	}
	groupGone := func() bool { return syscall.Kill(-pgid, 0) == syscall.ESRCH }
	if groupGone() {
		return true
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if freed() {
		return false
	}
	if groupGone() {
		return !candidateWriterFree(path)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return !freed()
}

// probeRootExited is rootExited, as a seam so a test can make the probe
// misreport and prove the bounded fallback.
var probeRootExited = rootExited

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
