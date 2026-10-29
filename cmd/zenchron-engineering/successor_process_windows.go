//go:build windows

package main

// THE SAME TWO OPERATIONS, WHERE THERE ARE NO PROCESS GROUPS TO USE.
//
// This file exists so the cross-build lane is honest rather than so the
// handshake works here: the transport hands the successor descriptors 3 and 4
// through ExtraFiles, which Windows does not support, so a live transition is a
// Unix operation today. What must not happen is the platform difference being
// discovered as a compile error in a lane nobody ran locally - or, worse,
// papered over by weakening that lane.
//
// The successor is therefore started and stopped as a single process here. It
// is the honest degradation: killing one process without its children is worse
// than killing a group, and pretending a group exists would be worse than
// both.

import "os/exec"

// configureSuccessorProcess has nothing to configure on this platform.
func configureSuccessorProcess(*exec.Cmd) {}

// killSuccessorProcessTree stops the successor process itself.
func killSuccessorProcessTree(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if err := command.Process.Kill(); err != nil {
		return err
	}
	_, err := command.Process.Wait()
	return err
}
