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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
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
	// The inherited arguments deliberately already name a transition: that is
	// what a predecessor's own command line looks like from the second
	// generation on.
	successor, err := spawnInertSuccessor("/bin/sh", "handoff-1",
		[]string{"-c", body, "successor", successorFlag, "handoff-the-one-before"})
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

// THE EVALUATION EXCHANGE, across the same process boundary.
func TestTheSuccessorDecidesTheTransitionOverTheHandshake(t *testing.T) {
	binding := announcedBinding(t)
	successor, _ := fakeSuccessorScript(t, `
		printf 'identity %s\n' '`+binding+`' >&4
		read -r line <&3
		case "$line" in
			evaluate\ *) printf 'decided {"id":"handoff-1","phase":"prepared"}\n' >&4 ;;
			*) printf 'error unexpected %s\n' "$line" >&4 ;;
		esac`)
	if _, err := successor.Identify(); err != nil {
		t.Fatalf("HARNESS PRECONDITION: %v", err)
	}
	asking, ok := successor.(*spawnedSuccessor)
	if !ok {
		t.Fatalf("HARNESS PRECONDITION: %T", successor)
	}

	decided, err := asking.Evaluate(runtime.ControllerHandoff{ID: "handoff-1", Phase: runtime.HandoffPrepared})
	if err != nil {
		t.Fatalf("the successor did not decide the transition: %v", err)
	}
	if decided.ID != "handoff-1" || decided.Phase != runtime.HandoffPrepared {
		t.Fatalf("the decision did not survive the pipe: %+v", decided)
	}
}

// A SUCCESSOR THAT REFUSES TO DECIDE SAYS WHY, and the predecessor keeps that
// reason rather than a timeout.
func TestASuccessorThatCannotDecideSaysWhy(t *testing.T) {
	binding := announcedBinding(t)
	successor, _ := fakeSuccessorScript(t, `
		printf 'identity %s\n' '`+binding+`' >&4
		read -r line <&3
		printf 'error event type "plan.stage.superseded" is not in this controller vocabulary\n' >&4`)
	if _, err := successor.Identify(); err != nil {
		t.Fatalf("HARNESS PRECONDITION: %v", err)
	}
	asking := successor.(*spawnedSuccessor)

	_, err := asking.Evaluate(runtime.ControllerHandoff{ID: "handoff-1", Phase: runtime.HandoffPrepared})
	if err == nil || !strings.Contains(err.Error(), "is not in this controller vocabulary") {
		t.Fatalf("err = %v, want the successor's own reason", err)
	}
}

// ---------------------------------------------------------------------------
// The successor's side of the same loop
// ---------------------------------------------------------------------------

// handshakePair wires a successor handshake to a predecessor's two pipes,
// without a process boundary: this exercises the successor's command loop,
// which lives in this binary rather than in the child.
func handshakePair(t *testing.T, handoffID string) (*successorHandshake, *os.File, *bufio.Reader) {
	t.Helper()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		controlWrite.Close()
		reportRead.Close()
	})
	return &successorHandshake{
		handoffID: handoffID, control: bufio.NewReader(controlRead), report: reportWrite,
	}, controlWrite, bufio.NewReader(reportRead)
}

// THE SUCCESSOR ANSWERS EVALUATIONS AND THEN PROCEEDS, in that order and any
// number of times.
func TestTheSuccessorAnswersEvaluationsUntilItIsToldToProceed(t *testing.T) {
	handshake, control, report := handshakePair(t, "handoff-1")
	decisions := 0
	done := make(chan error, 1)
	go func() {
		done <- handshake.serveUntilProceed(func(asked runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
			decisions++
			asked.Runs = []runtime.HandoffRunDecision{{RunID: "run-1", Result: "compatible"}}
			return asked, nil
		})
	}()

	if _, err := io.WriteString(control, `evaluate {"id":"handoff-1","phase":"prepared"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := report.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, successorDecidedLine+" ") || !strings.Contains(line, "run-1") {
		t.Fatalf("reply = %q, want the decision", strings.TrimSpace(line))
	}
	if _, err := io.WriteString(control, "proceed handoff-1\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the successor did not proceed: %v", err)
	}
	if decisions != 1 {
		t.Fatalf("%d decisions, want 1", decisions)
	}
}

// A RELEASE OF SOMEBODY ELSE'S TRANSITION IS NOT A RELEASE OF THIS ONE.
func TestTheSuccessorRefusesToProceedOnAnotherTransition(t *testing.T) {
	handshake, control, _ := handshakePair(t, "handoff-1")
	done := make(chan error, 1)
	go func() {
		done <- handshake.serveUntilProceed(func(runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
			return runtime.ControllerHandoff{}, nil
		})
	}()
	if _, err := io.WriteString(control, "proceed handoff-somebody-else\n"); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "handoff-somebody-else") {
		t.Fatalf("err = %v, want a refusal naming the transition that was released", err)
	}
}

// A DECISION THAT CANNOT BE MADE IS REPORTED AND THE SUCCESSOR KEEPS WAITING.
// It is the predecessor's transition to abandon, not the successor's.
func TestASuccessorThatCannotDecideKeepsWaiting(t *testing.T) {
	handshake, control, report := handshakePair(t, "handoff-1")
	done := make(chan error, 1)
	go func() {
		done <- handshake.serveUntilProceed(func(runtime.ControllerHandoff) (runtime.ControllerHandoff, error) {
			return runtime.ControllerHandoff{}, fmt.Errorf("the journal could not be replayed")
		})
	}()
	if _, err := io.WriteString(control, `evaluate {"id":"handoff-1"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := report.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "the journal could not be replayed") {
		t.Fatalf("reply = %q, want the reason", strings.TrimSpace(line))
	}
	select {
	case err := <-done:
		t.Fatalf("the successor stopped waiting after a refusal: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// A SUCCESSOR IS STARTED FOR EXACTLY ONE TRANSITION, whatever the predecessor
// was started for.
//
// Found live, on the third generation: the command line was
//
//	serve --successor-of handoff-A-B --successor-of handoff-B-C
//
// because the predecessor passes its own arguments through, and its own
// arguments already name the transition that created it. One stale transition
// accumulates per upgrade, forever, and the chain survives only because the
// parser happens to take the last value.
func TestASuccessorIsStartedForExactlyOneTransition(t *testing.T) {
	inherited := []string{
		"serve", "--repo", "acme/repo",
		successorFlag, "handoff-two-upgrades-ago",
		"--config", "/etc/zenchron.json",
		successorFlag, "handoff-one-upgrade-ago",
	}
	kept := withoutSuccessorFlag(inherited)

	for _, argument := range kept {
		if argument == successorFlag || strings.HasPrefix(argument, "handoff-") {
			t.Fatalf("a stale transition survived: %v", kept)
		}
	}
	if strings.Join(kept, " ") != "serve --repo acme/repo --config /etc/zenchron.json" {
		t.Fatalf("the operator's own arguments did not survive intact: %v", kept)
	}
}

// AND THE PROCESS THAT IS ACTUALLY STARTED SEES ONE, which is the thing the
// live defect was about: a unit test of the helper cannot see what exec was
// handed.
func TestTheSpawnedSuccessorReceivesOneTransition(t *testing.T) {
	successor, command := fakeSuccessorScript(t, `printf 'identity %s\n' "$*" >&4`)
	_ = command
	spawned, ok := successor.(*spawnedSuccessor)
	if !ok {
		t.Fatalf("HARNESS PRECONDITION: %T", successor)
	}
	// fakeSuccessorScript starts /bin/sh -c <script> successor, so the flags
	// under test are everything after the script's own name.
	line, err := spawned.await(successorIdentityLine, 10*time.Second)
	if err != nil {
		t.Fatalf("the successor did not report its arguments: %v", err)
	}
	if occurrences := strings.Count(line, successorFlag); occurrences != 1 {
		t.Fatalf("the successor was started with %d transitions: %q", occurrences, line)
	}
	if !strings.Contains(line, successorFlag+" handoff-1") {
		t.Fatalf("the successor was not started for its own transition: %q", line)
	}
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
