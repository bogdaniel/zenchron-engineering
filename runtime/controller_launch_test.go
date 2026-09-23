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
	quiesceErr error
	// decide is the successor's answer. nil means "the record it was asked
	// about, unchanged and compatible".
	decide func(ControllerHandoff) (ControllerHandoff, error)

	spawned, began, quiesced, resumed int
	begunWith                         ControllerHandoff
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
		Quiesce: func(context.Context) (func(), error) {
			if h.quiesceErr != nil {
				return nil, h.quiesceErr
			}
			h.quiesced++
			return func() { h.resumed++ }, nil
		},
		Evaluate: func(_ context.Context, prepared ControllerHandoff) (ControllerHandoff, error) {
			if h.decide != nil {
				return h.decide(prepared)
			}
			return prepared, nil
		},
		Begin: func(prepared ControllerHandoff) (ControllerHandoff, error) {
			h.began++
			h.begunWith = prepared
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
		{"the work this controller started will not stop", func(h *launchHarness, _ *ControllerHandoff) {
			h.quiesceErr = fmt.Errorf("work this controller started was still running after 30m0s")
		}, "could not be brought to a stop"},
		{"the successor cannot decide", func(h *launchHarness, _ *ControllerHandoff) {
			h.decide = func(ControllerHandoff) (ControllerHandoff, error) {
				return ControllerHandoff{}, fmt.Errorf("the journal could not be read")
			}
		}, "could not decide the transition"},
		{"the successor cannot continue a live run", func(h *launchHarness, _ *ControllerHandoff) {
			h.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
				asked.Runs = []HandoffRunDecision{{RunID: "run-x", Result: SuccessionRefused,
					Refusals: []string{`state_vocabulary: event type "plan.stage.superseded" is not in this controller's vocabulary`},
					Decision: ControllerSuccessionDecision{RunID: "run-x", Result: SuccessionRefused}}}
				return asked, nil
			}
		}, "cannot continue every live run"},
		{"the successor decided another transition", func(h *launchHarness, _ *ControllerHandoff) {
			h.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
				asked.ID = "handoff-somebody-else"
				return asked, nil
			}
		}, "decided a different transition"},
		{"the successor renamed itself", func(h *launchHarness, _ *ControllerHandoff) {
			h.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
				other := attestedBuild(ControllerAdopted, strings.Repeat("9", 40), "tree-9", strings.Repeat("99", 32))
				asked.Successor.Binding.Build = &other
				return asked, nil
			}
		}, "names a different successor"},
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
			// AND INTAKE COMES BACK. A refused transition costs an update; a
			// controller left unable to admit work would have cost more.
			if harness.quiesced != harness.resumed {
				t.Fatalf("intake was suspended %d times and resumed %d", harness.quiesced, harness.resumed)
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

// THE TRANSITION IS BEGUN WITH THE SUCCESSOR'S RECORD, not the predecessor's
// screen of the same question.
//
// The admissions written during the handover come from record.Runs, and those
// decisions are the successor's: it is the code that will read those journals.
// Beginning from the predecessor's copy would write the predecessor's opinion
// of whether the successor can read them.
func TestTheTransitionIsBegunWithTheSuccessorsOwnDecisions(t *testing.T) {
	harness, record, subject := launchFixture(t)
	harness.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
		asked.Runs = []HandoffRunDecision{{RunID: "run-live", Result: SuccessionCompatible,
			EventCount: 7, Decision: ControllerSuccessionDecision{
				RunID: "run-live", Result: SuccessionCompatible,
				DurableReplay: SuccessionCheck{Passed: true, Detail: "the successor replayed 7 events"},
			}}}
		return asked, nil
	}
	launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

	if !launch.Served {
		t.Fatalf("the transition did not complete: %+v", launch)
	}
	if len(harness.begunWith.Runs) != 1 || harness.begunWith.Runs[0].RunID != "run-live" {
		t.Fatalf("the transition was begun with %d decision(s) and not the successor's", len(harness.begunWith.Runs))
	}
	if detail := harness.begunWith.Runs[0].Decision.DurableReplay.Detail; detail != "the successor replayed 7 events" {
		t.Fatalf("the admitted decision is not the one the successor made: %q", detail)
	}
}

// QUIESCENCE HAPPENS BEFORE THE SUCCESSOR DECIDES, and both happen before
// anything is given up. The order is the guarantee: a decision made while the
// predecessor's own work could still append would be a statement about a state
// that no longer exists by the time it matters.
func TestTheSuccessorDecidesOnlyAfterTheStateStopsMoving(t *testing.T) {
	harness, record, subject := launchFixture(t)
	var order []string
	harness.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
		order = append(order, "evaluate")
		return asked, nil
	}
	ports := harness.ports()
	quiesce, observe, begin := ports.Quiesce, ports.ObserveTrustedMain, ports.Begin
	ports.Quiesce = func(ctx context.Context) (func(), error) {
		order = append(order, "quiesce")
		return quiesce(ctx)
	}
	ports.ObserveTrustedMain = func(ctx context.Context) (RevisionRecord, error) {
		order = append(order, "recheck")
		return observe(ctx)
	}
	ports.Begin = func(prepared ControllerHandoff) (ControllerHandoff, error) {
		order = append(order, "begin")
		return begin(prepared)
	}

	if launch := LaunchSuccession(context.Background(), record, subject, ports); !launch.Served {
		t.Fatalf("the transition did not complete: %+v", launch)
	}
	// CURRENCY IS PROVEN LAST. Quiescence and the successor's replay both take
	// time a branch can move in, so a recheck before them would authorize a
	// handover happening now with a fact about some time ago.
	if strings.Join(order, ",") != "quiesce,evaluate,recheck,begin" {
		t.Fatalf("order = %v, want the state to stop moving, the successor to decide, currency proven, then the handover", order)
	}
}

// TRUSTED MAIN MOVING DURING THE EVALUATION IS CAUGHT.
//
// This is the window the final recheck exists for: the successor's decision
// takes as long as replaying every live journal, and the branch it was built
// from can advance while it is thinking. A currency proof taken before that
// would be a fact about the branch as it was, used to authorize a handover
// happening now.
func TestTrustedMainMovingWhileTheSuccessorDecidesRefusesTheTransition(t *testing.T) {
	harness, record, subject := launchFixture(t)
	harness.decide = func(asked ControllerHandoff) (ControllerHandoff, error) {
		// Somebody merges while the successor is replaying.
		harness.trusted = RevisionRecord{Revision: strings.Repeat("9", 40), Tree: "tree-9"}
		return asked, nil
	}

	launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

	if launch.Committed {
		t.Fatalf("a successor built from a revision main had left was handed the role: %+v", launch)
	}
	if harness.began != 0 {
		t.Fatal("the transition was begun after trusted main moved")
	}
	if launch.Evaluate.Outcome != StepSucceeded || launch.Recheck.Outcome != StepRefused {
		t.Fatalf("the refusal is not the currency one: evaluate=%q recheck=%q",
			launch.Evaluate.Outcome, launch.Recheck.Outcome)
	}
	if harness.resumed != 1 {
		t.Fatalf("intake was resumed %d times after a pre-commitment refusal", harness.resumed)
	}
	if harness.successor.abandoned != 1 {
		t.Fatal("the successor that will not be used was left running")
	}
}

// AFTER COMMITMENT, INTAKE NEVER COMES BACK - including when the predecessor's
// own half failed early enough that the gate it would have closed is still
// only held.
//
// Committed is set before Begin is attempted precisely because that operation
// can partially drain or release and report failure either way. Resuming
// afterwards would have this process admit work again in the one state where
// it has already decided it may not.
func TestIntakeIsNotResumedAfterCommitmentEvenWhenBeginFailsEarly(t *testing.T) {
	harness, record, subject := launchFixture(t)
	// A Begin that refuses BEFORE it drains: the record is already there, so
	// PutControllerHandoff returns without the gate having been touched.
	harness.beginErr = fmt.Errorf("handoff %s is already recorded; resolve it before starting another", record.ID)

	launch := LaunchSuccession(context.Background(), record, subject, harness.ports())

	if !launch.Committed {
		t.Fatalf("a failure after the drain was attempted left the controller believing it may serve: %+v", launch)
	}
	if harness.quiesced != 1 {
		t.Fatalf("intake was suspended %d times", harness.quiesced)
	}
	if harness.resumed != 0 {
		t.Fatal("a superseded controller resumed admitting work because its own half failed early")
	}
}
