package runtime

// STARTING THE SUCCESSOR, AND THEN GETTING OUT OF ITS WAY.
//
// Everything below this file made a transition SAFE to perform. This one
// performs it live: a predecessor that is serving spawns the successor it
// prepared, hands the role over, and stops. It is the last piece of #234 that
// a human currently does by hand, and the only one where both controllers are
// real processes at the same time.
//
// THE ORDER IS THE WHOLE DESIGN.
//
//	spawn     the successor exists and is INERT: no role, no service, no
//	          durable write, nothing it could do if this attempt is abandoned
//	identify  it says which generation it is, and that must be the generation
//	          the prepared record names - asked of the process, not of the
//	          path it was started from
//	recheck   trusted main is still the subject this successor was built for
//	--------- the point of no return ------------------------------------------
//	begin     the predecessor stops admitting work and releases the role
//	signal    the successor is told it may proceed
//	activate  it acquires, revalidates, activates and opens service, and says
//	          so - or says why not
//
// EVERYTHING EXPENSIVE HAPPENS BEFORE THE POINT OF NO RETURN, deliberately. A
// successor that cannot start, cannot prove what it is, or is already stale
// costs this attempt and nothing else: the predecessor is still serving,
// nothing durable was written, and the process that was spawned is abandoned
// without ever having held anything.
//
// AFTER IT, THE PREDECESSOR DOES NOT COME BACK. It drained and released; it is
// no longer the controller, and reacquiring the role because the successor
// disappointed it would be a process deciding it is still in charge because
// nobody else visibly is. That is the inference this stack exists to remove.
// What happens instead is what the durable record already says: the transition
// names who may recover, the stable entrypoint still names the predecessor's
// artifact until an activation moves it, and a controller started against that
// state resolves it. Availability loses; authority is not reconstructed.
//
// THIS FILE DECIDES NOTHING ABOUT AUTHORITY. It sequences operations that each
// enforce their own - the same discipline as the reconciler - and every step it
// reports is a step one of them answered.

import (
	"context"
	"fmt"
	"time"
)

// InertSuccessor is a spawned successor process that has taken nothing.
//
// It is an interface because the transport is a detail of the composition root
// - a pipe to a child process today - and because a test must be able to place
// a failure at each step without spawning anything.
type InertSuccessor interface {
	// Identify blocks until the successor reports what generation it is. It
	// asks the PROCESS rather than trusting the path it was started from: an
	// artifact directory is a claim, and the binary that is actually running
	// is the fact.
	Identify() (ControllerBinding, error)
	// Proceed tells the successor the role has been released.
	Proceed(handoffID string) error
	// AwaitActive blocks until the successor reports that it is the activated
	// generation and is admitting work, or reports why it is not.
	AwaitActive() error
	// Abandon stops a successor that will not be used. It is only ever called
	// before the point of no return, when the successor holds nothing.
	Abandon() error
}

// LaunchStep is one step's outcome, kept separate from the others so an
// operator reading a failed upgrade can see how far it got. "identify refused"
// and "activate refused" are different events with different consequences, and
// one error string would flatten them.
type LaunchStep struct {
	Outcome StepOutcome `json:"outcome"`
	Detail  string      `json:"detail,omitempty"`
}

func stepRefused(format string, args ...any) LaunchStep {
	return LaunchStep{Outcome: StepRefused, Detail: fmt.Sprintf(format, args...)}
}

// SuccessionLaunch is what one live transition did.
type SuccessionLaunch struct {
	HandoffID string     `json:"handoff_id"`
	Spawn     LaunchStep `json:"spawn"`
	Identify  LaunchStep `json:"identify"`
	Recheck   LaunchStep `json:"recheck"`
	Begin     LaunchStep `json:"begin"`
	Signal    LaunchStep `json:"signal"`
	Activate  LaunchStep `json:"activate"`

	// Committed reports that the predecessor gave up the role, whatever
	// happened afterwards. It is the answer to the only question the calling
	// process has left: may I keep serving?
	//
	// It is set the moment the drain is ATTEMPTED rather than when it is
	// confirmed, because a failure inside that operation can leave work
	// admission closed or the role released, and a process that cannot tell
	// which must assume the more restrictive one. Stopping a controller that
	// could have continued costs an upgrade; continuing to serve without the
	// role is the thing no part of this system is allowed to do.
	Committed bool `json:"committed"`
	// Served reports that the successor proved it is activated and admitting
	// work. A launch can be committed without being served, and that is the
	// case an operator has to see.
	Served    bool      `json:"served"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// Summary is one line for a report.
func (l SuccessionLaunch) Summary() string {
	switch {
	case l.Served:
		return fmt.Sprintf("transition %s activated and the successor is serving", l.HandoffID)
	case l.Committed:
		return fmt.Sprintf("transition %s passed the point of no return and the successor is not serving: %s",
			l.HandoffID, l.firstRefusal())
	default:
		return fmt.Sprintf("transition %s was not attempted to completion and this controller is still serving: %s",
			l.HandoffID, l.firstRefusal())
	}
}

func (l SuccessionLaunch) firstRefusal() string {
	for _, step := range []LaunchStep{l.Spawn, l.Identify, l.Recheck, l.Begin, l.Signal, l.Activate} {
		if step.Outcome == StepRefused {
			return step.Detail
		}
	}
	return "no step reported a refusal"
}

// SuccessionPorts are the effects a live transition performs.
type SuccessionPorts struct {
	// Spawn starts the successor from the artifact the record names, inert.
	Spawn func(artifact, handoffID string) (InertSuccessor, error)
	// ObserveTrustedMain answers what trusted main is NOW, immediately before
	// the point of no return.
	ObserveTrustedMain func(context.Context) (RevisionRecord, error)
	// Begin is the predecessor's half of the choreography: persist the
	// prepared record, stop admitting work, release the role. It is a port
	// onto the controller service rather than a direct call to BeginHandoff,
	// because the role and the admission gate belong to that service and this
	// file must not be able to reach either.
	Begin func(prepared ControllerHandoff) (ControllerHandoff, error)
	Now   func() time.Time
}

func (p SuccessionPorts) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now()
}

// LaunchSuccession performs one live transition.
//
// The prepared record is the authorization: it names both controllers, the
// artifact to start, and the runs that were preflighted. Nothing here
// recomputes any of that - a launcher that decided for itself which successor
// to start would be choosing the next controller, which is a decision the
// preflight already made and recorded.
func LaunchSuccession(ctx context.Context, prepared ControllerHandoff, subject RevisionRecord, ports SuccessionPorts) SuccessionLaunch {
	launch := SuccessionLaunch{HandoffID: prepared.ID, StartedAt: ports.now()}
	settle := func() SuccessionLaunch {
		launch.EndedAt = ports.now()
		return launch
	}

	// A record that is not a prepared, compatible transition is not an
	// authorization. BeginHandoff refuses the same two things; refusing them
	// here means no process is spawned to discover it.
	if prepared.Phase != HandoffPrepared {
		launch.Spawn = stepRefused("transition %s is at phase %q and a launch begins from %q",
			prepared.ID, prepared.Phase, HandoffPrepared)
		return settle()
	}
	if !prepared.Compatible() {
		launch.Spawn = stepRefused("transition %s is blocked: %v", prepared.ID, prepared.Blockers())
		return settle()
	}
	expected, err := prepared.Successor.Binding.Digest()
	if err != nil {
		launch.Spawn = stepRefused("the successor the transition names could not be digested: %v", err)
		return settle()
	}

	successor, err := ports.Spawn(prepared.Successor.ArtifactPath, prepared.ID)
	if err != nil {
		launch.Spawn = stepRefused("the successor could not be started: %v", err)
		return settle()
	}
	launch.Spawn = LaunchStep{Outcome: StepSucceeded}
	announced, err := successor.Identify()
	if err != nil {
		launch.Identify = stepRefused("the successor did not report what it is: %v", err)
		return abandonAnd(successor, &launch.Identify, settle)
	}
	stated, err := announced.Digest()
	switch {
	case err != nil:
		launch.Identify = stepRefused("the successor's own identity could not be digested: %v", err)
		return abandonAnd(successor, &launch.Identify, settle)
	case stated != expected:
		// THE RUNNING PROCESS, NOT THE PATH. The record names a binding and
		// the process announces one; a mismatch means the artifact directory
		// does not contain what the preflight decided about, and continuing
		// would hand the role to a controller nobody evaluated.
		launch.Identify = stepRefused(
			"the successor process reports %s and transition %s was prepared for %s",
			shortSHA(stated), prepared.ID, shortSHA(expected))
		return abandonAnd(successor, &launch.Identify, settle)
	}
	launch.Identify = LaunchStep{Outcome: StepSucceeded}

	// STILL CURRENT? The build was validated against trusted main when it was
	// produced, and time has passed. An observation that cannot be made fails
	// closed for the same reason it does in the updater: "nobody could say
	// otherwise" is not evidence of currency.
	observed, err := ports.ObserveTrustedMain(ctx)
	switch {
	case err != nil:
		launch.Recheck = stepRefused("trusted main could not be re-observed before the point of no return: %v", err)
		return abandonAnd(successor, &launch.Recheck, settle)
	case observed.Revision != subject.Revision || observed.Tree != subject.Tree:
		launch.Recheck = stepRefused("trusted main is %s and this successor was built from %s",
			shortSHA(observed.Revision), shortSHA(subject.Revision))
		return abandonAnd(successor, &launch.Recheck, settle)
	}
	launch.Recheck = LaunchStep{Outcome: StepSucceeded}

	// ---------------------------------------------------------------------
	// The point of no return.
	// ---------------------------------------------------------------------
	launch.Committed = true
	if _, err := ports.Begin(prepared); err != nil {
		launch.Begin = stepRefused("the predecessor's half of the transition did not complete: %v", err)
		return settle()
	}
	launch.Begin = LaunchStep{Outcome: StepSucceeded}

	if err := successor.Proceed(prepared.ID); err != nil {
		launch.Signal = stepRefused("the successor could not be told to proceed: %v", err)
		return settle()
	}
	launch.Signal = LaunchStep{Outcome: StepSucceeded}

	if err := successor.AwaitActive(); err != nil {
		launch.Activate = stepRefused("the successor did not become the serving controller: %v", err)
		return settle()
	}
	launch.Activate = LaunchStep{Outcome: StepSucceeded}
	launch.Served = true
	return settle()
}

// abandonAnd stops a successor that will not be used, before the point of no
// return, and records the refusal that caused it. The successor holds nothing
// at this point, so abandoning it costs an upgrade and no state.
func abandonAnd(successor InertSuccessor, step *LaunchStep, settle func() SuccessionLaunch) SuccessionLaunch {
	if err := successor.Abandon(); err != nil {
		step.Detail += fmt.Sprintf(" (and the successor process could not be stopped: %v)", err)
	}
	return settle()
}
