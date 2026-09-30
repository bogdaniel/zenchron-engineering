package runtime

import (
	"context"
	"errors"
	"sync"
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

// ProviderNotStartedError is the executor's answer when it refused to start a
// provider because its context had already ended. Cause is that context's
// cause at the refusal.
type ProviderNotStartedError struct{ Cause error }

func (e *ProviderNotStartedError) Error() string {
	return "the provider was not started: " + e.Cause.Error()
}

func (e *ProviderNotStartedError) Unwrap() error { return e.Cause }

// holdReaper is a test seam: it runs in the reaper goroutine before it waits
// on the root, so a test can hold the reap back after the root has exited.
var holdReaper = func() {}

// terminalOwnership is the single-assignment state UNDECIDED -> exactly one
// owner, with no transition out. settle commits the first owner offered and
// returns the committed one to every caller, however many race to settle it.
type terminalOwnership struct {
	once  sync.Once
	owner TerminationOwner
}

func (o *terminalOwnership) settle(owner TerminationOwner) TerminationOwner {
	o.once.Do(func() { o.owner = owner })
	return o.owner
}

// ownerOfCancellation names the owner of a termination the executor initiated
// because ctx ended. A context keeps the FIRST cause it was cancelled with, so
// this is the event that actually fired first, not whichever one is inspected
// last.
func ownerOfCancellation(ctx context.Context) TerminationOwner {
	return ownerOfCause(context.Cause(ctx))
}

// ownerOfCause names the owner a cancellation cause stands for.
func ownerOfCause(cause error) TerminationOwner {
	switch {
	case errors.Is(cause, errRunStopped):
		return OwnerOperatorStop
	case errors.Is(cause, ErrProviderInactive):
		return OwnerInactivity
	case errors.Is(cause, context.DeadlineExceeded):
		return OwnerDeadline
	default:
		return OwnerControllerShutdown
	}
}
