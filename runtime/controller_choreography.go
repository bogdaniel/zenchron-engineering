package runtime

// THE HANDOFF PROTOCOL: two live processes, one of which may serve.
//
// The three files before this one define primitives and truth - which
// successor may continue a run, what the durable transition record says, and
// which generation a process can prove itself to be. None of them says how two
// processes actually exchange the scheduler without split-brain, without
// admitting work on unproven authority, and without leaving a state that only a
// human can interpret. That is this file.
//
// WHAT "ADMIT" MEANS, settled here once because one verb must not mean two
// things. There are two admissions in this system and they are unrelated:
//
//	succession admission  evidence in a RUN's journal that the successor may
//	                      continue it. Written after revalidation, never at
//	                      preflight. See controller_succession.go.
//	work admission        the right of a controller to take on and drive work
//	                      at all. It is what draining removes from the
//	                      predecessor and what activation grants the successor.
//
// The order below is written in those terms, so no step can be read as the
// other kind of admission:
//
//	predecessor stops WORK ADMISSION      (drain)
//	predecessor releases ownership
//	successor acquires ownership          <- ownership linearization
//	successor revalidates after acquiring
//	successor writes SUCCESSION ADMISSIONS
//	successor proves its generation
//	successor records durable activation  <- authority linearization
//	projection repaired to the successor  <- projection linearization
//	successor takes WORK ADMISSION
//	predecessor exits
//
// THREE LINEARIZATION POINTS, deliberately distinct:
//
//	OWNERSHIP is the exclusive right to perform the transition. It is the
//	scheduler lock. Holding it permits the choreography and nothing else.
//	AUTHORITY is the durable activation record. That, and only that, is when
//	the successor becomes the active generation in the control model - not
//	acquisition, not a process asserting it, not the pointer.
//	PROJECTION is the rename of the stable entrypoint. It reflects authority
//	that already exists and can never create it.
//
// OWNERSHIP IS NOT ACTIVATION. A successor can hold exclusive ownership while
// being neither activated nor serving, and keeping those apart is what stops
// the lock from becoming a second source of truth about which generation is
// live.
//
// AVAILABILITY MAY FALL TO ZERO; ADMISSION AUTHORITY MUST NEVER RISE ABOVE ONE.
// Between the predecessor draining and the successor activating, NOBODY is
// permitted to admit work, and that gap is deliberate: a moment with no
// controller serving is an availability cost, while a moment with two is a
// correctness failure that durable state cannot describe. Any future change
// that shortens the gap by letting both parties serve across it is trading the
// invariant for the symptom.
//
// INSPECT BEFORE ACQUIRE FOR PLANNING; TRUST ONLY WHAT IS REVALIDATED AFTER.
// Everything the preflight learned was learned while another process could
// still act, so it is stale by definition the moment ownership changes hands.

import (
	"fmt"
	"time"
)

// HandoffRole is which half of the protocol a process is performing.
type HandoffRole string

const (
	RolePredecessor HandoffRole = "predecessor"
	RoleSuccessor   HandoffRole = "successor"
)

// WorkAdmission is whether a controller may take on work right now, and why
// not when it may not.
//
// It is computed from the durable record rather than from process state,
// because the question "may I serve" must have the same answer whoever asks
// and whenever they ask it. A predecessor that has drained and a successor that
// has not activated are both refused, which is what makes "at most one process
// admitting work" a property of the record instead of a race.
type WorkAdmission struct {
	Permitted bool   `json:"permitted"`
	Reason    string `json:"reason,omitempty"`
}

// WorkAdmissionFor answers for one controller against one in-flight handoff.
//
// A controller with no handoff in flight is not this function's business: the
// caller admits work as it always did. What this governs is the window, and in
// the window the rule is exact - the predecessor may serve until it drains, the
// successor may serve only once the record says activated AND it can prove it
// is the generation that record names.
func WorkAdmissionFor(record *ControllerHandoff, self ControllerSelfRecord, controller string) WorkAdmission {
	if record == nil {
		return WorkAdmission{Permitted: true}
	}
	predecessor, predecessorErr := record.Predecessor.Binding.Digest()
	successor, successorErr := record.Successor.Binding.Digest()
	if predecessorErr != nil || successorErr != nil {
		return WorkAdmission{Reason: "the handoff record does not yield controller identities"}
	}
	switch controller {
	case predecessor:
		// A FAILED HANDOFF RESTORES THE PREDECESSOR only when the record says
		// it recovers; that is the eligibility statement, not the phase.
		if record.Phase == HandoffFailed && record.MayRecover(controller) {
			return WorkAdmission{Permitted: true}
		}
		if record.Phase == HandoffPrepared {
			return WorkAdmission{Permitted: true}
		}
		return WorkAdmission{Reason: fmt.Sprintf(
			"this controller drained for handoff %s and does not resume work at phase %q", record.ID, record.Phase)}
	case successor:
		if record.Phase != HandoffActivated {
			return WorkAdmission{Reason: fmt.Sprintf(
				"handoff %s is at phase %q; work admission follows the durable activation, not ownership", record.ID, record.Phase)}
		}
		if err := self.ProvesGeneration(record.Successor.Binding); err != nil {
			return WorkAdmission{Reason: "this process is not the activated generation: " + err.Error()}
		}
		return WorkAdmission{Permitted: true}
	}
	// A THIRD CONTROLLER during a handoff is neither party. It is refused
	// rather than reasoned about: two generations are already exchanging the
	// scheduler and a third opinion cannot improve that.
	return WorkAdmission{Reason: fmt.Sprintf(
		"handoff %s is in flight between two other controller generations", record.ID)}
}

// HandoffAction is what a process starting on a state directory that holds an
// in-flight handoff is permitted to do about it.
type HandoffAction string

const (
	// HandoffActionNone is no handoff to resolve.
	HandoffActionNone HandoffAction = "none"
	// HandoffActionResumePredecessor is a transition that died before the
	// successor became authoritative. The predecessor generation resumes and
	// the record is failed back to it.
	HandoffActionResumePredecessor HandoffAction = "resume_predecessor"
	// HandoffActionContinueSuccessor is a transition that reached ownership
	// but not activation, resumed by the successor generation itself.
	HandoffActionContinueSuccessor HandoffAction = "continue_successor"
	// HandoffActionRepairProjection is authority already established with the
	// pointer possibly stale. Nothing about authority is reconsidered.
	HandoffActionRepairProjection HandoffAction = "repair_projection"
	// HandoffActionRefuse is a record this process is not eligible to touch.
	HandoffActionRefuse HandoffAction = "refuse"
)

// HandoffResolution is the answer to "I am starting up and there is a handoff
// on disk".
type HandoffResolution struct {
	Action HandoffAction      `json:"action"`
	Record *ControllerHandoff `json:"record,omitempty"`
	Detail string             `json:"detail,omitempty"`
}

// ResolveControllerHandoff decides what this process may do about the newest
// unsettled transition.
//
// IT READS NOTHING LIVE. Not the pointer, not another process, not a lock file:
// a crash resolution that consulted a projection would be deriving authority
// from a convenience, and one that consulted a live process would be asking the
// least reliable participant. The durable record and this process's own proven
// identity are the whole input.
func ResolveControllerHandoff(records []ControllerHandoff, self ControllerSelfRecord, controller string) HandoffResolution {
	for _, record := range records {
		if record.Phase == HandoffFailed {
			continue
		}
		predecessor, predecessorErr := record.Predecessor.Binding.Digest()
		successor, successorErr := record.Successor.Binding.Digest()
		if predecessorErr != nil || successorErr != nil {
			return HandoffResolution{Action: HandoffActionRefuse, Record: &record,
				Detail: "the handoff record does not yield controller identities"}
		}
		if record.Phase == HandoffActivated {
			// AUTHORITY IS SETTLED. The only thing left that can be wrong is
			// the projection, and repairing it reconsiders nothing.
			if controller == successor {
				return HandoffResolution{Action: HandoffActionRepairProjection, Record: &record,
					Detail: "the successor is the activated generation; the stable entrypoint may need repair"}
			}
			return HandoffResolution{Action: HandoffActionRefuse, Record: &record,
				Detail: fmt.Sprintf("handoff %s activated another generation; this one does not resume scheduling", record.ID)}
		}
		if !record.MayRecover(controller) {
			return HandoffResolution{Action: HandoffActionRefuse, Record: &record,
				Detail: fmt.Sprintf("handoff %s names another controller as its recovery owner", record.ID)}
		}
		switch controller {
		case predecessor:
			return HandoffResolution{Action: HandoffActionResumePredecessor, Record: &record,
				Detail: fmt.Sprintf("handoff %s stopped at %q before the successor became authoritative", record.ID, record.Phase)}
		case successor:
			if err := self.ProvesGeneration(record.Successor.Binding); err != nil {
				return HandoffResolution{Action: HandoffActionRefuse, Record: &record,
					Detail: "this process is not the successor generation: " + err.Error()}
			}
			return HandoffResolution{Action: HandoffActionContinueSuccessor, Record: &record,
				Detail: fmt.Sprintf("handoff %s reached %q and this process is its successor", record.ID, record.Phase)}
		}
	}
	return HandoffResolution{Action: HandoffActionNone}
}

// ---------------------------------------------------------------------------
// The protocol itself
// ---------------------------------------------------------------------------

// handoffStore is everything the choreography touches durably.
type handoffStore interface {
	runReader
	handoffReader
	ControllerHandoffs() ([]ControllerHandoff, error)
	PutControllerHandoff(handoff ControllerHandoff, expected HandoffPhase) (bool, error)
	ActivateControllerHandoff(handoff ControllerHandoff, expected HandoffPhase) (bool, error)
}

// HandoffPorts are the effects the protocol performs, supplied by whichever
// process is performing its half.
//
// They are ports rather than direct calls for the ordinary reason - the
// predecessor's drain and the successor's acquisition live in different
// processes - and for one specific one: a crash test has to be able to stop
// this protocol between any two durable writes, which means every step must be
// a value the test can fail on demand.
type HandoffPorts struct {
	Store handoffStore
	Self  ControllerSelfRecord
	Now   func() time.Time

	// DrainWorkAdmission stops the predecessor taking on new work. It never
	// cancels: a controller upgrade is not a reason to end anybody's run.
	DrainWorkAdmission func() error
	// ReleaseOwnership gives up the scheduler and the control endpoint.
	ReleaseOwnership func() error
	// AcquireOwnership takes them, and reports whether they were taken.
	AcquireOwnership func() error
	// AdmitSuccession writes one run's succession evidence.
	AdmitSuccession func(runID string, decision ControllerSuccessionDecision) error
	// ProveControlEndpoint answers "does the control endpoint respond as this
	// generation". It is a port because proving it means talking to a socket
	// this package does not own.
	ProveControlEndpoint func() error
	// ControllerRoot holds the immutable generations and the projection.
	ControllerRoot string
}

func (p HandoffPorts) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now()
}

// persist advances the record and writes it under the phase it was read at.
func (p HandoffPorts) persist(record ControllerHandoff, to HandoffPhase) (ControllerHandoff, error) {
	advanced, err := record.Advance(to, p.now())
	if err != nil {
		return record, err
	}
	// THE ACTIVATION COMMIT MOVES BOTH OR NEITHER. Reaching activated is the
	// authority linearization point, and the pointer that says which
	// activation governs is part of that point rather than a write that
	// follows it: a crash between the two would leave a transition claiming to
	// be activated while the pointer named another, with nothing able to say
	// afterwards which was true.
	var wrote bool
	var err2 error
	if to == HandoffActivated {
		wrote, err2 = p.Store.ActivateControllerHandoff(advanced, record.Phase)
	} else {
		wrote, err2 = p.Store.PutControllerHandoff(advanced, record.Phase)
	}
	if err2 != nil {
		return record, err2
	}
	if !wrote {
		// ANOTHER PROCESS MOVED IT. The compare-and-set failing is not a
		// retryable hiccup: it means this process's view of the transition is
		// not the transition, and continuing would be acting on a phase that
		// is no longer true.
		return record, fmt.Errorf("handoff %s was not at phase %q when this process tried to move it to %q",
			record.ID, record.Phase, to)
	}
	return advanced, nil
}

// BeginHandoff is the PREDECESSOR's half: stop taking work, then let go.
//
// It persists the prepared record first, so the transition exists durably
// before any capability is given up. A crash between the write and the drain
// leaves a prepared record the predecessor itself resumes; a crash after the
// release leaves ownership free and the record still naming the predecessor as
// its recovery owner.
func BeginHandoff(ports HandoffPorts, prepared ControllerHandoff) (ControllerHandoff, error) {
	if prepared.Phase != HandoffPrepared {
		return prepared, fmt.Errorf("a handoff begins from %q, not %q", HandoffPrepared, prepared.Phase)
	}
	if !prepared.Compatible() {
		return prepared, fmt.Errorf("handoff %s was not compatible at preflight: %v", prepared.ID, prepared.Blockers())
	}
	wrote, err := ports.Store.PutControllerHandoff(prepared, "")
	if err != nil {
		return prepared, err
	}
	if !wrote {
		// A SETTLED ATTEMPT AT THIS TRANSITION IS RE-ADDRESSED, NOT AVOIDED.
		//
		// The id is deterministic in the two controllers precisely so that a
		// retried transition between the same pair addresses the same record
		// instead of accumulating one per attempt. Without this, the first
		// attempt that failed - a crash resolved at startup, a successor that
		// could not prove itself - would make every later attempt between
		// those two generations refuse for the rest of the record's life,
		// which is the deterministic id defeating its own purpose.
		//
		// It is conditional on FAILED. A record in any live phase belongs to a
		// transition that is still happening, and this refuses exactly as it
		// did before. What is superseded is the settled record's detail; the
		// failure was reported when it happened, in the supervisor report and
		// on the startup banner that settled it.
		settled, found, readErr := ports.Store.ControllerHandoff(prepared.ID)
		if readErr != nil {
			return prepared, readErr
		}
		if !found || settled.Phase != HandoffFailed {
			return prepared, fmt.Errorf("handoff %s is already recorded; resolve it before starting another", prepared.ID)
		}
		if wrote, err = ports.Store.PutControllerHandoff(prepared, HandoffFailed); err != nil {
			return prepared, err
		}
		if !wrote {
			return prepared, fmt.Errorf("handoff %s changed while a new attempt at it was being recorded", prepared.ID)
		}
	}
	if err := ports.DrainWorkAdmission(); err != nil {
		return prepared, fmt.Errorf("the predecessor could not stop admitting work: %w", err)
	}
	draining, err := ports.persist(prepared, HandoffDraining)
	if err != nil {
		return prepared, err
	}
	if err := ports.ReleaseOwnership(); err != nil {
		return draining, fmt.Errorf("the predecessor could not release ownership: %w", err)
	}
	return ports.persist(draining, HandoffOwnershipReleased)
}

// CompleteHandoff is the SUCCESSOR's half, from acquiring ownership to being
// permitted to serve.
//
// Every durable write is conditional on the phase this process read, so two
// successors cannot walk the same transition, and nothing is inferred from the
// projection at any point.
func CompleteHandoff(ports HandoffPorts, id string) (ControllerHandoff, error) {
	record, found, err := ports.Store.ControllerHandoff(id)
	if err != nil {
		return record, err
	}
	if !found {
		return record, fmt.Errorf("no handoff %q is recorded", id)
	}
	if record.Phase != HandoffOwnershipReleased {
		return record, fmt.Errorf("a successor takes over at %q, and handoff %s is at %q",
			HandoffOwnershipReleased, id, record.Phase)
	}

	// OWNERSHIP LINEARIZATION. After this the predecessor may not reacquire
	// implicitly and this process alone may continue the transition - which is
	// permission to perform it, and not yet permission to serve.
	if err := ports.AcquireOwnership(); err != nil {
		return record, fmt.Errorf("the successor could not acquire ownership: %w", err)
	}
	record, err = ports.persist(record, HandoffSuccessorAcquired)
	if err != nil {
		return record, err
	}

	// Everything learned before acquisition was learned while the predecessor
	// could still act. Prove it again, now that nobody else can.
	if err := RevalidateAcquiredHandoff(ports.Store, record, ports.Self); err != nil {
		return ports.failBackToPredecessor(record, err)
	}
	for _, run := range record.Runs {
		if err := ports.AdmitSuccession(run.RunID, run.Decision); err != nil {
			return ports.failBackToPredecessor(record, fmt.Errorf("run %s: %w", run.RunID, err))
		}
	}
	record, err = ports.persist(record, HandoffRevalidated)
	if err != nil {
		return record, err
	}

	if err := ports.Self.ProvesGeneration(record.Successor.Binding); err != nil {
		return record, fmt.Errorf("the successor cannot prove its own generation: %w", err)
	}
	if err := ports.ProveControlEndpoint(); err != nil {
		return record, fmt.Errorf("the successor's control endpoint did not answer: %w", err)
	}

	// AUTHORITY LINEARIZATION. From here the successor is the active
	// generation in the control model, and nothing that happens to the
	// projection changes that.
	record, err = ports.persist(record, HandoffActivated)
	if err != nil {
		return record, err
	}

	// PROJECTION LINEARIZATION. It reflects authority that already exists; a
	// failure here leaves an operator aimed at the previous artifact and the
	// control model entirely correct, which is why it is repair rather than
	// part of the transition.
	if _, err := ActivateControllerGeneration(ports.Store, record.ID, ports.Self, ports.ControllerRoot); err != nil {
		// TYPED, because the caller must be able to tell this apart from a
		// failed transition. The successor IS active; what failed is a
		// projection, and treating the two alike would let a symlink decide
		// whether a controller may serve.
		return record, &ProjectionRepairFailedError{HandoffID: record.ID, Cause: err}
	}
	return record, nil
}

// ProjectionRepairFailedError is an activation that succeeded with a stable
// entrypoint that did not follow. Authority is established; the operator's
// convenience path is stale and repairable.
type ProjectionRepairFailedError struct {
	HandoffID string
	Cause     error
}

func (e *ProjectionRepairFailedError) Error() string {
	return fmt.Sprintf("handoff %s activated and its stable entrypoint needs repair: %v", e.HandoffID, e.Cause)
}

func (e *ProjectionRepairFailedError) Unwrap() error { return e.Cause }

// failBackToPredecessor is the failure law: a successor that acquired
// ownership and could not prove the state it was handed admits nothing,
// schedules nothing, gives ownership back, and records the predecessor as the
// controller permitted to resume.
func (p HandoffPorts) failBackToPredecessor(record ControllerHandoff, cause error) (ControllerHandoff, error) {
	failed, failErr := record.Fail(cause.Error(), record.Predecessor, p.now())
	if failErr != nil {
		return record, fmt.Errorf("%w (and the failure could not be recorded: %v)", cause, failErr)
	}
	if _, writeErr := p.Store.PutControllerHandoff(failed, record.Phase); writeErr != nil {
		return record, fmt.Errorf("%w (and the failure could not be recorded: %v)", cause, writeErr)
	}
	if releaseErr := p.ReleaseOwnership(); releaseErr != nil {
		return failed, fmt.Errorf("%w (and ownership could not be released: %v)", cause, releaseErr)
	}
	return failed, cause
}

// RevalidateAcquiredHandoff is the strong revalidation, performed only after
// ownership is exclusive.
//
// It proves the assumptions the preflight was prepared under are still true:
// both generations are still the ones named, the phase still permits this
// transition, this process is still the successor it claims to be, no
// contradictory activation exists, and the durable run state has not advanced.
// Anything inspected before acquisition was stale by definition.
func RevalidateAcquiredHandoff(store handoffStore, record ControllerHandoff, self ControllerSelfRecord) error {
	stored, found, err := store.ControllerHandoff(record.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("handoff %s is no longer recorded", record.ID)
	}
	if stored.Phase != HandoffSuccessorAcquired {
		return fmt.Errorf("handoff %s is at phase %q, and revalidation runs at %q",
			record.ID, stored.Phase, HandoffSuccessorAcquired)
	}
	current, currentErr := stored.Successor.Binding.Digest()
	expected, expectedErr := record.Successor.Binding.Digest()
	if currentErr != nil || expectedErr != nil || current != expected {
		return fmt.Errorf("handoff %s no longer names the successor it was prepared for", record.ID)
	}
	previous, previousErr := stored.Predecessor.Binding.Digest()
	wasPrevious, wasPreviousErr := record.Predecessor.Binding.Digest()
	if previousErr != nil || wasPreviousErr != nil || previous != wasPrevious {
		return fmt.Errorf("handoff %s no longer names the predecessor it was prepared for", record.ID)
	}
	if err := self.ProvesGeneration(stored.Successor.Binding); err != nil {
		return fmt.Errorf("this process is not the successor the record names: %w", err)
	}
	// THE CURRENT ACTIVATION MUST BE THE ONE THIS TRANSITION SUCCEEDS FROM.
	//
	// The previous form scanned every activated handoff and refused if any of
	// them named a different generation - which is what an OLDER activation is
	// supposed to do. G1->G2 followed by G2->G3 would have been refused on the
	// strength of H1, the very record proving G2 legitimately became active.
	// A second upgrade was therefore impossible, and the check was reading
	// history as contradiction.
	//
	// What actually contradicts this transition is the PRESENT: if the
	// activation that governs now is not the predecessor this transition
	// succeeds from, somebody else activated in between and this one is
	// proceeding from a world that has moved.
	activation, found, err := store.CurrentControllerActivation()
	if err != nil {
		return err
	}
	if found {
		governing, err := activation.Successor.Binding.Digest()
		if err != nil {
			return err
		}
		if governing != previous {
			return fmt.Errorf("transition %s governs now and activated a generation this transition does not succeed from",
				activation.ID)
		}
	}
	// And the runs themselves: same heads, same replay, nothing new that was
	// never classified.
	return RevalidateControllerHandoff(store, stored)
}
