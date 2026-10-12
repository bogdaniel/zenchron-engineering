package orchestration

// Remediation epoch primitives (#581, ADR-0008 Batch 1: additive foundation
// only). These are pure projections, exactly like ItemState (item.go) - never
// stored, always recomputed from durable facts already owned elsewhere (a
// bound run's own ItemState/RunTermination/Output, and whichever
// authorization/decision rows exist). Nothing here is wired into
// ProjectWorkGraph yet; that wiring, and the atomic epoch+run+operations
// admission transaction ADR-0008 §8 specifies, is #581 Batch 2.

// EpochOutcome is one remediation epoch's derived outcome. It is never
// stored: an epoch's own durable row (runtime.RemediationEpoch) carries only
// how it was authorized and activated, never how it went. "How it went" is
// always read off the bound run's own, already-authoritative lifecycle
// (ItemState/RunTermination), through DeriveEpochOutcome below - the same
// discipline ItemState itself already applies to an ordinary run.
type EpochOutcome string

const (
	// EpochRunning is a bound run still live (RunLive).
	EpochRunning EpochOutcome = "running"
	// EpochCompletedAdmitted is a terminal, completed run that produced an
	// admitted handoff/Output. This is the only outcome that can ever become
	// a unit's LatestAdmittedHandoffEpoch.
	EpochCompletedAdmitted EpochOutcome = "completed_admitted"
	// EpochCompletedNoOutput is a terminal, completed run with no admitted
	// Output (e.g. a conflict that was never resolved within this attempt).
	EpochCompletedNoOutput EpochOutcome = "completed_no_output"
	// EpochFailed is a terminal run whose own assurance or execution failed.
	EpochFailed EpochOutcome = "failed"
	// EpochCancelled is a run the operator or an existing timeout mechanism
	// cancelled before it reached a terminal outcome of its own.
	EpochCancelled EpochOutcome = "cancelled"
)

// DeriveEpochOutcome computes one epoch's outcome purely from its bound run's
// own existing, already-authoritative facts. termination and hasAdmittedOutput
// come from the run's own journal/handoff table (runtime), never from a
// second, separately-mutated status column - there is no such column (ADR-0008
// §4.1).
func DeriveEpochOutcome(termination RunTermination, hasAdmittedOutput bool) EpochOutcome {
	switch termination {
	case RunCancelled:
		return EpochCancelled
	case RunFailed:
		return EpochFailed
	case RunCompleted:
		if hasAdmittedOutput {
			return EpochCompletedAdmitted
		}
		return EpochCompletedNoOutput
	default: // RunLive, and any future addition: never silently terminal
		return EpochRunning
	}
}

// RemediationState is one WorkUnit's derived remediation posture. It is
// never stored (ADR-0008 §4.2): every value is recomputed from the unit's
// epoch rows, their bound runs' outcomes, any live decision_request asking
// to authorize the next epoch, and whether the triggering Finding was
// explicitly dismissed.
type RemediationState string

const (
	// RemediationNone: no Finding has ever produced a live remediation
	// request against this unit's current admitted epoch, or the only one
	// that ever did has been dismissed with nothing new pending.
	RemediationNone RemediationState = "none"
	// RemediationPendingDecision: a live decision_request(epoch_authorization)
	// is outstanding for this unit.
	RemediationPendingDecision RemediationState = "pending_decision"
	// RemediationRunning: the unit's most recent attempt epoch is still live.
	RemediationRunning RemediationState = "running"
	// RemediationExhaustedUnresolved: the most recent attempt did not
	// succeed, no decision is pending, no further epoch can be granted under
	// the unit's effective ceiling, and the triggering Finding is still
	// live (not dismissed). The unit's original conflicting output stays
	// unusable to downstream dependents until a human dismisses the Finding
	// or explicitly raises the ceiling.
	RemediationExhaustedUnresolved RemediationState = "exhausted_unresolved"
	// RemediationResolved: either the most recent attempt succeeded
	// (produced a new EpochCompletedAdmitted epoch) with no live Finding
	// against it, or the live Finding was explicitly dismissed.
	RemediationResolved RemediationState = "resolved"
)

// DeriveRemediationState computes a unit's RemediationState purely from
// already-durable facts (ADR-0008 §4.2). It takes no store/DB dependency;
// every input is a fact its caller (runtime) already read elsewhere.
//
//   - latestAttemptOutcome is EpochRunning/EpochCompletedAdmitted/... for the
//     unit's highest-numbered epoch (1 if it has never remediated), or ""
//     if the unit has not even completed its original epoch 1 yet (in which
//     case this function is not meaningful and the caller should not call it).
//   - latestAttemptSucceeded reports whether that SAME epoch is also the
//     unit's LatestAdmittedHandoffEpoch (i.e. the most recent attempt is the
//     one that is current, not a stale admitted epoch from before a later,
//     failed attempt).
//   - hasLivePendingDecision reports a live decision_request(epoch_authorization)
//     for this unit's next epoch.
//   - triggeringFindingDismissed reports whether the Finding that most
//     recently justified a remediation request against this unit's current
//     admitted epoch was resolved with Outcome: dismiss. A unit that has
//     never had a live Finding against its current admitted epoch at all
//     should pass dismissed=true (there is nothing to resolve, which is
//     indistinguishable from RemediationNone for this function's purposes).
//   - effectiveCeilingReached reports whether EffectiveCeiling(unit) <=
//     latestAttemptEpoch, i.e. no further epoch can be granted right now.
func DeriveRemediationState(
	latestAttemptOutcome EpochOutcome,
	latestAttemptSucceeded bool,
	hasLivePendingDecision bool,
	triggeringFindingDismissed bool,
	effectiveCeilingReached bool,
) RemediationState {
	if triggeringFindingDismissed {
		return RemediationResolved
	}
	if latestAttemptOutcome == EpochCompletedAdmitted && latestAttemptSucceeded {
		return RemediationResolved
	}
	if latestAttemptOutcome == EpochRunning {
		return RemediationRunning
	}
	if hasLivePendingDecision {
		return RemediationPendingDecision
	}
	if effectiveCeilingReached {
		return RemediationExhaustedUnresolved
	}
	return RemediationPendingDecision
}

// DownstreamDependencySatisfaction is the ONLY fact a dependent unit's own
// digest/satisfaction computation may read for an upstream unit (ADR-0008
// §4.2, §10). currentEffectiveOutput is the historical last-admitted handoff
// (runtime.RemediationEpoch-derived; nil if the unit has never admitted one).
// It is deliberately NOT returned while remediation is pending, running, or
// exhausted-unresolved: a failed or outstanding remediation attempt must
// never make an old, disputed handoff look current to a dependent (the exact
// defect ADR-0008's second review pass fixed).
func DownstreamDependencySatisfaction(state RemediationState, currentEffectiveOutput *UnitOutput) *UnitOutput {
	switch state {
	case RemediationNone, RemediationResolved:
		return currentEffectiveOutput
	default:
		return nil
	}
}
