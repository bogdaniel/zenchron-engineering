package runtime

// WHAT A CONTROLLER DOES ABOUT A TRANSITION IT FINDS IN FLIGHT WHEN IT STARTS.
//
// A crash between the predecessor persisting a prepared transition and the
// successor activating it leaves a durable record at prepared, draining or
// ownership_released. Nothing about that is ambiguous - the record names the
// controller permitted to recover, and ResolveControllerHandoff already answers
// what this process may do about it from the record and this process's own
// proven identity alone.
//
// WHAT WAS MISSING WAS THE CALL. serve never asked. The predecessor came back
// up, took the free role, served correctly - and left the record in flight
// forever. The next automatic upgrade between the same two controllers computes
// the same deterministic transition id, finds it already recorded, and refuses:
// safe, diagnosable, and permanently stuck behind its own interrupted attempt.
// That is #288, and it is contrary to #234's requirement that a handoff is
// crash-safe and converges without a person.
//
// THIS FILE ADDS NO AUTHORITY SEMANTICS. It calls the resolver, and on the one
// answer that means "this process is the controller of record and the
// transition never happened" it settles the record with the same Fail the
// protocol uses everywhere else, naming the predecessor as recovery owner -
// which is what the record already said. Every other answer is reported and
// acted on by nothing: a transition whose recovery owner is another generation
// is not this process's to settle, and an activated one is authority that is
// already established.

import (
	"fmt"
	"time"
)

// StartupResolution is what a starting controller found and did.
type StartupResolution struct {
	Resolution HandoffResolution `json:"resolution"`
	// Settled reports that an interrupted transition was closed, so a later
	// attempt at the same transition is not refused for already existing.
	Settled bool `json:"settled"`
}

// Describe renders one startup resolution for the banner.
func (r StartupResolution) Describe() string {
	switch {
	case r.Resolution.Action == HandoffActionNone:
		return "none in flight"
	case r.Settled:
		return fmt.Sprintf("settled interrupted transition %s (%s)", r.Resolution.Record.ID, r.Resolution.Detail)
	case r.Resolution.Record != nil:
		return fmt.Sprintf("%s: transition %s (%s)", r.Resolution.Action, r.Resolution.Record.ID, r.Resolution.Detail)
	default:
		return string(r.Resolution.Action)
	}
}

// ResolveHandoffAtStartup settles an interrupted transition this controller is
// the recovery owner of.
//
// The write is conditional on the phase that was read, so a successor that is
// mid-transition at this exact moment wins and this process's settle is
// refused rather than racing it. That is the same compare-and-set every other
// durable step in this protocol uses, for the same reason.
func ResolveHandoffAtStartup(store handoffStore, self ControllerSelfRecord, controller string, now time.Time) (StartupResolution, error) {
	records, err := store.ControllerHandoffs()
	if err != nil {
		return StartupResolution{}, err
	}
	resolution := ResolveControllerHandoff(records, self, controller)
	if resolution.Action != HandoffActionResumePredecessor || resolution.Record == nil {
		// none            nothing to do
		// continue_successor  a successor's own resumption, which this process
		//                 is not and must not perform on its behalf
		// repair_projection   authority already settled; the reconciler repairs
		//                 the pointer on its own pass
		// refuse          another generation's record
		return StartupResolution{Resolution: resolution}, nil
	}
	record := *resolution.Record
	// THE RECOVERY OWNER IS THE ONE THE RECORD ALREADY NAMES. Nothing here
	// chooses it: the predecessor is where an interrupted transition returns
	// to, because before activation it has admitted nothing and scheduled
	// nothing, and the state is exactly what it left.
	settled, err := record.Fail(fmt.Sprintf(
		"the transition was interrupted at %q and the predecessor restarted; it is settled so a later attempt is not refused for already existing",
		record.Phase), record.Predecessor, now)
	if err != nil {
		return StartupResolution{Resolution: resolution}, err
	}
	wrote, err := store.PutControllerHandoff(settled, record.Phase)
	if err != nil {
		return StartupResolution{Resolution: resolution}, err
	}
	if !wrote {
		// Somebody moved it between the read and the write. The record is not
		// what this process resolved against, so it settles nothing and says
		// so; the next start resolves the state as it then is.
		return StartupResolution{Resolution: HandoffResolution{
			Action: HandoffActionRefuse, Record: &record,
			Detail: fmt.Sprintf("transition %s moved while it was being settled", record.ID),
		}}, nil
	}
	resolution.Record = &settled
	return StartupResolution{Resolution: resolution, Settled: true}, nil
}
