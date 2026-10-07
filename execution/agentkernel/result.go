package agentkernel

import (
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// uncertainSideEffect is the detail prefix the kernel records on tool.executed
// when a possibly mutating tool ran before the execution stopped (execution
// spec §5, engine.observeUncertainMutation).
const uncertainSideEffect = "side effect outcome uncertain"

// TerminationError carries the kernel's settled termination to the host when
// an execution did not complete. The host's diagnostic reads it; nothing
// here is acceptance or authority.
type TerminationError struct {
	Termination api.Termination
	// ProviderError is the last typed provider failure the adapter's provider
	// worker returned, or nil.
	ProviderError *api.ProviderError
	// UncertainSideEffect reports that a possibly mutating tool ran and its
	// outcome is unknown, so the host must reconcile the candidate from the
	// workspace itself before any retry. It is never "nothing changed".
	UncertainSideEffect bool
}

func (e *TerminationError) Error() string {
	t := e.Termination
	s := "agent execution kernel: " + string(t.Outcome) + "/" + string(t.Cause)
	if t.Dimension != "" {
		s += "/" + string(t.Dimension)
	}
	if t.Cancellation != "" {
		s += " (cancellation " + string(t.Cancellation) + ")"
	}
	if e.UncertainSideEffect {
		s += "; " + uncertainSideEffect + ": the candidate must be reconciled"
	}
	if t.Detail != "" {
		s += ": " + t.Detail
	}
	return s
}

// classify is the total mapping from a kernel termination onto the host's
// outcome and failure class. hostCause is the cause the host's own context
// (or the adapter's inactivity watchdog) cancelled the execution with, nil if
// neither did; it alone decides a cancellation's class, so the kernel's
// settlement latch never becomes a sixth termination owner.
func classify(t api.Termination, perr *api.ProviderError, hostCause error) (execution.Outcome, execution.FailureClass) {
	switch t.Outcome {
	case api.OutcomeCompleted:
		if t.Cause == api.CauseLoopCompleted {
			return execution.Succeeded, ""
		}
	case api.OutcomeCancelled:
		return cancellation(hostCause)
	case api.OutcomeExhausted:
		// Every exhausted dimension is one of the host's or the adapter's own
		// bounds reached: incomplete work, never an unknown failure. The
		// deadline is the same class the host's deadline cause stands for.
		return execution.Failed, execution.FailureExecutionIncomplete
	case api.OutcomeIncomplete:
		// recording_failed: the transcript or admission was not written. The
		// observed outcome is kept in the detail.
		return execution.Failed, execution.FailureExecutionIncomplete
	case api.OutcomeBlocked:
		if t.Cause == api.CauseProviderUnavailable {
			return execution.Failed, providerClass(perr)
		}
	case api.OutcomeFailed:
		switch t.Cause {
		case api.CauseProviderFailed:
			if perr == nil {
				// "provider stopped without completing": no typed error, the
				// turn simply did not finish. A bounded host retry.
				return execution.Failed, execution.FailureTransientProvider
			}
			return execution.Failed, providerClass(perr)
		case api.CauseToolFailed:
			// The side effect's outcome is unknown; stop rather than guess.
			return execution.Failed, execution.FailureUnknown
		}
	}
	// provider_refused, invalid_request, insufficient_capacity,
	// no_eligible_provider and anything this kernel version does not emit.
	return execution.Failed, execution.FailureUnknown
}

// cancellation maps the host's cause exactly as every other host result
// built from a cancelled context does (execution.CancellationClass): only a
// controller shutdown or an operator stop is a Cancelled outcome.
func cancellation(hostCause error) (execution.Outcome, execution.FailureClass) {
	class := execution.CancellationClass(hostCause)
	if class == execution.FailureControllerShutdown || class == execution.FailureRunCancelled {
		return execution.Cancelled, class
	}
	return execution.Failed, class
}

// providerClass maps the kernel's provider error classes onto the host's.
// The kernel classifies every 429 rate_limited, so quota is not
// distinguished from a rate limit (integration plan §5.6, carried); both
// route to the host's durable wait.
func providerClass(perr *api.ProviderError) execution.FailureClass {
	if perr == nil {
		return execution.FailureUnknown
	}
	switch perr.Class {
	case api.ProviderRateLimited:
		return execution.FailureProviderRateLimited
	case api.ProviderUnavailable:
		return execution.FailureProviderUnavailable
	case api.ProviderAuth:
		return execution.FailureProviderAccountUnavailable
	case api.ProviderTransport:
		return execution.FailureConnectivity
	case api.ProviderServer:
		return execution.FailureTransientProvider
	case api.ProviderDeadline:
		return execution.FailureExecutionIncomplete
	}
	// malformed, oversized, rejected, cancelled: a request or adapter defect.
	return execution.FailureUnknown
}

// ownerOf is the host termination owner of one settled execution: the host
// cause when the kernel settled on the host's (or the inactivity watchdog's)
// cancellation, the deadline when that bound ran out, and otherwise the
// execution ending on its own. A cancellation the kernel settled after an
// observed outcome ended nothing, so it owns nothing.
func ownerOf(t api.Termination, hostCause error) execution.TerminationOwner {
	switch {
	case t.Outcome == api.OutcomeCancelled:
		return execution.OwnerOfCause(hostCause)
	case t.Outcome == api.OutcomeExhausted && t.Dimension == api.DimensionDeadline:
		return execution.OwnerDeadline
	}
	return execution.OwnerProviderExited
}

// uncertain reports whether the kernel observed a possibly mutating tool
// whose outcome is unknown.
func uncertain(result api.ExecutionResult) bool {
	if result.Termination.Cause == api.CauseToolFailed {
		return true
	}
	for _, o := range result.Observations {
		if o.Kind == api.EventToolExecuted && strings.HasPrefix(o.Detail, uncertainSideEffect) {
			return true
		}
	}
	return false
}

// usage reports the kernel's provider-reported usage as OBSERVATIONS. An
// unknown count stays nil, estimates are never reported, and nothing here
// consumes a host budget.
func usage(u api.Usage) (tokens, costMicros *int64) {
	if in, out := u.Reported.Input, u.Reported.Output; in != nil && out != nil {
		total := *in + *out
		tokens = &total
	}
	if u.Cost.Known {
		cost := u.Cost.Micros
		costMicros = &cost
	}
	return tokens, costMicros
}
