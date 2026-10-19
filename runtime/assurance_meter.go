package runtime

import "time"

// The run's assurance work meter (#640).
//
// Verification of a candidate is real, charged work, but it is a different
// resource from execution and remediation: a repository whose full suite runs
// for 43 minutes must not have a 30 minute execution allowance spent by a
// SUCCESSFUL verification. Total active work stays one number (activeElapsed,
// which the plan's aggregate ceiling also reads); this file splits it into the
// assurance share, bounded by RunBudgets.AssuranceWallLimit, and the rest,
// bounded by RunBudgets.WallLimit.

func isAssuranceKind(kind string) bool { return kind == OpAssuranceGo || kind == OpAssuranceSemantic }

// assuranceElapsed is the time spent inside assurance operations, derived from
// the journal so a restart, a crash or a rerun reaches the same number. An
// attempt that never finished is charged up to its successor's start (or to
// now), never refunded. Verification-capacity waits inside an operation are
// already excluded from total active work, so they are excluded here too, or
// the split would subtract them twice.
//
// ponytail: refolded per call (a run has a handful of assurance intervals);
// memoize like externalWait if conditions() ever shows up in a profile.
func (s *runState) assuranceElapsed(now time.Time) time.Duration {
	var total time.Duration
	charge := func(id string, from, to time.Time) {
		if to.After(from) {
			total += max(0, to.Sub(from)-verificationWait(s.events, id, from, to))
		}
	}
	inFlight := map[string]time.Time{}
	for _, event := range s.events {
		switch event.Type {
		case EventOperationBefore:
			var op RunOperation
			if decodeJSON(event.Payload, &op) != nil || !isAssuranceKind(op.Kind) {
				continue
			}
			if from, ok := inFlight[event.OperationID]; ok {
				charge(event.OperationID, from, event.OccurredAt)
			}
			inFlight[event.OperationID] = event.OccurredAt
		case EventOperationAfter:
			if from, ok := inFlight[event.OperationID]; ok {
				charge(event.OperationID, from, event.OccurredAt)
				delete(inFlight, event.OperationID)
			}
		case EventRunCompleted, EventRunFailed, EventRunCancelled:
			for id, from := range inFlight {
				charge(id, from, event.OccurredAt)
			}
			clear(inFlight)
		}
	}
	for id, from := range inFlight {
		charge(id, from, now)
	}
	return total
}

// executionElapsed is the active work WallLimit judges: total active work less
// the assurance share. A run frozen before #640 has no assurance ceiling and
// keeps the legacy rule exactly - assurance is charged to WallLimit.
func (s *runState) executionElapsed(now time.Time) time.Duration {
	total := s.activeElapsed(now)
	if s.budgets().AssuranceWallLimit <= 0 {
		return total
	}
	return max(0, total-s.assuranceElapsed(now))
}

// assuranceRemaining is the run's remaining assurance allowance; bounded is
// false for a run with no assurance ceiling (the legacy rule).
func (s *runState) assuranceRemaining(now time.Time) (remaining time.Duration, bounded bool) {
	limit := s.budgets().AssuranceWallLimit
	if limit <= 0 {
		return 0, false
	}
	return max(limit-s.assuranceElapsed(now), 0), true
}

// operationWallBudget is the budget an operation of kind is planned against:
// assurance operations draw on the assurance ceiling, everything else on the
// execution one.
func (s *runState) operationWallBudget(kind string) time.Duration {
	if limit := s.budgets().AssuranceWallLimit; limit > 0 && isAssuranceKind(kind) {
		return limit
	}
	return s.budgets().WallLimit
}

// operationLimit is the authority one physical attempt of kind receives. An
// assurance attempt is bounded by what remains of the assurance allowance, so
// a retry or a restart never starts with a fresh one.
func (s *runState) operationLimit(kind string, now time.Time) *AttemptLimit {
	if remaining, bounded := s.assuranceRemaining(now); bounded && isAssuranceKind(kind) {
		return &AttemptLimit{Within: remaining, Bound: BoundAssuranceWork}
	}
	return s.attemptLimit(now)
}
