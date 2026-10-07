package execution

import (
	"context"
	"errors"
)

// TerminationOwner is the ONE event that ended, or successfully initiated the
// ending of, a physical provider execution (#213). It is committed exactly
// once, by the executor, at the instant it is decided, and nothing later
// replaces it: run state, authority and publication may still change, but the
// attempt's termination cause, bound and provider failure class stay
// attributable to this owner.
type TerminationOwner string

const (
	// OwnerUndecided: the executor did not report an owner.
	OwnerUndecided TerminationOwner = ""
	// OwnerProviderExited: the process exited before any external event
	// initiated its termination - however long its descendants then held its
	// output pipes open.
	OwnerProviderExited TerminationOwner = "provider_exited"
	// OwnerDeadline: the invocation's wall deadline (the attempt wall, or the
	// run's remaining active work, whichever bound set it) fired first and
	// the executor began terminating the process because of it.
	OwnerDeadline TerminationOwner = "deadline"
	// OwnerInactivity: the inactivity watchdog fired first.
	OwnerInactivity TerminationOwner = "inactivity"
	// OwnerOperatorStop: an operator stop, observed by the execution watcher,
	// fired first.
	OwnerOperatorStop TerminationOwner = "operator_stop"
	// OwnerControllerShutdown: the controller's own context ended first.
	OwnerControllerShutdown TerminationOwner = "controller_shutdown"
	// OwnerNotStarted: the executor refused to start the process because its
	// context had already ended. No process existed, so no provider
	// termination is attributed to anything.
	OwnerNotStarted TerminationOwner = "not_started"
)

// NotStartedError is the executor's answer when it refused to start a
// provider because its context had already ended. Cause is that context's
// cause at the refusal.
type NotStartedError struct{ Cause error }

func (e *NotStartedError) Error() string {
	return "the provider was not started: " + e.Cause.Error()
}

func (e *NotStartedError) Unwrap() error { return e.Cause }

// ErrRunStopped is the cancellation CAUSE the host's execution watcher gives
// the provider's context. It is set only after a successful durable read of the
// run as Cancelled, never inferred from an error that coincides with a stop.
var ErrRunStopped = errors.New("the run was stopped while its execution was running")

// ErrProviderInactive is the cause a cancelled provider context carries when
// the inactivity policy - not the deadline, not a shutdown, not the operator -
// is what ended the invocation.
//
// It is a CAUSE rather than a return value because the thing that observes the
// silence is the process runner, several frames below the adapter that has to
// classify it, and context.Cause is the one channel that already crosses that
// boundary. Without it an inactivity kill was indistinguishable from a
// supervisor shutdown through ctx.Err() alone, and those two mean opposite
// things to a run: one is a dead provider, the other is a resumable pause.
var ErrProviderInactive = errors.New("the provider produced no output within its inactivity bound")

// CancellationClass is the failure class a cancellation cause stands for. It is
// the one mapping every result built from a cancelled context uses, so a
// shutdown always waits and resumes, and an operator stop always stops.
// The OpenAI loop keeps a second, narrower mapping of its own (ADR-0005,
// contradiction 5); the two are preserved as they are until Gate B picks one.
func CancellationClass(cause error) FailureClass {
	switch OwnerOfCause(cause) {
	case OwnerOperatorStop:
		return FailureRunCancelled
	case OwnerDeadline:
		return FailureExecutionIncomplete
	case OwnerInactivity:
		return FailureProviderNoProgress
	}
	return FailureControllerShutdown
}

// OwnerOfCause names the owner a cancellation cause stands for.
func OwnerOfCause(cause error) TerminationOwner {
	switch {
	case errors.Is(cause, ErrRunStopped):
		return OwnerOperatorStop
	case errors.Is(cause, ErrProviderInactive):
		return OwnerInactivity
	case errors.Is(cause, context.DeadlineExceeded):
		return OwnerDeadline
	default:
		return OwnerControllerShutdown
	}
}

// NotStartedResult is the answer for a provider the executor never started
// because its context had already ended (#213). It carries no invocation
// provenance and no transcript: nothing ran. The class names why nothing ran,
// from the cause the executor recorded at the refusal.
func NotStartedResult(providerID, model, authMode string, attempt int, notStarted *NotStartedError) Result {
	result := Result{ProviderID: providerID, Model: model, AuthMode: authMode, Attempt: attempt, Outcome: Cancelled}
	class := CancellationClass(notStarted.Cause)
	if class != FailureControllerShutdown && class != FailureRunCancelled {
		result.Outcome = Failed // a runtime bound ended it, not a cancellation
	}
	result.Failure = &Failure{Classification: class}
	return result
}
