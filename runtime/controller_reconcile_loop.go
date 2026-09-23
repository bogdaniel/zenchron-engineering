package runtime

// ONE RECONCILIATION ATTEMPT PER SUPERVISOR PASS, and no second loop.
//
// The supervisor already owns everything a maintenance loop needs: a poll
// interval, cancellation, drain and shutdown behaviour, a report, and the
// guarantee that it stops when serve stops. A goroutine with its own timer
// would be a second lifecycle to get right, and the first thing it would get
// wrong is dying at a different time from the thing it maintains.
//
// IT DECIDES NOTHING. Observe, classify, execute, report - and every decision
// inside that sequence was made and proven below. What this file adds is
// timing, and timing is deliberately the only thing it adds.
//
// BACKOFF CONTROLS WHEN TO ASK AGAIN, NEVER WHETHER AN ACTION IS AUTHORIZED.
// A refusal that repeats every two seconds is noise an operator learns to
// ignore, so a result that made no progress earns a wait. The wait changes
// nothing about the answer: when it expires the state is OBSERVED AGAIN from
// scratch.
//
// NO INTENT SURVIVES A PASS. The loop caches timing and nothing else. Replaying
// a cached intent would be acting on an authority question answered against a
// world that has since moved - which is exactly the staleness the layer below
// refuses, and there is no reason to manufacture it here. A restart forgets the
// backoff entirely and that is safe, because backoff is operational convenience
// rather than authority.

import (
	"fmt"
	"time"
)

// Reconciliation backoff. The bound is small and the ceiling is modest: this is
// a controller maintaining itself, not a client retrying a remote service, and
// an operator who fixed the cause should not wait minutes to see it take.
const (
	reconciliationBackoffBase = 15 * time.Second
	reconciliationBackoffMax  = 5 * time.Minute
)

// ControllerReconciler runs one observe-classify-execute attempt per pass.
type ControllerReconciler struct {
	executor       reconcilerService
	store          handoffStore
	controllerRoot string
	self           ControllerSelfRecord
	// observe reaches the live controller. It is a port because this process
	// IS that controller and can answer directly, while an operator's status
	// command has to cross a socket to ask.
	observe func() (LiveControllerSnapshot, error)

	// Timing state, and the only state this type keeps.
	nextEligibleAt time.Time
	consecutive    int
}

// NewControllerReconciler binds the loop to a service the caller already owns.
func NewControllerReconciler(service *ControllerService, store handoffStore, controllerRoot string, self ControllerSelfRecord, observe func() (LiveControllerSnapshot, error)) *ControllerReconciler {
	return &ControllerReconciler{
		executor: service, store: store, controllerRoot: controllerRoot,
		self: self, observe: observe,
	}
}

// serviceForTest points the loop at a different executor target. It exists so
// a test can count CALLS - the failure mode of a loop is acting too often, and
// a result alone does not show that - without a second production path.
func (r *ControllerReconciler) serviceForTest(service reconcilerService) { r.executor = service }

// ReconciliationAttempt is what one pass did about controller state.
type ReconciliationAttempt struct {
	// Skipped reports a pass that did not look, because a previous result
	// earned a wait.
	Skipped bool `json:"skipped,omitempty"`
	// Result is present when an attempt was made.
	Result *ReconciliationResult `json:"result,omitempty"`
	// Error is a failure to COLLECT - the status could not be gathered at all.
	// It is separate from a refusal, which is an answer.
	Error string `json:"error,omitempty"`
	// NextEligibleAt is when this loop will look again.
	NextEligibleAt time.Time `json:"next_eligible_at"`
}

// Attempt performs at most one reconciliation.
//
// The sequence is fixed and short: gather the status, classify it together with
// this process's own identity, and hand any mutating intent to the executor.
// Nothing between those steps interprets anything.
func (r *ControllerReconciler) Attempt(now time.Time) ReconciliationAttempt {
	if now.Before(r.nextEligibleAt) {
		return ReconciliationAttempt{Skipped: true, NextEligibleAt: r.nextEligibleAt}
	}
	status, err := DescribeControllerStatus(r.store, r.controllerRoot, r.observe, now)
	if err != nil {
		// A COLLECTION FAILURE IS NOT A VERDICT. Nothing is classified and
		// nothing is attempted; the loop simply waits before looking again.
		return r.settle(now, ReconciliationAttempt{Error: err.Error()})
	}
	intent := ClassifyReconciliation(WatcherObservation{Status: status, Self: r.self})
	result := ExecuteReconciliation(r.executor, intent, now)
	return r.settle(now, ReconciliationAttempt{Result: &result})
}

// settle records whether this attempt made progress and when to look again.
//
// PROGRESS IS CONVERGENCE OR NOTHING TO DO. Everything else - an unknown, a
// refusal, an execution that did not converge, a collection failure - waits,
// and waits longer each consecutive time, because a controller that cannot
// proceed usually cannot proceed for a reason that outlasts one poll interval.
func (r *ControllerReconciler) settle(now time.Time, attempt ReconciliationAttempt) ReconciliationAttempt {
	progressed := attempt.Error == "" && attempt.Result != nil &&
		(attempt.Result.Converged || attempt.Result.Intent.Action == ReconcileNone)
	if progressed {
		r.consecutive, r.nextEligibleAt = 0, time.Time{}
		attempt.NextEligibleAt = now
		return attempt
	}
	r.consecutive++
	doublings := r.consecutive - 1
	if doublings > 5 {
		doublings = 5
	}
	wait := reconciliationBackoffBase * time.Duration(1<<doublings)
	if wait > reconciliationBackoffMax {
		wait = reconciliationBackoffMax
	}
	r.nextEligibleAt = now.Add(wait)
	attempt.NextEligibleAt = r.nextEligibleAt
	return attempt
}

// Describe renders one attempt for a report line.
func (a ReconciliationAttempt) Describe() string {
	switch {
	case a.Skipped:
		return fmt.Sprintf("waiting until %s before looking again", a.NextEligibleAt.Format(time.RFC3339))
	case a.Error != "":
		return "the controller status could not be collected: " + a.Error
	case a.Result != nil:
		return a.Result.Summary()
	default:
		return "no reconciliation was attempted"
	}
}
