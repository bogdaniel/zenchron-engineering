package runtime

// THE CONTROLLER SERVICE: the one place the four transitions are sequenced, and
// the reason they cannot be performed in another order.
//
//	role acquisition      permits choreography, and nothing else
//	durable activation    establishes which generation is true
//	work admission        establishes live service
//	role release          requires service already drained
//
// Each of those is a correct primitive on its own, and every one of them has a
// way to be combined wrongly. The dangerous combination is not hypothetical:
//
//	A: admission open, releases the role
//	B: acquires the role, opens its admission
//	A: still admitting
//
// Two serving controllers, with a role lock that behaved perfectly throughout.
// Nothing in the lock or the gate is wrong there; what is missing is the rule
// that a process may not give up the role while it is still serving. So the
// release is not exposed on its own: DrainAndReleaseRole closes admission,
// waits for intake already in progress, and only then lets the role go.
//
// ACTIVATION AND SERVICE ARE DIFFERENT EVENTS, and the gap between them is a
// legitimate state rather than a failure. A controller can be durably active
// and not serving - it just activated and has not opened admission, or it
// activated and died - and the recovery path exists precisely because that
// state has to be resumable. If activation implied service, a crash in the gap
// would strand the controller permanently.
//
// TWO SUCCESSOR PATHS, and only one of them decides anything:
//
//	transition   the record is at successor_acquired and names this process as
//	             the successor; revalidate, admit, activate.
//	recovery     the record ALREADY says activated and already names this
//	             generation; prove it and resume. Nothing is re-decided,
//	             because the decision is on disk.

import (
	"fmt"
	"sync"
	"time"
)

// workAdmissionController is the serving side's admission gate, as the service
// needs to see it. It is an interface so the service sequences the supervisor's
// one gate rather than owning a second one: two gates would be two answers to
// "may this controller serve".
type workAdmissionController interface {
	EnableWorkAdmission(WorkAdmission) error
	AdmittingWork() bool
	Drain()
}

// ControllerService owns the role lease for a controller's serving lifetime and
// mediates every transition that involves it.
type ControllerService struct {
	// mu serializes the transitions against each other. The lease and the gate
	// have their own boundaries for their own races; this one stops two
	// transitions from interleaving - an activation and a drain, say - and
	// leaving the pair in a state neither of them describes.
	mu             sync.Mutex
	stateDir       string
	controllerRoot string
	store          handoffStore
	self           ControllerSelfRecord
	lease          *ControllerRoleLease
	admission      workAdmissionController
	// now stamps durable records. It is a field so a test pins the times it
	// writes rather than asserting against whatever the wall clock said.
	now func() time.Time
}

// StartControllerService takes the controller role and returns a service that
// is NOT yet serving.
//
// Acquiring the role opens nothing. That is the whole point of the separation:
// a successor holds the role while it revalidates, proves itself and activates,
// and any of those steps may refuse.
func StartControllerService(stateDir, controllerRoot string, store handoffStore, self ControllerSelfRecord, admission workAdmissionController) (*ControllerService, error) {
	lease, err := AcquireControllerRole(stateDir)
	if err != nil {
		return nil, err
	}
	return &ControllerService{
		stateDir: stateDir, controllerRoot: controllerRoot,
		store: store, self: self, lease: lease, admission: admission,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ActivationExpectation is the EXACT transition an activation was authorized
// against.
//
// Activation does not say "activate this generation". It says "activate this
// generation for this transition, out of this phase, between these two
// controllers" - and if any of it has moved, it refuses rather than re-reading
// and making the best of it. The caller's authority was established against one
// transition; a different one is a different question that nobody answered.
type ActivationExpectation struct {
	HandoffID   string
	Phase       HandoffPhase
	Predecessor string
	Successor   string
}

// Expect builds the expectation from a record the caller revalidated.
func Expect(record ControllerHandoff) (ActivationExpectation, error) {
	predecessor, err := record.Predecessor.Binding.Digest()
	if err != nil {
		return ActivationExpectation{}, err
	}
	successor, err := record.Successor.Binding.Digest()
	if err != nil {
		return ActivationExpectation{}, err
	}
	return ActivationExpectation{
		HandoffID: record.ID, Phase: record.Phase,
		Predecessor: predecessor, Successor: successor,
	}, nil
}

// matches compares a stored record against what the caller was authorized for.
func (e ActivationExpectation) matches(record ControllerHandoff) error {
	if record.ID != e.HandoffID {
		return fmt.Errorf("the record is transition %s and the caller was authorized for %s", record.ID, e.HandoffID)
	}
	if record.Phase != e.Phase {
		return fmt.Errorf("transition %s is at phase %q and the caller was authorized at %q", record.ID, record.Phase, e.Phase)
	}
	predecessor, err := record.Predecessor.Binding.Digest()
	if err != nil {
		return err
	}
	successor, err := record.Successor.Binding.Digest()
	if err != nil {
		return err
	}
	if predecessor != e.Predecessor || successor != e.Successor {
		return fmt.Errorf("transition %s no longer names the controllers the caller was authorized for", record.ID)
	}
	return nil
}

// ActivateSuccessor runs the successor's half of a transition: revalidate under
// exclusive ownership, write the succession admissions, prove this generation,
// and commit the durable activation.
//
// IT LEAVES ADMISSION CLOSED. Activation establishes which generation is true
// and says nothing about whether anyone is serving; opening admission is a
// separate, separately authorized step. A test pins that the gate is still shut
// when this returns successfully.
//
// A projection that could not be repaired is reported and does not undo
// anything: the successor is active, and an operator's stable path is stale.
func (s *ControllerService) ActivateSuccessor(expect ActivationExpectation, proveEndpoint func() error) (ControllerHandoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, found, err := s.store.ControllerHandoff(expect.HandoffID)
	if err != nil {
		return ControllerHandoff{}, err
	}
	if !found {
		return ControllerHandoff{}, fmt.Errorf("no transition %q is recorded", expect.HandoffID)
	}
	if err := expect.matches(stored); err != nil {
		return stored, fmt.Errorf("the activation was authorized against a transition that has moved: %w", err)
	}
	if err := s.self.ProvesGeneration(stored.Successor.Binding); err != nil {
		return stored, fmt.Errorf("this process is not the successor the transition names: %w", err)
	}
	if proveEndpoint == nil {
		proveEndpoint = func() error { return nil }
	}

	ports := HandoffPorts{
		Store: s.store, Self: s.self, ControllerRoot: s.controllerRoot, Now: s.now,
		// THE ROLE IS NOT REACQUIRED. This process already holds it and has
		// held it since it started; taking it again - or releasing and
		// retaking it - would open a window for another process to intervene
		// in the middle of a transition. The port verifies rather than
		// acquires.
		AcquireOwnership: func() error {
			return s.lease.WithAuthority(func() error { return nil })
		},
		ReleaseOwnership: func() error { return s.lease.Release() },
		AdmitSuccession: func(runID string, decision ControllerSuccessionDecision) error {
			return s.admitSuccession(runID, decision)
		},
		ProveControlEndpoint: proveEndpoint,
	}
	// The whole commit runs under the role, so a release arriving mid-transition
	// waits for it and one arriving after is refused.
	var activated ControllerHandoff
	var completion error
	if err := s.lease.WithAuthority(func() error {
		activated, completion = CompleteHandoff(ports, expect.HandoffID)
		return nil
	}); err != nil {
		return stored, err
	}
	return activated, completion
}

// admitSuccession writes one run's succession evidence through a runtime bound
// to this process's store.
func (s *ControllerService) admitSuccession(runID string, decision ControllerSuccessionDecision) error {
	writer, ok := s.store.(*SQLiteOperationStore)
	if !ok {
		return fmt.Errorf("this controller cannot write succession evidence")
	}
	return admitControllerSuccession(writer, runID, decision, s.now())
}

// RecoverActivated resumes a transition whose activation is ALREADY durable.
//
// It decides nothing. The record says this generation is active, this process
// proves it is that generation, and what remains is to repair the projection
// and be ready to serve. A crash between the activation commit and everything
// after it is therefore an ordinary resumable state rather than a stranded
// controller.
func (s *ControllerService) RecoverActivated(handoffID string) (ControllerHandoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, found, err := s.store.ControllerHandoff(handoffID)
	if err != nil {
		return ControllerHandoff{}, err
	}
	if !found {
		return ControllerHandoff{}, fmt.Errorf("no transition %q is recorded", handoffID)
	}
	if record.Phase != HandoffActivated {
		return record, fmt.Errorf("transition %s is at phase %q; recovery resumes an activation that already happened", handoffID, record.Phase)
	}
	if err := s.self.ProvesGeneration(record.Successor.Binding); err != nil {
		return record, fmt.Errorf("this process is not the activated generation: %w", err)
	}
	if err := s.lease.WithAuthority(func() error {
		_, repairErr := ActivateControllerGeneration(s.store, record.ID, s.self, s.controllerRoot)
		if repairErr != nil {
			return &ProjectionRepairFailedError{HandoffID: record.ID, Cause: repairErr}
		}
		return nil
	}); err != nil {
		return record, err
	}
	return record, nil
}

// EnableWorkAdmission opens service, and only on the conjunction.
//
// It requires the live role, a durable record that names this process's
// generation as active, and this process proving it IS that generation - all
// established INSIDE the authority section, so the checks cannot be made and
// then acted on after the role has gone.
func (s *ControllerService) EnableWorkAdmission(handoffID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admission == nil {
		return fmt.Errorf("this controller has no work-admission gate to open")
	}
	return s.lease.WithAuthority(func() error {
		record, found, err := s.store.ControllerHandoff(handoffID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no transition %q is recorded", handoffID)
		}
		if record.Phase != HandoffActivated {
			return &WorkAdmissionRefusedError{Reason: fmt.Sprintf(
				"transition %s is at phase %q; service follows the durable activation", handoffID, record.Phase)}
		}
		successor, err := record.Successor.Binding.Digest()
		if err != nil {
			return err
		}
		return s.admission.EnableWorkAdmission(WorkAdmissionFor(&record, s.self, successor))
	})
}

// DrainAndReleaseRole is the ONLY way this service gives up the role.
//
// Closing admission first is not a convention the caller has to remember: a
// process that released the role while still admitting would let its successor
// start serving beside it, which is two live controllers reached through two
// individually correct primitives. The drain also WAITS for intake already in
// progress, so nothing is still being admitted when the role changes hands.
func (s *ControllerService) DrainAndReleaseRole() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admission != nil {
		s.admission.Drain()
		if s.admission.AdmittingWork() {
			return fmt.Errorf("work admission did not close, so the controller role is not being released")
		}
	}
	return s.lease.Release()
}

// AdmittingWork reports whether this controller is serving.
func (s *ControllerService) AdmittingWork() bool {
	return s.admission != nil && s.admission.AdmittingWork()
}

// RoleLease exposes the capability for the control endpoint to serve under. It
// is the one place the lease leaves this type, and it leaves as the capability
// itself rather than as a claim about it.
func (s *ControllerService) RoleLease() *ControllerRoleLease { return s.lease }
