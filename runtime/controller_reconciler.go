package runtime

// CARRYING OUT AN INTENT, and containing no reasoning of its own.
//
// The classifier decided what should be attempted. This file attempts it, by
// calling operations that already enforce their own authority, and reports what
// happened. It determines nothing: not who should be active, not who owns the
// role, not whether an unreachable process is dead, not whether an admission
// should exist. If it ever needs to, the tripwire has been crossed and
// something below is missing.
//
// COMPOSITION IS NOT A NEW AUTHORITY SEMANTIC. Resuming service takes two
// calls because the protocol deliberately separates them: RecoverActivated
// resumes an activation already on disk and leaves the gate closed, and
// EnableWorkAdmission opens service against the durable record, the live role
// and this process's proven identity. Each re-checks its own preconditions at
// the moment of the call, so the executor's job is sequencing and reporting,
// not deciding.
//
// A PROJECTION THAT WOULD NOT REPAIR DOES NOT STOP SERVICE. #276 established
// that authority is durable and the stable entrypoint is a repairable
// projection of it. The naive shape here -
//
//	if err := RecoverActivated(h); err != nil { return err }
//	EnableWorkAdmission(h)
//
// - would quietly reinstate the pointer as authoritative, in the one place
// nobody would look for it. A failed repair is reported as drift and service is
// still attempted; every other refusal stops.
//
// THE ROLE CAN VANISH BETWEEN THE TWO CALLS, and that is ordinary rather than
// exceptional. The second operation refuses through its own authority check,
// the executor reports an incomplete convergence, and nothing is reacquired,
// retried under a different authority, or reinterpreted. Availability loses;
// authority is not reconstructed.

import (
	"errors"
	"fmt"
	"time"
)

// StepOutcome is what one attempted operation did.
type StepOutcome string

const (
	StepNotAttempted StepOutcome = "not_attempted"
	StepSucceeded    StepOutcome = "succeeded"
	StepRefused      StepOutcome = "refused"
	StepDrifted      StepOutcome = "drifted"
)

// ReconciliationStep is one operation's result, kept separate from the others
// so a watcher running repeatedly can be reasoned about: "recovery succeeded
// and admission refused" is a different world from "recovery refused", and a
// single error would flatten them.
type ReconciliationStep struct {
	Outcome StepOutcome `json:"outcome"`
	Detail  string      `json:"detail,omitempty"`
}

// ReconciliationResult is what one pass of the executor did.
type ReconciliationResult struct {
	Intent ReconciliationIntent `json:"intent"`
	// Recovery is the RecoverActivated call: resuming an activation already on
	// disk, which also repairs the projection.
	Recovery ReconciliationStep `json:"recovery"`
	// Projection reports the stable entrypoint separately from the recovery
	// that attempted it, because a repair can fail without the recovery having
	// failed.
	Projection ReconciliationStep `json:"projection"`
	// WorkAdmission is the EnableWorkAdmission call.
	WorkAdmission ReconciliationStep `json:"work_admission"`
	// Converged reports whether the state the intent aimed at was reached.
	Converged   bool      `json:"converged"`
	AttemptedAt time.Time `json:"attempted_at"`
}

// reconcilerService is the executor's whole reach into the runtime: two
// operations, both of which enforce their own authority. It is an interface so
// a test can prove the sequencing - including the role vanishing between the
// two calls - without racing a real process to do it.
type reconcilerService interface {
	RecoverActivated(handoffID string) (ControllerHandoff, error)
	EnableWorkAdmission(handoffID string) error
}

// ExecuteReconciliation carries out one intent.
//
// Every branch either calls an already-authorized operation or does nothing.
// There is no path on which this function decides that an operation should
// have succeeded, or that a refusal was mistaken.
func ExecuteReconciliation(service reconcilerService, intent ReconciliationIntent, now time.Time) ReconciliationResult {
	result := ReconciliationResult{
		Intent: intent, AttemptedAt: now,
		Recovery:      ReconciliationStep{Outcome: StepNotAttempted},
		Projection:    ReconciliationStep{Outcome: StepNotAttempted},
		WorkAdmission: ReconciliationStep{Outcome: StepNotAttempted},
	}
	switch intent.Action {
	case ReconcileRepairProjection:
		return repairProjection(service, result)
	case ReconcileResumeActivatedService:
		return resumeService(service, result)
	default:
		// none, unknown, refuse - and anything a future classifier adds. An
		// action this build does not recognise attempts NOTHING, for the same
		// reason the classifier treats an unrecognised verdict as unknown.
		result.Converged = intent.Action == ReconcileNone
		return result
	}
}

// repairProjection asks the runtime to resume the recorded activation, which is
// the operation that repairs the stable entrypoint.
//
// RecoverActivated is the proven path rather than a bare pointer write: it
// checks that the transition is activated, proves this process is the
// generation the record names, and performs the repair under the live role. A
// watcher writing the symlink itself would be constructing a target instead of
// asking for the recorded one.
func repairProjection(service reconcilerService, result ReconciliationResult) ReconciliationResult {
	_, err := service.RecoverActivated(result.Intent.HandoffID)
	switch {
	case err == nil:
		result.Recovery = ReconciliationStep{Outcome: StepSucceeded}
		result.Projection = ReconciliationStep{Outcome: StepSucceeded}
		result.Converged = true
	case isProjectionRepairError(err):
		// The activation stands and the pointer does not. Reported, not
		// escalated: a stale entrypoint is operator drift rather than a
		// governance failure.
		result.Recovery = ReconciliationStep{Outcome: StepSucceeded}
		result.Projection = ReconciliationStep{Outcome: StepDrifted, Detail: err.Error()}
	default:
		result.Recovery = ReconciliationStep{Outcome: StepRefused, Detail: err.Error()}
	}
	return result
}

// resumeService composes the two operations that reach the convergence the
// intent names.
func resumeService(service reconcilerService, result ReconciliationResult) ReconciliationResult {
	_, err := service.RecoverActivated(result.Intent.HandoffID)
	switch {
	case err == nil:
		result.Recovery = ReconciliationStep{Outcome: StepSucceeded}
		result.Projection = ReconciliationStep{Outcome: StepSucceeded}
	case isProjectionRepairError(err):
		// SERVICE IS STILL ATTEMPTED. The successor is active; a symlink is
		// stale. Stopping here would make the projection a condition of
		// service, which is exactly what the protocol below refuses to let it
		// become.
		result.Recovery = ReconciliationStep{Outcome: StepSucceeded}
		result.Projection = ReconciliationStep{Outcome: StepDrifted, Detail: err.Error()}
	default:
		// An authority, identity or durable-state refusal. Admission is NOT
		// attempted: the recovery that would have established the right to
		// serve did not happen, and asking anyway would be hoping a second
		// check disagrees with the first.
		result.Recovery = ReconciliationStep{Outcome: StepRefused, Detail: err.Error()}
		return result
	}
	if err := service.EnableWorkAdmission(result.Intent.HandoffID); err != nil {
		// The role may have gone between the two calls. That is ordinary: the
		// operation refused through its own authority check, and nothing here
		// reacquires, retries under another authority, or reinterprets it.
		result.WorkAdmission = ReconciliationStep{Outcome: StepRefused, Detail: err.Error()}
		return result
	}
	result.WorkAdmission = ReconciliationStep{Outcome: StepSucceeded}
	result.Converged = true
	return result
}

// isProjectionRepairError reports the one failure that does not stop service.
func isProjectionRepairError(err error) bool {
	var repair *ProjectionRepairFailedError
	return errors.As(err, &repair)
}

// Summary is a one-line account for an operator, built from the dimensions
// rather than replacing them.
func (r ReconciliationResult) Summary() string {
	if r.Intent.Action == ReconcileNone || r.Intent.Action == ReconcileUnknown {
		return string(r.Intent.Action) + ": " + r.Intent.Reason
	}
	return fmt.Sprintf("%s: recovery %s, projection %s, admission %s, converged %t",
		r.Intent.Action, r.Recovery.Outcome, r.Projection.Outcome, r.WorkAdmission.Outcome, r.Converged)
}
