package runtime

// How an operation result's recorded failure class is read back: the class
// itself, whether it leaves the same operation eligible again, whether it routes
// to a wait, and whether the attempt reached a worker (which decides what a wait
// may refund). Moved out of reconciler.go unchanged.

import "encoding/json"

// failureClassOf reads the same one shared mutationResult field lastFailure
// does, without requiring the caller to care whether the class routes
// anywhere in particular - unlike waitRoutedFailure, which only answers for
// RouteWait.
func failureClassOf(raw json.RawMessage) FailureClass {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil {
		return ""
	}
	return result.FailureClass
}

// journalled reports whether an effect appended one particular event type.
func journalled(entries []journalEntry, eventType string) bool {
	for _, entry := range entries {
		if entry.Type == eventType {
			return true
		}
	}
	return false
}

// reattemptable reports whether a route leaves the SAME operation eligible to
// run again. Retry means run it again now; wait means run it again once the
// external condition it named has been corrected. Every other route names a
// different operation, or none.
func reattemptable(route FailureRoute) bool {
	return route == RouteRetry || route == RouteWait
}

// waitRoutedFailure reads the class an operation result recorded and reports it
// only when it routes to a wait. It reads the same one shared field lastFailure
// does, so this is not an execution.invoke special case: any handler that
// records a wait-routed class settles the run into that wait.
// providerExecuted reports whether the attempt recorded in this result actually
// reached a worker. An unreadable or absent result is treated as HAVING
// executed: refunding budget is the generous direction, and guessing generously
// about an unknown is how a bounded budget stops being one.
func providerExecuted(raw json.RawMessage) bool {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil {
		return true
	}
	return result.ProviderExecuted
}

func waitRoutedFailure(raw json.RawMessage) (FailureClass, bool) {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil || result.FailureClass == "" {
		return "", false
	}
	if RouteFailure(result.FailureClass) != RouteWait {
		return "", false
	}
	return result.FailureClass, true
}
