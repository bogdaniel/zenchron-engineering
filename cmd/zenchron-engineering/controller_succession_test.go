package main

// THE TRANSPORT, AGAINST A REAL SECOND PROCESS.
//
// The successor half of a live transition cannot be proven here - two
// genuinely different adopted builds are not something a test can produce, and
// runtime/controller_crash_test.go already drives the protocol between two real
// processes with supplied identities. What IS proven here is the part that is
// only real with a second process: descriptors 3 and 4, the framing, what a
// silence looks like, and what happens to a successor that will not be used.

import (
	"encoding/json"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// fakeSuccessorScript is a successor written in shell: it says what it is on
// fd 4, waits for a line on fd 3, and answers. It is deliberately not this
// binary re-executed - the point is to exercise the descriptors across an exec
// boundary, and a shell does that with nothing else attached.
func fakeSuccessorScript(t *testing.T, body string) (runtime.InertSuccessor, *exec.Cmd) {
	t.Helper()
	successor, err := spawnInertSuccessor("/bin/sh", "handoff-1", []string{"-c", body, "successor"})
	if err != nil {
		t.Fatalf("HARNESS PRECONDITION: the fake successor could not be started: %v", err)
	}
	spawned, ok := successor.(*spawnedSuccessor)
	if !ok {
		t.Fatalf("HARNESS PRECONDITION: spawnInertSuccessor returned %T", successor)
	}
	t.Cleanup(func() { _ = successor.Abandon() })
	return successor, spawned.command
}

func announcedBinding(t *testing.T) string {
	t.Helper()
	build := runtime.ControllerBuild{
		Kind: runtime.ControllerAdopted, Version: "main-bbbbbbb",
		SourceRevision: strings.Repeat("b", 40), SourceTree: strings.Repeat("c", 40),
		BinarySHA256: strings.Repeat("d", 64),
	}
	encoded, err := json.Marshal(runtime.ControllerBinding{
		Controller: "zenchron-engineering", Build: &build,
		Config: runtime.ConfigDigest{Global: "global", Repository: "repository"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// THE WHOLE HANDSHAKE, across a process boundary.
func TestTheSuccessorHandshakeCrossesAProcessBoundary(t *testing.T) {
	binding := announcedBinding(t)
	successor, _ := fakeSuccessorScript(t, `
		printf 'identity %s\n' '`+binding+`' >&4
		read -r line <&3
		case "$line" in
			"proceed handoff-1") printf 'active handoff-1\n' >&4 ;;
			*) printf 'error unexpected %s\n' "$line" >&4 ;;
		esac`)

	announced, err := successor.Identify()
	if err != nil {
		t.Fatalf("the successor did not identify itself: %v", err)
	}
	if announced.Build == nil || announced.Build.Version != "main-bbbbbbb" {
		t.Fatalf("the announced generation did not survive the pipe: %+v", announced)
	}
	if announced.Config.Repository != "repository" {
		t.Fatalf("the announced configuration did not survive the pipe: %+v", announced.Config)
	}
	if err := successor.Proceed("handoff-1"); err != nil {
		t.Fatalf("the successor could not be told to proceed: %v", err)
	}
	if err := successor.AwaitActive(); err != nil {
		t.Fatalf("the successor did not report that it is serving: %v", err)
	}
}

// A SUCCESSOR THAT REFUSES SAYS WHY. A predecessor that learned only "it did
// not happen" would have nothing to report and nothing to wait for.
func TestASuccessorRefusalCarriesItsReason(t *testing.T) {
	successor, _ := fakeSuccessorScript(t,
		`printf 'error the operator configuration could not be loaded\n' >&4`)

	_, err := successor.Identify()
	if err == nil || !strings.Contains(err.Error(), "the operator configuration could not be loaded") {
		t.Fatalf("error = %v, want the successor's own reason", err)
	}
}

// A SUCCESSOR THAT DIES IS A REFUSAL, NOT A WAIT.
func TestASuccessorThatDiesBeforeAnsweringIsARefusal(t *testing.T) {
	successor, command := fakeSuccessorScript(t, `exit 3`)
	_ = command

	_, err := successor.Identify()
	if err == nil || !strings.Contains(err.Error(), "stopped answering") {
		t.Fatalf("error = %v, want one naming a successor that stopped answering", err)
	}
}

// AN ANSWER THIS PROTOCOL DOES NOT KNOW IS NOT AN ANSWER.
func TestAnUnrecognisedReplyIsNotAnIdentity(t *testing.T) {
	successor, _ := fakeSuccessorScript(t, `printf 'hello\n' >&4; sleep 30`)

	_, err := successor.Identify()
	if err == nil || !strings.Contains(err.Error(), "where \"identity\" was expected") {
		t.Fatalf("error = %v, want one naming the unexpected reply", err)
	}
}

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
	// The process is gone rather than merely unreferenced: signalling a live
	// process group succeeds, and this must not.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-command.Process.Pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the abandoned successor's process group is still alive")
}
