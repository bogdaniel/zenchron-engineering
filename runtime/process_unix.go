//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// runBoundedProcess owns a new child process group. On Linux it additionally
// records descendants while the root is alive so that a child which calls
// setsid(2) cannot escape the runtime-owned execution merely by leaving that
// process group. The recorded identity includes the process start time, so a
// recycled host PID is never signalled.
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
