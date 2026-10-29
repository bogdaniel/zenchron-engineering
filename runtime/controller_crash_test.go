package runtime

// THE TWO-PROCESS CRASH SUITE.
//
// Everything below this point in the stack is proven in isolation. This proves
// the composed system: at every crash boundary, durable state stays
// interpretable, authority never duplicates, and recovery proceeds without
// deriving truth from a socket, a process's existence, a stale journal or a
// projection.
//
// IT IS ORGANISED BY RECOVERY CLASS rather than by cut point, because the cut
// points are instances of four situations:
//
//	A  the predecessor is still authoritative
//	B  nobody is serving, the predecessor is still authoritative
//	C  the successor is durably authoritative and not serving
//	D  the successor is serving and the predecessor is merely alive
//
// HARNESS FAILURES ARE NAMED AS SUCH. Three fixture defects in this stack have
// already produced failures that read as protocol defects, so every cut point
// asserts its own preconditions first - the child is alive, the pipe answers,
// the durable state is where the test believes it is - and reports
// HARNESS PRECONDITION as a distinct thing from a protocol failure.
//
// THE TWO LENSES ARE NOT REQUIRED TO AGREE IN CERTAINTY. The harness knows
// things an operator cannot: it killed the process itself. DescribeControllerStatus
// sees only what its evidence supports. The rule asserted here is the weaker and
// correct one - status must never assert anything the protocol evidence
// contradicts - rather than "status must reproduce what the test knows".

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// controllerChild is a real controller process the parent drives and kills.
//
// Deliberately dumb: start, send, read, kill, wait. No retries, no respawn, no
// interpretation of output as protocol truth beyond the ack the parent asked
// for, and no child-side lifetime logic.
type controllerChild struct {
	t      *testing.T
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	exited bool
}

func startControllerChild(t *testing.T, name, state, root string, generation ControllerBuild, artifact string) *controllerChild {
	t.Helper()
	encoded, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		controllerChildEnv+"=1",
		childGenerationEnv+"="+string(encoded),
		childStateEnv+"="+state,
		childRootEnv+"="+root,
		childArtifactEnv+"="+artifact,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &controllerChild{t: t, name: name, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(child.stop)
	return child
}

// step sends one command and returns the acknowledgement.
func (c *controllerChild) step(command string) string {
	c.t.Helper()
	c.requireAlive("before " + command)
	if _, err := io.WriteString(c.stdin, command+"\n"); err != nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s could not be sent %q: %v", c.name, command, err)
	}
	line, err := c.stdout.ReadString('\n')
	if err != nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s did not answer %q: %v", c.name, command, err)
	}
	return strings.TrimSpace(line)
}

// mustStep fails the test when the child reports a protocol error.
func (c *controllerChild) mustStep(command string) string {
	c.t.Helper()
	reply := c.step(command)
	if strings.HasPrefix(reply, "error") {
		c.t.Fatalf("%s refused %q: %s", c.name, command, reply)
	}
	return reply
}

// requireAlive is a HARNESS assertion. A child that died before the test meant
// to kill it invalidates the cut point, and saying so here is the difference
// between a fixture bug and a reported protocol failure.
func (c *controllerChild) requireAlive(when string) {
	c.t.Helper()
	if c.exited {
		c.t.Fatalf("HARNESS PRECONDITION: %s had already exited %s", c.name, when)
	}
	if c.cmd.Process == nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s never started, %s", c.name, when)
	}
	if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s is not alive %s: %v", c.name, when, err)
	}
}

// kill ends the child the way a crash does, and WAITS for it. A test that
// started a replacement before the corpse was reaped would be measuring a race
// it did not mean to run.
func (c *controllerChild) kill() {
	c.t.Helper()
	c.requireAlive("before being killed")
	if err := c.cmd.Process.Kill(); err != nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s could not be killed: %v", c.name, err)
	}
	_, _ = c.cmd.Process.Wait()
	c.exited = true
}

// stop ends the child politely, for cleanup.
func (c *controllerChild) stop() {
	if c.exited || c.cmd.Process == nil {
		return
	}
	_ = c.stdin.Close()
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	c.exited = true
}

func (c *controllerChild) snapshot(handoffID string) LiveControllerSnapshot {
	c.t.Helper()
	reply := c.step(stepSnapshot + " " + handoffID)
	payload, ok := strings.CutPrefix(reply, "snapshot ")
	if !ok {
		c.t.Fatalf("%s did not report a snapshot: %s", c.name, reply)
	}
	var snapshot LiveControllerSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		c.t.Fatalf("HARNESS PRECONDITION: %s reported an unreadable snapshot: %v", c.name, err)
	}
	return snapshot
}

// requireRefusal asserts a REFUSAL FOR THE RIGHT REASON. Asserting only that
// the reply began with "error" would pass on a harness fault - a child that
// never started, a pipe that broke - which is how a broken fixture comes to
// look like a protocol that works.
func requireRefusal(t *testing.T, reply, reason, what string) {
	t.Helper()
	if !strings.HasPrefix(reply, "error") {
		t.Fatalf("%s: %s", what, reply)
	}
	if !strings.Contains(reply, reason) {
		t.Fatalf("%s was refused for the wrong reason: got %q, want one naming %q", what, reply, reason)
	}
}

// crashFixture is a state directory with a transition ready for a successor,
// plus the two generations as real directories.
type crashFixture struct {
	t           *testing.T
	state       string
	root        string
	store       *SQLiteOperationStore
	record      ControllerHandoff
	predecessor ControllerBuild
	successor   ControllerBuild
}

func newCrashFixture(t *testing.T) *crashFixture {
	t.Helper()
	inner := newChoreography(t)
	released, err := BeginHandoff(inner.predecessorPorts(), inner.record)
	if err != nil {
		t.Fatal(err)
	}
	return &crashFixture{
		t: t, state: inner.state, root: inner.root, store: inner.store, record: released,
		predecessor: *released.Predecessor.Binding.Build,
		successor:   *released.Successor.Binding.Build,
	}
}

func (f *crashFixture) successorChild(name string) *controllerChild {
	f.t.Helper()
	return startControllerChild(f.t, name, f.state, f.root, f.successor, f.record.Successor.ArtifactPath)
}

func (f *crashFixture) predecessorChild(name string) *controllerChild {
	f.t.Helper()
	return startControllerChild(f.t, name, f.state, f.root, f.predecessor, f.record.Predecessor.ArtifactPath)
}

// storedPhase is protocol-level evidence, read straight from the store.
func (f *crashFixture) storedPhase() HandoffPhase {
	f.t.Helper()
	stored, found, err := f.store.ControllerHandoff(f.record.ID)
	if err != nil || !found {
		f.t.Fatalf("read the transition: %v found=%v", err, found)
	}
	return stored.Phase
}

// roleIsFree is protocol-level evidence the operator surface deliberately
// cannot produce: the harness takes the role to find out, which an observer
// must not do.
func (f *crashFixture) roleIsFree() bool {
	f.t.Helper()
	lease, err := AcquireControllerRole(f.state)
	if err != nil {
		return false
	}
	_ = lease.Release()
	return true
}

// status is the operator lens, with no live observation available.
func (f *crashFixture) status(observe func() (LiveControllerSnapshot, error)) ControllerStatus {
	f.t.Helper()
	status, err := DescribeControllerStatus(f.store, f.root, observe, time.Now().UTC())
	if err != nil {
		f.t.Fatal(err)
	}
	return status
}

// requireStatusAgrees is THE RULE between the two lenses: status may know less
// than the harness, and may never assert something the protocol evidence
// contradicts.
func (f *crashFixture) requireStatusAgrees(status ControllerStatus, activeGeneration *ControllerBuild) {
	f.t.Helper()
	switch {
	case activeGeneration == nil && status.Durable.Generation != nil:
		f.t.Fatalf("status claims %q is active while the record has no activation",
			status.Durable.Generation.Version)
	case activeGeneration != nil && status.Durable.Generation == nil:
		f.t.Fatalf("status reports no active generation while the record activated %q",
			activeGeneration.Version)
	case activeGeneration != nil && *status.Durable.Generation != *activeGeneration:
		f.t.Fatalf("status reports %q active, the record says %q",
			status.Durable.Generation.Version, activeGeneration.Version)
	}
	if status.DurableConsistency == DurableViolation {
		f.t.Fatalf("status reported an invariant violation: %v", status.Findings)
	}
}

// CLASS A / B: the successor dies after taking the role and before activating.
// Nothing was activated, nobody serves, and the predecessor is still the
// authority - which is to say, nothing was invented.
func TestSuccessorDyingBeforeActivationInventsNothing(t *testing.T) {
	fixture := newCrashFixture(t)
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)

	// Harness evidence: the role really is taken.
	if fixture.roleIsFree() {
		t.Fatal("HARNESS PRECONDITION: the successor did not take the controller role")
	}
	if phase := fixture.storedPhase(); phase != HandoffOwnershipReleased {
		t.Fatalf("phase = %q before activation, want ownership_released", phase)
	}

	successor.kill()

	if phase := fixture.storedPhase(); phase == HandoffActivated {
		t.Fatal("a successor that died before activating left an activation behind")
	}
	if !fixture.roleIsFree() {
		t.Fatal("the role was not released by the successor's death")
	}
	// Operator lens: nothing activated, nothing observable, no violation.
	status := fixture.status(nil)
	fixture.requireStatusAgrees(status, nil)
	if status.Serving != ServingUnknown {
		t.Fatalf("serving = %q, want unknown with no observable controller", status.Serving)
	}
}

// CLASS C: the successor dies immediately after the activation commits, with
// the projection not yet repaired. Durable truth survives the process, nobody
// serves, and a replacement of that generation resumes.
func TestActivatedSuccessorDyingBeforeServiceIsRecoverable(t *testing.T) {
	fixture := newCrashFixture(t)
	// A directory at the pointer makes the projection repair fail, which is
	// the honest way to reach "activated, pointer stale" without racing.
	if err := os.MkdirAll(filepath.Join(fixture.root, StableEntrypointName, "operator-files"), 0o700); err != nil {
		t.Fatal(err)
	}
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)
	reply := successor.step(stepActivate + " " + fixture.record.ID)
	if !strings.HasPrefix(reply, "activated-with-drift") {
		t.Fatalf("activate = %q, want an activation with a failed projection", reply)
	}

	// Protocol evidence before the crash.
	if phase := fixture.storedPhase(); phase != HandoffActivated {
		t.Fatalf("phase = %q, want activated", phase)
	}
	snapshot := successor.snapshot(fixture.record.ID)
	if snapshot.WorkAdmission == AdmissionOpen {
		t.Fatal("HARNESS PRECONDITION: the successor opened service before the cut point")
	}

	successor.kill()

	// Durable truth outlives the process.
	if phase := fixture.storedPhase(); phase != HandoffActivated {
		t.Fatalf("phase = %q after the crash, want activated", phase)
	}
	if !fixture.roleIsFree() {
		t.Fatal("the role was not released by the crash")
	}
	// Operator lens: consistent, unknown service, drifted pointer, no violation.
	status := fixture.status(nil)
	fixture.requireStatusAgrees(status, &fixture.successor)
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent", status.DurableConsistency)
	}
	if status.Projection.State == ProjectionCurrent {
		t.Fatal("the projection was reported current while it was never repaired")
	}

	// A replacement of the SAME generation resumes without a new transition.
	if err := os.RemoveAll(filepath.Join(fixture.root, StableEntrypointName)); err != nil {
		t.Fatal(err)
	}
	replacement := fixture.successorChild("replacement")
	replacement.mustStep(stepStart)
	replacement.mustStep(stepRecover + " " + fixture.record.ID)
	replacement.mustStep(stepEnable + " " + fixture.record.ID)
	if resumed := replacement.snapshot(fixture.record.ID); resumed.WorkAdmission != AdmissionOpen {
		t.Fatalf("the replacement is not serving: %+v", resumed)
	}
}

// THE PREDECESSOR NEVER RESUMES SERVICE once it has drained and released, even
// when the successor fails. Availability may lose; authority does not improvise.
func TestPredecessorDoesNotResumeWhenTheSuccessorDies(t *testing.T) {
	fixture := newCrashFixture(t)
	predecessor := fixture.predecessorChild("predecessor")
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)
	successor.mustStep(stepActivate + " " + fixture.record.ID)
	successor.kill()

	// The predecessor is alive and the role is free. It may take the role -
	// nothing stops a process from doing that - and it must not serve.
	predecessor.mustStep(stepStart)
	requireRefusal(t, predecessor.step(stepEnable+" "+fixture.record.ID),
		"not the activated generation", "the predecessor opened service after the successor activated")
	snapshot := predecessor.snapshot(fixture.record.ID)
	if snapshot.WorkAdmission == AdmissionOpen {
		t.Fatal("the predecessor is admitting work while another generation is durably active")
	}
	// And the operator lens says the same thing: a live predecessor holding the
	// role is a violation once another generation is active.
	status := fixture.status(func() (LiveControllerSnapshot, error) { return snapshot, nil })
	if status.DurableConsistency != DurableViolation {
		t.Fatalf("durable consistency = %q, want invariant_violation: a stale generation holds the role",
			status.DurableConsistency)
	}
}

// THE STALE PROJECTION TEST. The pointer still names the predecessor after the
// successor activated, the successor dies, and the stable path launches the OLD
// generation. It may start and it may take the free role. It must not serve.
func TestStaleProjectionCannotRestoreAnOldGeneration(t *testing.T) {
	fixture := newCrashFixture(t)
	// The pointer names the predecessor, as it does before any repair.
	pointer := filepath.Join(fixture.root, StableEntrypointName)
	if err := os.Symlink(filepath.Dir(fixture.record.Predecessor.ArtifactPath), pointer); err != nil {
		t.Fatal(err)
	}
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)
	// Activation repairs the pointer, so put it back: this test is about what
	// happens when the repair never ran.
	successor.mustStep(stepActivate + " " + fixture.record.ID)
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(fixture.record.Predecessor.ArtifactPath), pointer); err != nil {
		t.Fatal(err)
	}
	successor.kill()

	target, err := os.Readlink(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Dir(fixture.record.Predecessor.ArtifactPath) {
		t.Fatalf("HARNESS PRECONDITION: the pointer names %s, not the predecessor", target)
	}

	// What the stable entrypoint launches: the OLD generation.
	launched := fixture.predecessorChild("launched-by-stale-pointer")
	launched.mustStep(stepStart)
	requireRefusal(t, launched.step(stepRecover+" "+fixture.record.ID),
		"not the activated generation", "an old generation recovered an activation that names another")
	requireRefusal(t, launched.step(stepEnable+" "+fixture.record.ID),
		"not the activated generation", "an old generation launched by a stale pointer began serving")
	if snapshot := launched.snapshot(fixture.record.ID); snapshot.WorkAdmission == AdmissionOpen {
		t.Fatal("the stale generation is admitting work")
	}
}

// TWO REPLACEMENTS OF ONE GENERATION race recovery. Both prove the same
// identity - they are the same generation - and only the one that takes the role
// can recover or serve.
func TestTwoReplacementsOfOneGenerationDoNotBothServe(t *testing.T) {
	fixture := newCrashFixture(t)
	original := fixture.successorChild("original")
	original.mustStep(stepStart)
	original.mustStep(stepActivate + " " + fixture.record.ID)
	original.kill()

	first := fixture.successorChild("replacement-one")
	second := fixture.successorChild("replacement-two")
	firstStart := first.step(stepStart)
	secondStart := second.step(stepStart)
	started := 0
	for _, reply := range []string{firstStart, secondStart} {
		if !strings.HasPrefix(reply, "error") {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("%d replacements took the controller role, want exactly 1 (%q / %q)",
			started, firstStart, secondStart)
	}
	holder, other := first, second
	loserStart := secondStart
	if strings.HasPrefix(firstStart, "error") {
		holder, other, loserStart = second, first, firstStart
	}
	// THE LOSER LOST FOR THE PROTOCOL'S REASON. It was refused the role, not
	// tripped by the fixture - which is the difference between proving
	// exclusion and proving that a child failed to start.
	requireRefusal(t, loserStart, "controller role",
		"the second replacement was not refused the role")
	holder.mustStep(stepRecover + " " + fixture.record.ID)
	holder.mustStep(stepEnable + " " + fixture.record.ID)
	if snapshot := holder.snapshot(fixture.record.ID); snapshot.WorkAdmission != AdmissionOpen {
		t.Fatal("the replacement holding the role is not serving")
	}
	// The other is the same generation and has nothing.
	// And it cannot serve. Having been refused the role it holds no service to
	// open, which is itself the point: service is unreachable without the
	// capability, not merely refused by a later check.
	if reply := other.step(stepEnable + " " + fixture.record.ID); !strings.HasPrefix(reply, "error") {
		t.Fatalf("a second process of the active generation began serving: %s", reply)
	}
	if snapshot := holder.snapshot(fixture.record.ID); snapshot.Role != RoleHeld {
		t.Fatal("the serving replacement does not hold the role")
	}
}

// A WRONG-GENERATION PROCESS may take a free role and still cannot recover an
// activation that names somebody else.
func TestWrongGenerationCannotRecoverAnActivation(t *testing.T) {
	fixture := newCrashFixture(t)
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)
	successor.mustStep(stepActivate + " " + fixture.record.ID)
	successor.kill()

	stranger := fixture.predecessorChild("wrong-generation")
	stranger.mustStep(stepStart) // the role is free, so this succeeds
	requireRefusal(t, stranger.step(stepRecover+" "+fixture.record.ID),
		"not the activated generation", "a wrong-generation process recovered the activation")
	requireRefusal(t, stranger.step(stepEnable+" "+fixture.record.ID),
		"not the activated generation", "a wrong-generation process opened service")
}

// CLASS D: the successor is serving and the predecessor is merely alive. That
// is not a violation, and it becomes one only if the predecessor owns something.
func TestServingSuccessorBesideALivePredecessorIsNotAViolation(t *testing.T) {
	fixture := newCrashFixture(t)
	predecessor := fixture.predecessorChild("predecessor")
	successor := fixture.successorChild("successor")
	successor.mustStep(stepStart)
	successor.mustStep(stepActivate + " " + fixture.record.ID)
	successor.mustStep(stepEnable + " " + fixture.record.ID)

	// The predecessor is alive, holds nothing, admits nothing.
	predecessor.requireAlive("while the successor serves")
	serving := successor.snapshot(fixture.record.ID)
	if serving.WorkAdmission != AdmissionOpen || serving.Role != RoleHeld {
		t.Fatalf("HARNESS PRECONDITION: the successor is not serving: %+v", serving)
	}
	status := fixture.status(func() (LiveControllerSnapshot, error) { return serving, nil })
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent: overlapping liveness is not overlapping authority",
			status.DurableConsistency)
	}
	if status.Serving != Serving {
		t.Fatalf("serving = %q, want serving", status.Serving)
	}
}

// THE DEATH BOUNDARY, raced deliberately: a replacement retries a nonblocking
// acquire while the holder is killed. Never two holders, and the replacement
// eventually wins.
func TestRoleTransfersExactlyOnceAcrossAHardKill(t *testing.T) {
	fixture := newCrashFixture(t)
	holder := fixture.successorChild("holder")
	holder.mustStep(stepStart)

	acquired := make(chan *ControllerRoleLease, 8)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				close(acquired)
				return
			default:
			}
			if lease, err := AcquireControllerRole(fixture.state); err == nil {
				acquired <- lease
				close(acquired)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	holder.kill()
	deadline := time.After(5 * time.Second)
	select {
	case lease := <-acquired:
		if lease == nil {
			t.Fatal("the racing acquirer stopped without taking the role")
		}
		_ = lease.Release()
	case <-deadline:
		close(stop)
		t.Fatal("the role never transferred after the holder was killed")
	}
}
