package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// dispositionPayload is what Reduce reads from the run disposition events.
// Only run.failed may carry held material (#203); on any other disposition it
// is refused rather than ignored.
func dispositionPayload(mayHold, mayWaitCapacity bool) payloadValidator {
	return func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var payload dispositionRecord
		if err := strictJSON(raw, &payload); err != nil {
			return err
		}
		if payload.Capacity != nil {
			if !mayWaitCapacity {
				return errors.New("capacity is recorded only on run.waiting")
			}
			if err := payload.Capacity.validate(payload.Reason); err != nil {
				return err
			}
		}
		switch {
		case payload.HeldMaterial == nil:
			return nil
		case !mayHold:
			return errors.New("held_material is recorded only on run.failed")
		}
		return payload.HeldMaterial.validate()
	}
}

// dispositionRecord is the run disposition payload. HeldMaterial is present
// only on a budget-boundary failure that held valuable material (#203); every
// older event, and every other disposition, has none.
type dispositionRecord struct {
	Capacity     *CapacityWait `json:"capacity,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	HeldMaterial *HeldMaterial `json:"held_material,omitempty"`
}

// recordDisposition persists the run's disposition. The event is appended only
// when the disposition, reason, capacity ceiling or producer goal changes.
// An unchanged observation wait does not grow the journal; the run document
// is always refreshed, so a later resume sees current bindings without replaying.
func (r *EngineeringRuntime) recordDisposition(state *runState, disposition Disposition, reason string) error {
	// A run the operator has already STOPPED is never settled onto anything
	// else. Every disposition this pass could record was derived from a
	// snapshot read at the start of the pass, and CancelRun writes from another
	// goroutine entirely - the control endpoint's stop-all runs concurrently
	// with the tick that is driving this run. Recording the stale answer
	// appended run.waiting after run.cancelled and wrote the run document back
	// to waiting, which returned the run to the supervisor's active set and
	// handed the work the operator stopped straight back to the next tick.
	//
	// The re-read is not the guarantee - PutRun's own condition is, and it
	// refuses to replace a cancelled row whatever this pass decided. What the
	// re-read buys is the COMMON case: a stop that has already landed stops
	// this pass from appending a junk run.waiting or run.failed to the hash
	// chain at all, which the write below cannot do anything about because the
	// append comes first. A stop that lands between this read and that write
	// is caught by the condition, and adopted straight afterwards.
	if disposition != Cancelled {
		live, found, err := r.deps.Store.Run(state.run.ID)
		if err != nil {
			return err
		}
		if found && live.Disposition == Cancelled {
			state.run = live
			state.snapshot.Disposition, state.snapshot.Reason = live.Disposition, live.Reason
			return nil
		}
	}
	recordedCapacity, err := capacityWaitFor(state.events, reason)
	if err != nil {
		return err
	}
	capacityChanged := state.capacityWait != nil && (recordedCapacity == nil || *recordedCapacity != *state.capacityWait)
	// A goal marker is current only after the latest engineering operation.
	// Re-observation can discover remediation; an old marker cannot certify it.
	goalChanged := disposition == Waiting && reason == ReasonGoalStateReached &&
		!state.producerStageFinished()
	if state.snapshot.Disposition != disposition || state.snapshot.Reason != reason || capacityChanged || goalChanged {
		eventType, ok := dispositionEvents[disposition]
		if !ok {
			return fmt.Errorf("no journal event for disposition %q", disposition)
		}
		// A budget boundary names what the run is holding (#203) in the SAME
		// event that ends it, so the terminal fact and the held material can
		// never be journalled apart, and replay reads the record back rather
		// than re-deriving it.
		// A record already journalled is reused, never re-derived: a later
		// re-settle carries the SAME identity rather than an opinion of it
		// from moved state. Only run.failed may carry one.
		var held *HeldMaterial
		if disposition == Failed {
			held = state.snapshot.HeldMaterial
			if held == nil && BudgetBoundary(disposition, reason) {
				held = state.heldMaterial(reason)
			}
		}
		payload := dispositionRecord{Reason: reason, HeldMaterial: held, Capacity: state.capacityWait}
		if err := r.append(state, eventType, "", payload, nil); err != nil {
			return err
		}
		state.snapshot.Disposition, state.snapshot.Reason = disposition, reason
		if held != nil {
			state.snapshot.HeldMaterial = held
		}
	}
	run := state.run
	run.Phase = state.phase()
	run.Disposition = disposition
	run.Reason = reason
	run.Base = Ref{ID: r.deps.Repository.DefaultBranch, Revision: state.baseRevision()}
	run.Candidate = Candidate{Branch: candidateBranch(run.ID), Revision: state.projection.CandidateRevision, Tree: state.projection.CandidateTree}
	run.Contract = state.projection.Contract
	run.UpdatedAt = r.deps.Clock.Now()
	state.run = run
	if err := r.deps.Store.PutRun(run); err != nil {
		return err
	}
	// ADOPT WHAT THE ROW ACTUALLY SAYS. The write above is conditional and
	// refuses silently, so a stop that won the race leaves this pass holding a
	// disposition the database never accepted. Nothing durable is wrong at that
	// point - the row and replay both say cancelled, and the acquisition
	// statement reads the row - but the pass would go on to REPORT `waiting`
	// for a run the operator stopped, which is the one thing its caller acts
	// on. One read is cheaper than an operator who believes their stop is still
	// pending.
	if disposition != Cancelled {
		live, found, err := r.deps.Store.Run(run.ID)
		if err != nil {
			return err
		}
		if found && live.Disposition == Cancelled {
			state.run = live
			state.snapshot.Disposition, state.snapshot.Reason = live.Disposition, live.Reason
		}
	}
	return nil
}

var dispositionEvents = map[Disposition]string{
	Waiting:   EventRunWaiting,
	Completed: EventRunCompleted,
	Failed:    EventRunFailed,
	Cancelled: EventRunCancelled,
}

func (r *EngineeringRuntime) settle(state *runState, disposition Disposition, reason string) (Outcome, error) {
	if err := r.recordDisposition(state, disposition, reason); err != nil {
		return Outcome{}, err
	}
	// Reported from what was RECORDED, not from what was asked for: a pass
	// settling on stale state over a stopped run records the stop instead, and
	// the operator's caller has to be told the run is cancelled.
	return Outcome{RunID: state.run.ID, Disposition: state.run.Disposition, Reason: state.run.Reason}, nil
}
