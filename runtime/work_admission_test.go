package runtime

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE LINEARIZATION PROPERTY, driven concurrently rather than in a simulated
// order: after close returns, no admission that had not already committed can
// become visible.
//
// The previous shape - read a flag, release the lock, then create the run -
// passes any serial test and loses this one.
func TestNoAdmissionBecomesVisibleAfterTheDrainReturns(t *testing.T) {
	gate := newWorkAdmissionGate(true)
	var committed int64
	var started sync.WaitGroup
	var finished sync.WaitGroup
	release := make(chan struct{})

	// A batch of admissions, each of which takes real time between being
	// permitted and committing - which is the window the defect lived in.
	const admissions = 32
	for i := 0; i < admissions; i++ {
		started.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			once := sync.OnceFunc(started.Done)
			_ = gate.admit(func() error {
				once()
				<-release
				atomic.AddInt64(&committed, 1)
				return nil
			})
			once() // a refused admission still reports that it ran
		}()
	}
	started.Wait()

	// Close while every in-flight admission is parked mid-commit.
	closed := make(chan struct{})
	go func() {
		gate.close("drained")
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("the drain returned while admissions were still committing")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-closed

	// Whatever committed, committed BEFORE the drain returned. Sample now and
	// prove the number cannot move afterwards.
	atDrain := atomic.LoadInt64(&committed)
	finished.Wait()
	if after := atomic.LoadInt64(&committed); after != atDrain {
		t.Fatalf("%d admission(s) became visible after the drain returned", after-atDrain)
	}
	if atDrain == 0 {
		t.Fatal("the test did not exercise in-flight admissions")
	}

	// And nothing new is admitted afterwards, in any interleaving.
	var late int64
	var lateGroup sync.WaitGroup
	for i := 0; i < 8; i++ {
		lateGroup.Add(1)
		go func() {
			defer lateGroup.Done()
			_ = gate.admit(func() error { atomic.AddInt64(&late, 1); return nil })
		}()
	}
	lateGroup.Wait()
	if late != 0 {
		t.Fatalf("%d admission(s) were accepted after the drain", late)
	}
}

// A section behaves the same way for intake that creates several runs.
func TestIntakeSectionHoldsTheGateOpenForItsWholeDuration(t *testing.T) {
	gate := newWorkAdmissionGate(true)
	release, err := gate.section()
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { gate.close("drained"); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the drain returned while an intake section was open")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-closed
	if _, err := gate.section(); err == nil {
		t.Fatal("an intake section opened after the drain")
	}
}

// Closing is terminal, and a withheld gate is not an open one.
func TestWorkAdmissionLifecycle(t *testing.T) {
	withheld := newWorkAdmissionGate(false)
	if withheld.permitted() {
		t.Fatal("a withheld gate admitted work")
	}
	var refused *WorkAdmissionRefusedError
	if err := withheld.admit(func() error { return nil }); err == nil {
		t.Fatal("a withheld gate ran a commit")
	} else if !asRefusal(err, &refused) {
		t.Fatalf("error = %v, want a work-admission refusal", err)
	}
	if err := withheld.open(); err != nil {
		t.Fatal(err)
	}
	if !withheld.permitted() {
		t.Fatal("an opened gate does not admit work")
	}
	withheld.close("drained")
	if err := withheld.open(); err == nil {
		t.Fatal("a closed gate reopened")
	}
	if !withheld.closed() {
		t.Fatal("a closed gate does not report closed")
	}
}

func asRefusal(err error, target **WorkAdmissionRefusedError) bool {
	refusal, ok := err.(*WorkAdmissionRefusedError)
	if ok {
		*target = refusal
	}
	return ok
}

// HISTORICAL SUCCESSION VALIDITY IS NOT CURRENT SERVICE AUTHORITY. A transition
// that activated stays activated forever; the generation it activated can still
// be forbidden to serve because a later transition replaced it.
func TestActivatedTransitionDoesNotConferPresentAuthority(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	g1 := adoptedBinding(predecessorRevision, "tree-a", config)
	g2 := adoptedBinding(successorRevision, "tree-b", config)
	g3 := adoptedBinding(strangerRevision, "tree-c", config)
	build2 := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("ab", 32))
	self2 := ControllerSelfRecord{Build: build2, Measured: build2.BinarySHA256}
	digest := func(binding ControllerBinding) string {
		value, err := binding.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	now := time.Unix(1700000000, 0).UTC()

	first := ControllerHandoff{
		ID: "handoff-first", Phase: HandoffActivated,
		Predecessor: HandoffParty{Binding: g1}, Successor: HandoffParty{Binding: g2},
		RecoveryOwner: digest(g2), UpdatedAt: now,
	}
	second := ControllerHandoff{
		ID: "handoff-second", Phase: HandoffActivated,
		Predecessor: HandoffParty{Binding: g2}, Successor: HandoffParty{Binding: g3},
		RecoveryOwner: digest(g3), UpdatedAt: now,
	}

	// The historical oracle still says the first transition activated, and it
	// always will: a run inherited through it was legitimately inherited.
	activated := map[string]bool{first.ID: true, second.ID: true}
	oracle := transitionActivated(func(id string) bool { return activated[id] })
	run := runFor(t, g1)
	events := []EngineeringEvent{func() EngineeringEvent {
		event := admission(t, g1, g2)
		var decision ControllerSuccessionDecision
		if err := decodeJSON(event.Payload, &decision); err != nil {
			t.Fatal(err)
		}
		decision.HandoffID = first.ID
		payload, err := marshalPayloadJSON(decision)
		if err != nil {
			t.Fatal(err)
		}
		event.Payload = payload
		return event
	}()}
	if !ControllerSuccessionContinues(run, events, digest(g2), oracle) {
		t.Fatal("a run inherited through an activated transition stopped being inherited")
	}

	// And the present question answers no, because a later transition replaced
	// that generation.
	if admission := WorkAdmissionFor(&second, self2, digest(g2)); admission.Permitted {
		t.Fatal("a superseded generation may still admit work")
	}
}
