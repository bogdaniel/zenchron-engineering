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
//	quiesce   intake is suspended and the work this controller started is
//	          finished, so the durable head cannot move again
//	evaluate  the SUCCESSOR decides whether it can continue every live run,
//	          against that quiescent head, using its own code
//	recheck   trusted main is STILL the subject this successor was built for,
//	          asked last because that is where it has to be true
//	--------- the point of no return ------------------------------------------
//	begin     the predecessor stops admitting work and releases the role
//	signal    the successor is told it may proceed
//	activate  it acquires, revalidates, activates and opens service, and says
//	          so - or says why not
//
// QUIESCENCE AND EVALUATION ARE ONE IDEA IN TWO STEPS. Compatibility is a
// statement about a durable state, and a statement about a state that is still
// moving is worth nothing: the predecessor's own run drivers are inside
// providers and journal writes, and they would keep appending while the
// successor was deciding, and after the role changed hands. So intake is
// suspended, the drivers are allowed to finish, and only then is the question
// asked - of the successor, because "can you read this journal" is only
// meaningful asked of the code that would read it.
//
// THE ANSWER ARRIVES BEFORE ANYTHING IS GIVEN UP, which is the point. A
// successor that has dropped an event type the predecessor understands refuses
// here, intake resumes, and the controller keeps serving: an incompatible
// successor costs an update. The revalidation the successor performs after
// acquiring ownership stays exactly as it was and is now a race check on a
// state nothing can have moved, rather than the first time anybody asked.
//
// CURRENCY IS PROVEN LAST, and that is an ordering law rather than a
// preference. Quiescence waits for work this controller started, which is
// bounded in tens of minutes, and the successor's evaluation replays every
// live journal; trusted main can move through both. A check that happened
// before them would be a statement about a branch as it was some time ago,
// used to authorize a handover happening now. Whatever the point of no return
// moves to, the last proof of currency moves with it.
//
// INTAKE COMES BACK ONLY BEFORE COMMITMENT. The hold is reversible precisely so
// a refusal costs an update instead of a controller - and Committed is set
// BEFORE Begin is attempted, because that operation can partially drain or
// release and report failure either way. Releasing the hold afterwards would
// have this process resume admitting work in the one state where it has
// already decided it may not: uncertainty after commitment narrows authority
// and never widens it.
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
	Quiesce   LaunchStep `json:"quiesce"`
	Evaluate  LaunchStep `json:"evaluate"`
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
	for _, step := range []LaunchStep{l.Spawn, l.Identify, l.Quiesce, l.Evaluate, l.Recheck, l.Begin, l.Signal, l.Activate} {
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
	// Quiesce suspends intake and waits for the work this controller started,
	// returning the one way to put intake back. It is called before the point
	// of no return and its release runs on every refusal after it.
	Quiesce func(ctx context.Context) (resume func(), err error)
	// Evaluate asks the SUCCESSOR to decide the transition against the state
	// as it now stands, and returns the record carrying its decisions. That
	// record - not the predecessor's earlier screen of the same question - is
	// what the transition is begun with, because the admissions written during
	// the handover are the successor's own.
	Evaluate func(ctx context.Context, prepared ControllerHandoff) (ControllerHandoff, error)
	Now      func() time.Time
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

	// NOTHING MAY APPEND WHILE THE SUCCESSOR DECIDES. Intake is suspended and
	// the run drivers this controller started are allowed to finish, so the
	// journal the successor is about to read is the journal it will inherit.
	resume, err := ports.Quiesce(ctx)
	if err != nil {
		launch.Quiesce = stepRefused("the work this controller started could not be brought to a stop: %v", err)
		return abandonAnd(successor, &launch.Quiesce, settle)
	}
	launch.Quiesce = LaunchStep{Outcome: StepSucceeded}
	// INTAKE COMES BACK ON EVERY PRE-COMMITMENT EXIT AND ON NO OTHER. The flag
	// is not a duplicate of launch.Committed for a reader's benefit: it is what
	// makes the deferred release unreachable once this process has declared
	// itself superseded, including on the paths where Begin failed so early
	// that the gate it would have closed is still only held.
	committed := false
	defer func() {
		if !committed {
			resume()
		}
	}()

	// THE SUCCESSOR DECIDES, NOT THIS PROCESS. A predecessor answering "can B
	// read this journal" would be answering from its own decoders, which is
	// the question nobody needs answered.
	decided, err := ports.Evaluate(ctx, prepared)
	if err != nil {
		launch.Evaluate = stepRefused("the successor could not decide the transition: %v", err)
		return abandonAnd(successor, &launch.Evaluate, settle)
	}
	if err := decidedMatches(prepared, decided); err != nil {
		launch.Evaluate = stepRefused("the successor decided a different transition: %v", err)
		return abandonAnd(successor, &launch.Evaluate, settle)
	}
	if !decided.Compatible() {
		// AN INCOMPATIBLE SUCCESSOR COSTS AN UPDATE, NOT A CONTROLLER. Intake
		// resumes, the successor is stopped, and this process keeps serving
		// the runs it was already serving.
		launch.Evaluate = stepRefused("the successor cannot continue every live run: %v", decided.Blockers())
		return abandonAnd(successor, &launch.Evaluate, settle)
	}
	launch.Evaluate = LaunchStep{Outcome: StepSucceeded}

	// STILL CURRENT, ASKED LAST. The build was validated against trusted main
	// when it was produced, and everything since - the quiescence wait, the
	// successor's replay of every live journal - takes time a branch can move
	// in. An observation that cannot be made fails closed for the same reason
	// it does in the updater: "nobody could say otherwise" is not evidence of
	// currency.
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
	committed = true
	launch.Committed = true
	if _, err := ports.Begin(decided); err != nil {
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

// decidedMatches requires the successor's record to be the transition it was
// asked about.
//
// The successor is trusted to DECIDE compatibility and not to choose what it is
// deciding about: a record naming other controllers, or at another phase, is
// not an answer to this question, and beginning a transition from it would let
// the successor nominate itself under a different binding than the one that was
// identified and re-observed.
func decidedMatches(asked, decided ControllerHandoff) error {
	if decided.ID != asked.ID {
		return fmt.Errorf("it decided transition %s and it was asked about %s", decided.ID, asked.ID)
	}
	if decided.Phase != HandoffPrepared {
		return fmt.Errorf("it returned a record at phase %q", decided.Phase)
	}
	for _, party := range []struct {
		name           string
		asked, was     ControllerBinding
		askedAt, wasAt string
	}{
		{"predecessor", asked.Predecessor.Binding, decided.Predecessor.Binding, asked.Predecessor.ArtifactPath, decided.Predecessor.ArtifactPath},
		{"successor", asked.Successor.Binding, decided.Successor.Binding, asked.Successor.ArtifactPath, decided.Successor.ArtifactPath},
	} {
		expected, expectedErr := party.asked.Digest()
		got, gotErr := party.was.Digest()
		if expectedErr != nil || gotErr != nil || expected != got {
			return fmt.Errorf("it names a different %s", party.name)
		}
		if party.askedAt != party.wasAt {
			return fmt.Errorf("it names a different %s artifact: %s", party.name, party.wasAt)
		}
	}
	return nil
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
