package runtime

// WHAT A WATCHER IS ALLOWED TO CONCLUDE, and nothing it is allowed to do.
//
// This file is a pure function from an observation to an INTENT. It mutates
// nothing, holds nothing, and reaches nothing: the executor that carries an
// intent out lives elsewhere, and it may only invoke operations whose authority
// semantics were proven before it existed.
//
// THE WATCHER HAS NO AUTHORITY SEMANTICS OF ITS OWN. It observes, classifies,
// and asks for an existing operation. Unknown facts, unstable observations,
// collection failures and invariant violations all produce NO autonomous
// mutation. That last category is the one most likely to be argued with, so it
// is written down: given
//
//	durable active     G2
//	observed role      G1
//	observed admission G1
//
// the watcher must NOT conclude that G2 obviously wins and repair G1. Deciding
// which of two claimants is right is an authority decision, and a component
// that makes it has stopped reconciling and started governing. It refuses and
// leaves the evidence intact for a human.
//
// IT NEVER INFERS DEATH, ABSENCE OR FREEDOM. An unreachable endpoint is not a
// dead process; a dead process is not a free role; a reachable socket is not an
// owner. Those three inferences are exactly the ones the stack below spent
// itself removing, and re-deriving any of them here would put them back one
// layer up.
//
// WHAT "ELIGIBLE FOR RECOVERY" MEANS is deliberately narrow: this process can
// SEE ITSELF being the activated generation and not serving. That is a
// self-observation rather than a judgement about somebody else. Whether it may
// actually recover remains the runtime's decision, made against the capability
// and the durable record at the moment of the call - the watcher asks, and is
// refused if it was wrong.
//
// THE REVIEW TRIPWIRE, recorded where it will be read: if this file ever needs
// to determine who should be active, who owns the role, whether an unreachable
// process is dead, whether an admission should exist, or whether a transition
// should activate, then it has crossed from reconciliation into authority and
// something below it is missing.

import "fmt"

// ReconciliationAction is what the watcher believes should be attempted.
type ReconciliationAction string

const (
	// ReconcileNone is a state that needs nothing. It is also the answer for
	// every state the watcher cannot safely classify, which is why the
	// distinction between "nothing to do" and "not enough known" lives in the
	// reason rather than in the power of the action.
	ReconcileNone ReconciliationAction = "none"
	// ReconcileUnknown is insufficient evidence. No mutation.
	ReconcileUnknown ReconciliationAction = "unknown"
	// ReconcileRefuse is an invariant violation: two authorities where there
	// may be one. No mutation, and deliberately not a repair - choosing the
	// winner would be the authority decision this component may not make.
	ReconcileRefuse ReconciliationAction = "refuse"
	// ReconcileRepairProjection asks for the existing projection repair. The
	// target is never constructed here; it is read from the durable record.
	ReconcileRepairProjection ReconciliationAction = "repair_projection"
	// ReconcileResumeActivatedService asks for service to resume on a
	// controller that is already durably activated and is not serving.
	//
	// It names the CONVERGENCE rather than one of the operations that reaches
	// it, because RecoverActivated alone does not converge: it resumes the
	// activation and deliberately leaves work admission closed, so an intent
	// named after it would be satisfied by a call that changes nothing the
	// classifier can observe, and would be emitted again on every pass.
	// Composing the two already-authorized operations is the executor's job;
	// naming the goal is this file's.
	ReconcileResumeActivatedService ReconciliationAction = "resume_activated_service"
)

// Mutating reports whether an action asks for a change at all. It exists so a
// test can state the metamorphic law - degrading a fact to unknown may remove
// an action and may never add one - without enumerating actions.
func (a ReconciliationAction) Mutating() bool {
	return a == ReconcileRepairProjection || a == ReconcileResumeActivatedService
}

// Power orders actions by how much they attempt, so the law above is
// checkable: replacing a known fact with an unknown one must never raise it.
func (a ReconciliationAction) Power() int {
	switch a {
	case ReconcileRepairProjection:
		return 1
	case ReconcileResumeActivatedService:
		return 2
	default:
		return 0
	}
}

// ReconciliationIntent is the whole output: what to attempt, why, and against
// which exact subject.
//
// The subject is copied from durable truth rather than assembled, because a
// watcher that computed its own target would be deciding what the right answer
// is instead of asking for the recorded one.
type ReconciliationIntent struct {
	Action ReconciliationAction `json:"action"`
	Reason string               `json:"reason"`
	// HandoffID is the transition the action concerns, from the record.
	HandoffID string `json:"handoff_id,omitempty"`
	// Generation is the durably active generation, from the record.
	Generation *ControllerBuild `json:"generation,omitempty"`
}

// WatcherObservation is what the watcher classifies from: the operator-shaped
// status, and THIS PROCESS's own identity.
//
// They are separate fields because they answer different questions and the
// difference is the one #274 and #275 exist to protect. ControllerStatus's live
// snapshot is collected through a transport the caller supplies, so it proves
// that the process AT THAT ENDPOINT says it is some generation - not that the
// watcher is. A classifier that read the endpoint's identity as its own would
// be making precisely the substitution those slices removed, and would be
// correct today only because RecoverActivated re-checks identity at the moment
// of the call.
//
// So the watcher supplies its own identity as a fact, and recovery requires all
// three to agree: this process is the durably active generation, the endpoint
// observed is this same generation, and it is not admitting work.
type WatcherObservation struct {
	Status ControllerStatus     `json:"status"`
	Self   ControllerSelfRecord `json:"self"`
}

// ClassifyReconciliation turns one observation into one intent.
//
// It is deterministic in its input and has no other inputs: the same
// WatcherObservation always yields the same intent, which is what makes the
// decision table reviewable before any executor exists.
func ClassifyReconciliation(observation WatcherObservation) ReconciliationIntent {
	status := observation.Status
	// 1. THE OBSERVATION ITSELF must be trustworthy before anything it
	// contains is acted on.
	switch status.DurableConsistency {
	case DurableConsistent:
		// The only verdict from which anything may be attempted.
	case DurableUnstable:
		return ReconciliationIntent{Action: ReconcileNone,
			Reason: "the durable transition changed while the status was collected; a torn observation is not evidence"}
	case DurableViolation:
		return ReconciliationIntent{Action: ReconcileRefuse, HandoffID: status.Durable.HandoffID,
			Generation: status.Durable.Generation,
			Reason:     "two authorities where there may be one; choosing between them is not this component's decision"}
	case DurableUnrecorded:
		return ReconciliationIntent{Action: ReconcileNone,
			Reason: "no activation is recorded, so there is no durable target to reconcile toward"}
	default:
		// A VERDICT THIS BUILD DOES NOT RECOGNISE. The zero value and any
		// future member land here, and both mean the same thing: something
		// said something this code cannot read. Falling through as though it
		// were "consistent" would let an unrecognised value authorize a
		// mutation, which is the rule this file states about itself.
		return ReconciliationIntent{Action: ReconcileUnknown,
			Reason: fmt.Sprintf("durable consistency %q is not a verdict this controller recognises", status.DurableConsistency)}
	}
	if status.Durable.Generation == nil {
		return ReconciliationIntent{Action: ReconcileUnknown,
			Reason: "the durable record names no active generation"}
	}

	// 2. RECOVERY, and only as a SELF-observation. This process can see that
	// it is the activated generation and is not serving. It never concludes
	// that somebody else is dead, and an unreachable endpoint yields nothing
	// at all - not death, not absence, not a free role.
	live := status.Live.Snapshot
	selfIsActive := !observation.Self.Unattested && observation.Self.Build == *status.Durable.Generation
	observedIsSelf := status.Live.Reachable && live != nil && live.Identity.Build == observation.Self.Build
	if selfIsActive && observedIsSelf {
		switch live.WorkAdmission {
		case AdmissionOpen:
			// Serving. Only the projection could still be wrong.
		case AdmissionUnknown:
			return ReconciliationIntent{Action: ReconcileUnknown, HandoffID: status.Durable.HandoffID,
				Generation: status.Durable.Generation,
				Reason:     "the observed controller did not report whether it is admitting work"}
		case AdmissionWithheld, AdmissionClosed:
			return ReconciliationIntent{
				Action: ReconcileResumeActivatedService, HandoffID: status.Durable.HandoffID,
				Generation: status.Durable.Generation,
				Reason: fmt.Sprintf(
					"this controller is the activated generation for %s and is not admitting work",
					status.Durable.HandoffID),
			}
		default:
			// Unknown, and any state a future build might report. An
			// unrecognised gate must never acquire recovery semantics by
			// being the default arm of a switch.
			return ReconciliationIntent{Action: ReconcileUnknown, HandoffID: status.Durable.HandoffID,
				Generation: status.Durable.Generation,
				Reason: fmt.Sprintf("work admission %q is not a state this controller recognises",
					live.WorkAdmission)}
		}
	}

	// 3. THE PROJECTION, which is the one thing that may be repaired from
	// durable truth alone: the record says where the active artifact is, and a
	// pointer that names something else is stale rather than authoritative.
	switch status.Projection.State {
	case ProjectionDrift, ProjectionMissing, ProjectionInvalid:
		return ReconciliationIntent{
			Action: ReconcileRepairProjection, HandoffID: status.Durable.HandoffID,
			Generation: status.Durable.Generation,
			Reason: fmt.Sprintf("the stable entrypoint is %s while %s is durably active",
				status.Projection.State, status.Durable.Generation.Version),
		}
	}

	// 4. NOTHING KNOWN TO BE WRONG. An unobservable controller lands here too:
	// not seeing something is not a reason to change it.
	if !status.Live.Reachable || status.Live.Snapshot == nil {
		return ReconciliationIntent{Action: ReconcileNone, HandoffID: status.Durable.HandoffID,
			Generation: status.Durable.Generation,
			Reason:     "no live controller was observed, and an unobservable controller is not an actionable one"}
	}
	return ReconciliationIntent{Action: ReconcileNone, HandoffID: status.Durable.HandoffID,
		Generation: status.Durable.Generation,
		Reason:     "durable state, the live controller and the stable entrypoint agree"}
}
