//go:build !windows

package main

// The process-group half of controller_succession_test.go: signalling a group
// is Unix-only, like successor_process_unix.go that creates it.

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ABANDONING STOPS THE WHOLE PROCESS GROUP. A successor left running would be
// a second controller process waiting on a handshake nobody will complete.
func TestAbandoningASuccessorStopsIt(t *testing.T) {
	successor, command := fakeSuccessorScript(t, `
		printf 'identity {}\n' >&4
		sleep 300`)
	if _, err := successor.Identify(); err != nil {
		t.Fatalf("HARNESS PRECONDITION: %v", err)
	}
	if err := successor.Abandon(); err != nil {
		t.Fatalf("the successor could not be abandoned: %v", err)
	}
	// NOTHING IN THE GROUP IS STILL RUNNING.
	//
	// Not "the group id cannot be signalled", which is a different and weaker
	// question: a killed process whose exit status nobody has collected stays
	// signallable as a zombie, and its group with it. That happens wherever
	// orphans are not reaped - the container the selfhost harness verifies
	// candidates in, for one - and it made this test report a successor that
	// had in fact been stopped. What the abandon must achieve is that no
	// process from it can still execute.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processGroupRunning(command.Process.Pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the abandoned successor's process group is still running")
}

// processGroupRunning reports whether any process in a group is still
// executing. A group whose only members are zombies is not.
func processGroupRunning(pgid int) bool {
	if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
		return false
	}
	// EVERY PROCESS, FILTERED HERE. `ps -g` selects by session on Linux and by
	// process group on macOS, so asking ps to do the filtering is asking two
	// different questions depending on the host; listing pgid and state and
	// matching in Go asks one.
	listing, err := exec.Command("sh", "-c", "ps -e -o pgid=,state=").Output()
	if err != nil {
		// ps could not say, and the signal probe said the group exists.
		// Believing the stricter answer is the fail-closed direction for a
		// test asserting that something was stopped.
		return true
	}
	group := strconv.Itoa(pgid)
	for _, line := range strings.Split(string(listing), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != group {
			continue
		}
		if !strings.HasPrefix(fields[1], "Z") {
			return true
		}
	}
	return false
}
