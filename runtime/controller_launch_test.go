package runtime

// THE ORDER AND THE POINT OF NO RETURN, proven step by step.
//
// Each case places one failure at one step and asserts the two things an
// operator's availability depends on: how far the transition got, and whether
// this controller is still allowed to serve.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeSuccessor is a spawned process that never existed. Its point is to fail
// at exactly one step: a real child process cannot be made to refuse Identify
// and nothing else.
type fakeSuccessor struct {
	announce    ControllerBinding
	identifyErr error
	proceedErr  error
	activeErr   error
	abandonErr  error

	identified, proceeded, awaited, abandoned int
}

func (f *fakeSuccessor) Identify() (ControllerBinding, error) {
	f.identified++
	return f.announce, f.identifyErr
}

func (f *fakeSuccessor) Proceed(string) error { f.proceeded++; return f.proceedErr }
func (f *fakeSuccessor) AwaitActive() error   { f.awaited++; return f.activeErr }
func (f *fakeSuccessor) Abandon() error       { f.abandoned++; return f.abandonErr }

type launchHarness struct {
	successor  *fakeSuccessor
	spawnErr   error
	trusted    RevisionRecord
	observeErr error
	beginErr   error

	spawned, began int
}

func (h *launchHarness) ports() SuccessionPorts {
	return SuccessionPorts{
		Spawn: func(string, string) (InertSuccessor, error) {
			h.spawned++
			if h.spawnErr != nil {
				return nil, h.spawnErr
			}
			return h.successor, nil
		},
		ObserveTrustedMain: func(context.Context) (RevisionRecord, error) {
			return h.trusted, h.observeErr
		},
		Begin: func(prepared ControllerHandoff) (ControllerHandoff, error) {
			h.began++
			if h.beginErr != nil {
				return prepared, h.beginErr
			}
			advanced, err := prepared.Advance(HandoffDraining, time.Unix(1700000100, 0).UTC())
			if err != nil {
				return prepared, err
			}
			return advanced.Advance(HandoffOwnershipReleased, time.Unix(1700000101, 0).UTC())
		},
		Now: func() time.Time { return time.Unix(1700000200, 0).UTC() },
	}
}

// preparedTransition is a compatible, prepared record between two adopted
// builds of the same controller under the same configuration.
func preparedTransition(t *testing.T) (ControllerHandoff, ControllerBinding, RevisionRecord) {
	t.Helper()
	predecessorBuild := attestedBuild(ControllerAdopted, strings.Repeat("a", 40), "tree-a", strings.Repeat("ab", 32))
	successorBuild := attestedBuild(ControllerAdopted, strings.Repeat("b", 40), "tree-b", strings.Repeat("cd", 32))
	config := ConfigDigest{Global: "global", Repository: "repository"}
	successor := ControllerBinding{Controller: "zenchron-engineering", Build: &successorBuild, Config: config}
	record := ControllerHandoff{
		ID:    "handoff-launch",
		Phase: HandoffPrepared,
		Predecessor: HandoffParty{
			Binding:      ControllerBinding{Controller: "zenchron-engineering", Build: &predecessorBuild, Config: config},
			ArtifactPath: "/controller/main-a/zenchron-engineering",
		},
		Successor: HandoffParty{Binding: successor, ArtifactPath: "/controller/main-b/zenchron-engineering"},
	}
	return record, successor, RevisionRecord{Revision: successorBuild.SourceRevision, Tree: successorBuild.SourceTree}
}

func launchFixture(t *testing.T) (*launchHarness, ControllerHandoff, RevisionRecord) {
	t.Helper()
	record, successor, subject := preparedTransition(t)
	return &launchHarness{successor: &fakeSuccessor{announce: successor}, trusted: subject}, record, subject
}

// THE WHOLE SEQUENCE, in order, ending with the successor serving.
func TestALaunchHandsTheRoleOverAndTheSuccessorServes(t *testing.T) {
	harness, record, subject := launchFixture(t)
	launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

	if !launch.Served || !launch.Committed {
		t.Fatalf("the successor is not serving: %+v", launch)
	}
	for name, step := range map[string]LaunchStep{
		"spawn": launch.Spawn, "identify": launch.Identify, "recheck": launch.Recheck,
		"begin": launch.Begin, "signal": launch.Signal, "activate": launch.Activate,
	} {
		if step.Outcome != StepSucceeded {
			t.Fatalf("%s = %q (%s)", name, step.Outcome, step.Detail)
		}
	}
	if harness.successor.abandoned != 0 {
		t.Fatal("a successor that served was abandoned")
	}
}

// EVERY REFUSAL BEFORE THE POINT OF NO RETURN leaves this controller serving
// and the successor stopped.
func TestARefusalBeforeThePointOfNoReturnKeepsThePredecessorServing(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*launchHarness, *ControllerHandoff)
		want   string
	}{
		{"the record is not a prepared transition", func(_ *launchHarness, record *ControllerHandoff) {
			record.Phase = HandoffActivated
		}, "a launch begins from"},
		{"the transition is blocked", func(_ *launchHarness, record *ControllerHandoff) {
			record.Runs = []HandoffRunDecision{{RunID: "run-x", Decision: ControllerSuccessionDecision{
				RunID: "run-x", Result: SuccessionRefused,
				DurableReplay: SuccessionCheck{Detail: "the journal could not be replayed"}}}}
		}, "is blocked"},
		{"the successor cannot be started", func(h *launchHarness, _ *ControllerHandoff) {
			h.spawnErr = fmt.Errorf("permission denied")
		}, "could not be started"},
		{"the successor will not say what it is", func(h *launchHarness, _ *ControllerHandoff) {
			h.successor.identifyErr = fmt.Errorf("the pipe closed")
		}, "did not report what it is"},
		{"the successor is not the prepared generation", func(h *launchHarness, _ *ControllerHandoff) {
			other := attestedBuild(ControllerAdopted, strings.Repeat("c", 40), "tree-c", strings.Repeat("ef", 32))
			h.successor.announce.Build = &other
		}, "was prepared for"},
		{"trusted main cannot be re-observed", func(h *launchHarness, _ *ControllerHandoff) {
			h.observeErr = fmt.Errorf("the forge did not answer")
		}, "could not be re-observed"},
		{"trusted main has moved", func(h *launchHarness, _ *ControllerHandoff) {
			h.trusted = RevisionRecord{Revision: strings.Repeat("d", 40), Tree: "tree-d"}
		}, "trusted main is"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness, record, subject := launchFixture(t)
			test.break_(harness, &record)

			launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

			if launch.Committed {
				t.Fatalf("a refusal before the drain committed the transition: %+v", launch)
			}
			if harness.began != 0 {
				t.Fatal("the predecessor drained for a transition it had already refused")
			}
			if !strings.Contains(launch.Summary(), test.want) {
				t.Fatalf("summary = %q, want one naming %q", launch.Summary(), test.want)
			}
			// A successor that was started and will not be used is stopped.
			if harness.spawnErr == nil && harness.spawned == 1 && harness.successor.abandoned != 1 {
				t.Fatalf("a spawned successor was abandoned %d times", harness.successor.abandoned)
			}
			if harness.successor.proceeded != 0 {
				t.Fatal("a successor that was refused was told to proceed")
			}
		})
	}
}

// AFTER THE POINT OF NO RETURN, the predecessor is committed whatever the
// successor does - and it says so.
func TestAfterTheDrainThePredecessorIsCommittedEvenWhenTheSuccessorFails(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*launchHarness)
		want   string
	}{
		{"the predecessor's own half fails", func(h *launchHarness) {
			h.beginErr = fmt.Errorf("the transition could not be recorded")
		}, "did not complete"},
		{"the successor cannot be signalled", func(h *launchHarness) {
			h.successor.proceedErr = fmt.Errorf("the successor is gone")
		}, "could not be told to proceed"},
		{"the successor never activates", func(h *launchHarness) {
			h.successor.activeErr = fmt.Errorf("the successor could not acquire the controller role")
		}, "did not become the serving controller"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness, record, subject := launchFixture(t)
			test.break_(harness)

			launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

			if !launch.Committed {
				t.Fatalf("a failure after the drain left the controller believing it may serve: %+v", launch)
			}
			if launch.Served {
				t.Fatal("a failed transition reported the successor as serving")
			}
			if !strings.Contains(launch.Summary(), test.want) {
				t.Fatalf("summary = %q, want one naming %q", launch.Summary(), test.want)
			}
			// NOTHING IS TAKEN BACK. The predecessor does not abandon a
			// successor it can no longer outrank, and does not reacquire.
			if harness.successor.abandoned != 0 {
				t.Fatal("the predecessor killed a successor after giving up the role")
			}
		})
	}
}
