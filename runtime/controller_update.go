package runtime

// NOTICING THAT TRUSTED MAIN MOVED, AND PREPARING THE SUCCESSOR IT NAMES.
//
// This is the first half of the updater: observe, decide whether anything is
// owed, build the successor through the existing governed path, validate what
// came out, and stop with a prepared transition. It never drains, never
// releases the role and never hands anything off - a broken build must cost an
// update, not availability, and the drain boundary deserves its own review.
//
// MAIN MOVING IS NOT ADOPTION AUTHORITY. "GitHub says main changed" is a reason
// to LOOK; the trust root decides whether what it found may become a
// controller. That verification is not re-implemented here: BuildAdoptedController
// already observes the ruleset, verifies it against the policy, re-derives the
// revision and tree from the remote-observed head and proves containment, and
// it refuses rather than building when any of that fails. This file's job is to
// decide when to ask it, and to bind the answer to the exact subject it asked
// about.
//
// THE ATTEMPT IS BOUND TO ONE EXACT REVISION AND TREE. Main can move while a
// build is running - it takes minutes - and an attempt that validated B(M1)
// against an observation of M2 would be claiming the successor is current when
// it is one commit stale. So the subject is captured once, the build is pinned
// to it, the provenance that comes back is checked against it, and an
// observation that has moved marks the result SUPERSEDED rather than silently
// accepting it.
//
// IT DOES NOT BLOCK THE TICK. A governed build runs a container for minutes and
// the supervisor pass that starts it also drives runs; holding the pass would
// stall the fleet to upgrade the thing running it. The build runs on its own
// goroutine and each pass reports progress, which is the one piece of
// asynchrony here - not a scheduler, not a daemon, and no second cadence.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// UpdateState is where one update attempt stands.
type UpdateState string

const (
	// UpdateIdle is nothing owed: trusted main is what this controller is.
	UpdateIdle UpdateState = "idle"
	// UpdateObservationFailed is a trusted-main observation that could not be
	// made. The controller keeps serving and looks again later.
	UpdateObservationFailed UpdateState = "observation_failed"
	// UpdateBuilding is a governed build in flight for the bound subject.
	UpdateBuilding UpdateState = "building"
	// UpdateReady is a validated successor artifact, with a prepared
	// transition, waiting for the handoff this slice deliberately does not do.
	UpdateReady UpdateState = "ready"
	// UpdateRefused is the build or its validation refusing. The controller
	// keeps serving.
	UpdateRefused UpdateState = "refused"
	// UpdateSuperseded is a result whose subject stopped being trusted main
	// while it was produced. It is never activated: its evidence describes a
	// world that has moved.
	UpdateSuperseded UpdateState = "superseded"
	// UpdateBlocked is a successor that built and validated, and that the live
	// runs cannot be handed to.
	UpdateBlocked UpdateState = "blocked"
)

// ControllerUpdate is one attempt's durable-shaped account of itself.
type ControllerUpdate struct {
	State UpdateState `json:"state"`
	// Subject is the EXACT trusted-main revision and tree this attempt is
	// about. Every later step is checked against it rather than against a
	// fresher observation.
	Subject RevisionRecord `json:"subject"`
	// Artifact is the built successor, once there is one.
	Artifact string `json:"artifact,omitempty"`
	// Handoff is the prepared transition, once the successor is compatible.
	Handoff   string    `json:"handoff,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// ControllerUpdaterPorts are the effects, each of which already exists.
type ControllerUpdaterPorts struct {
	// ObserveTrustedMain reports the revision and tree trusted main is at. It
	// is a cheap read used only to decide whether to ask for a build; the
	// authoritative verification happens inside the build itself.
	ObserveTrustedMain func(context.Context) (RevisionRecord, error)
	// Build is the governed adopted build, unchanged. It observes the trust
	// root, verifies it, re-derives the subject and refuses rather than
	// building when any of that fails.
	Build func(context.Context, AdoptedBuildRequest) (AdoptedBuildProvenance, error)
	// Preflight classifies the live runs under the successor.
	Preflight func(ControllerHandoff) error
	// Prepare builds the prepared transition for a validated successor.
	Prepare func(context.Context, ControllerBinding, string) (ControllerHandoff, error)
}

// ControllerUpdater decides when to ask for a successor and reports where the
// asking got to.
type ControllerUpdater struct {
	ports ControllerUpdaterPorts
	// predecessor is the running controller's full binding: program identity,
	// build and configuration digest. The successor is this value with ONLY
	// the build replaced, because succession requires the controller id and
	// the effective configuration to be unchanged - a successor assembled from
	// a build alone would be refused by the very preflight this updater
	// prepares for.
	predecessor ControllerBinding
	request     AdoptedBuildRequest

	mu      sync.Mutex
	current *ControllerUpdate
	running bool
	// retryAfter keeps a refused subject from being rebuilt every tick. A
	// governed build is minutes of container time; retrying it on a poll
	// interval would be a denial of service against the operator's own machine.
	retryAfter time.Time
}

// NewControllerUpdater binds an updater to this controller's identity and the
// build request the composition root configured.
func NewControllerUpdater(predecessor ControllerBinding, request AdoptedBuildRequest, ports ControllerUpdaterPorts) *ControllerUpdater {
	return &ControllerUpdater{predecessor: predecessor, request: request, ports: ports}
}

// Attempt advances one update, without blocking.
//
// It returns what is known right now: a build already in flight reports
// progress, a finished one reports its result, and an idle controller whose
// trusted main has moved starts one.
func (u *ControllerUpdater) Attempt(ctx context.Context, now time.Time) ControllerUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.running {
		// A build is in flight. The only question worth asking each pass is
		// whether its subject is still trusted main; asking anything else
		// would mean interrupting a container to re-read a branch.
		return u.progress(ctx, now)
	}
	if u.predecessor.Build == nil {
		// An unattested controller has no lineage to succeed from, so there is
		// no successor it could prepare. Refusing here is the same law the
		// succession evaluation applies, stated before minutes of container
		// time are spent discovering it.
		u.current = &ControllerUpdate{State: UpdateRefused, EndedAt: now,
			Detail: "this controller is not an adopted build, so it has no successor to prepare"}
		return *u.current
	}
	observed, err := u.ports.ObserveTrustedMain(ctx)
	if err != nil {
		// THE CONTROLLER KEEPS SERVING. An unobservable trust root is a reason
		// not to upgrade, never a reason to stop.
		u.current = &ControllerUpdate{State: UpdateObservationFailed, Detail: err.Error(), EndedAt: now}
		return *u.current
	}
	if observed.Revision == u.predecessor.Build.SourceRevision {
		u.current = &ControllerUpdate{State: UpdateIdle, Subject: observed, EndedAt: now}
		return *u.current
	}
	if settled := u.settledFor(observed, now); settled != nil {
		return *settled
	}
	return u.start(ctx, observed, now)
}

// progress answers while a build is running, and notices its subject moving.
func (u *ControllerUpdater) progress(ctx context.Context, now time.Time) ControllerUpdate {
	update := *u.current
	observed, err := u.ports.ObserveTrustedMain(ctx)
	if err == nil && observed.Revision != update.Subject.Revision {
		// The build will finish and its result will be refused: a successor
		// built from a revision that is no longer trusted main is not a
		// successor, and validating it against the newer observation would be
		// exactly the substitution this file exists to prevent.
		update.Detail = fmt.Sprintf(
			"trusted main moved to %s while %s was building; this result will be superseded",
			shortSHA(observed.Revision), shortSHA(update.Subject.Revision))
		u.current.Detail = update.Detail
	}
	return update
}

// settledFor reports an existing conclusion about this exact subject, so the
// same trusted main is never built twice.
func (u *ControllerUpdater) settledFor(observed RevisionRecord, now time.Time) *ControllerUpdate {
	if u.current == nil || u.current.Subject.Revision != observed.Revision {
		return nil
	}
	switch u.current.State {
	case UpdateReady, UpdateBlocked:
		// Already built and validated. Building it again would produce the
		// same artifact and waste minutes of container time.
		return u.current
	case UpdateRefused, UpdateSuperseded, UpdateObservationFailed:
		// A post-build observation failure carries a subject and an artifact,
		// so it earns the same wait: rebuilding the same revision on the next
		// poll because a branch read failed once would spend minutes of
		// container time to produce the artifact that already exists.
		if now.Before(u.retryAfter) {
			return u.current
		}
	}
	return nil
}

// start launches the governed build for one exact subject.
func (u *ControllerUpdater) start(ctx context.Context, observed RevisionRecord, now time.Time) ControllerUpdate {
	u.current = &ControllerUpdate{State: UpdateBuilding, Subject: observed, StartedAt: now}
	u.running = true
	request := u.request
	// PINNED. The build is told exactly which revision to produce, so it
	// cannot quietly follow a branch that moves underneath it.
	request.Revision = observed.Revision
	subject, startedAt := observed, now
	go func() {
		provenance, err := u.ports.Build(ctx, request)
		// The goroutine carries everything it needs to describe the attempt it
		// belongs to. Reaching back into the updater's mutable state to
		// reconstruct that would be reading it without the lock the rest of
		// this type takes.
		u.finish(ctx, subject, startedAt, provenance, err)
	}()
	return *u.current
}

// finish validates what the build produced against the subject it was asked
// for, and prepares the transition when it holds.
func (u *ControllerUpdater) finish(ctx context.Context, subject RevisionRecord, startedAt time.Time, provenance AdoptedBuildProvenance, buildErr error) {
	settle := func(update ControllerUpdate) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.running = false
		if update.State == UpdateRefused || update.State == UpdateSuperseded ||
			update.State == UpdateObservationFailed {
			u.retryAfter = update.EndedAt.Add(updateRetryInterval)
		}
		u.current = &update
	}
	now := time.Now().UTC()
	base := ControllerUpdate{Subject: subject, StartedAt: startedAt, EndedAt: now}

	if buildErr != nil {
		base.State, base.Detail = UpdateRefused, buildErr.Error()
		settle(base)
		return
	}
	// THE ARTIFACT MUST BE THE SUBJECT. A build that produced a different
	// revision or tree than the one this attempt is bound to is not this
	// attempt's successor, whatever else it may be.
	if provenance.Source.Revision != subject.Revision || provenance.Source.Tree != subject.Tree {
		base.State = UpdateRefused
		base.Detail = fmt.Sprintf("the build produced %s/%s and this attempt is bound to %s/%s",
			shortSHA(provenance.Source.Revision), shortSHA(provenance.Source.Tree),
			shortSHA(subject.Revision), shortSHA(subject.Tree))
		settle(base)
		return
	}
	// AND THE SUBJECT MUST HAVE BEEN TRUSTED MAIN AT PUBLICATION. The builder
	// deliberately permits publishing an ANCESTOR of trusted main: it proves
	// containment rather than currency, so a manual build of an older adopted
	// commit is legitimate. For an updater it is not - a successor built from a
	// commit that main had already moved past is superseded the moment it
	// exists, and the provenance says so in a field this attempt would
	// otherwise ignore.
	if provenance.TrustedMain.Revision != subject.Revision || provenance.TrustedMain.Tree != subject.Tree {
		base.State = UpdateSuperseded
		base.Detail = fmt.Sprintf("trusted main was %s when this successor was published and it was built from %s",
			shortSHA(provenance.TrustedMain.Revision), shortSHA(subject.Revision))
		settle(base)
		return
	}
	if !provenance.SelfProbe.Matched {
		base.State = UpdateRefused
		base.Detail = "the built controller did not report the generation it was built as"
		settle(base)
		return
	}
	base.Artifact = provenance.OutputPath

	// STILL TRUSTED MAIN? An observation that moved does not invalidate the
	// artifact - it is a perfectly good build of a commit that is no longer
	// current - but it does mean this attempt has nothing to activate.
	//
	// AND AN OBSERVATION THAT COULD NOT BE MADE IS NOT A PASS. Falling through
	// when the answer is unknown would make "ready" mean "nobody could say
	// otherwise", which is the fail-open shape every other decision in this
	// stack refuses. The artifact is kept and reported; it is not prepared.
	observed, err := u.ports.ObserveTrustedMain(ctx)
	switch {
	case err != nil:
		base.State = UpdateObservationFailed
		base.Detail = "the successor was built and trusted main could not be re-observed to confirm it is current: " + err.Error()
		settle(base)
		return
	case observed.Revision != subject.Revision || observed.Tree != subject.Tree:
		base.State = UpdateSuperseded
		base.Detail = fmt.Sprintf("trusted main is %s and this successor was built from %s",
			shortSHA(observed.Revision), shortSHA(subject.Revision))
		settle(base)
		return
	}

	// ONLY THE BUILD CHANGES. Succession requires the controller id and the
	// effective configuration to be identical, so the successor is this
	// controller's own binding with a different build - not a binding
	// assembled here, which would differ in exactly the members the preflight
	// checks.
	successor := u.predecessor
	successor.Build = &ControllerBuild{
		Kind: ControllerAdopted, Version: provenance.Version,
		SourceRevision: provenance.Source.Revision, SourceTree: provenance.Source.Tree,
		BinarySHA256: provenance.BinarySHA256,
	}
	prepared, err := u.ports.Prepare(ctx, successor, provenance.OutputPath)
	if err != nil {
		base.State, base.Detail = UpdateRefused, err.Error()
		settle(base)
		return
	}
	base.Handoff = prepared.ID
	if err := u.ports.Preflight(prepared); err != nil {
		// A VALIDATED SUCCESSOR THE LIVE RUNS CANNOT MOVE TO. The controller
		// keeps serving and the operator is told which runs are in the way.
		base.State, base.Detail = UpdateBlocked, err.Error()
		settle(base)
		return
	}
	base.State = UpdateReady
	settle(base)
}

// updateRetryInterval keeps a refused or superseded subject from being rebuilt
// on every poll. A governed build is minutes of container time on the
// operator's own machine.
const updateRetryInterval = 10 * time.Minute

// Current reports the latest conclusion WITHOUT advancing anything.
//
// Attempt is the only thing that starts work, so a reader that wanted to know
// where an update got to would otherwise have to risk starting the next one to
// find out.
func (u *ControllerUpdater) Current() (ControllerUpdate, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.current == nil {
		return ControllerUpdate{}, false
	}
	return *u.current, true
}

// Describe renders one update for a report line.
func (u ControllerUpdate) Describe() string {
	switch u.State {
	case UpdateIdle:
		return "trusted main is the running controller"
	case UpdateObservationFailed:
		return "trusted main could not be observed: " + u.Detail
	default:
		line := fmt.Sprintf("%s for %s", u.State, shortSHA(u.Subject.Revision))
		if u.Detail != "" {
			line += ": " + u.Detail
		}
		return line
	}
}
