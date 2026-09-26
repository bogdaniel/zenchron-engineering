//go:build !windows

package main

// PROCESS-GROUP LIFECYCLE, on the systems that have one.
//
// The successor is about to outlive the process starting it, and until the
// point of no return it may have to be stopped. Both are process-GROUP
// questions rather than process questions: a controller that started anything
// of its own must not be half-killed, and a signal aimed at the predecessor's
// terminal must not reach a controller mid-activation.
//
// It is isolated here because it is genuinely platform-specific. The rest of
// the handshake is portable; SysProcAttr.Setpgid and killing a negative pid are
// not, and the build lane that cross-compiles for Windows is the thing that
// says so.

import (
	"os/exec"
	"syscall"
)

// configureSuccessorProcess puts the successor in its own process group.
func configureSuccessorProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killSuccessorProcessTree stops the successor and anything it started.
//
// A process group that has already gone is not an error: the successor may
// have exited on its own, and this is called precisely when nobody is going to
// use it either way.
func killSuccessorProcessTree(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	_, err := command.Process.Wait()
	return err
}
