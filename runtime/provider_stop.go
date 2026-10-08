package runtime

// ProviderStop is the typed reason a bounded reasoning loop ended. Every exit
// from the loop carries one, so exhaustion is diagnosable and never silent.
type ProviderStop string

const (
	StopCompleted        ProviderStop = "completed"
	StopIterationBudget  ProviderStop = "iteration_budget_exhausted"
	StopToolCallBudget   ProviderStop = "tool_call_budget_exhausted"
	StopTokenBudget      ProviderStop = "token_budget_exhausted"
	StopDeadlineExceeded ProviderStop = "deadline_exceeded"
	StopNoProgress       ProviderStop = "no_progress"
	StopCancelled        ProviderStop = "cancelled"
	StopProviderError    ProviderStop = "provider_error"
	// StopCostBudgetUnenforceable means the request named a MaxCostMicros
	// ceiling but no trusted cost oracle is configured. The Responses API
	// reports token usage, not money, and there is no authoritative source of
	// monetary cost here: a requested ceiling that cannot be checked is refused
	// rather than silently honored as if it were enforced.
	StopCostBudgetUnenforceable ProviderStop = "cost_budget_unenforceable"
)

// ProviderStopError is the diagnosable outcome of a bounded loop. ExecutionResult
// has no field for a stop reason and adapters.go is shared, so the reason is
// carried as a typed error the caller matches with errors.As.
type ProviderStopError struct {
	Reason ProviderStop
	Detail string
	// Status and Code are the SAFE control-plane facts about an HTTP exchange
	// that actually happened: the response status, and the provider's own error
	// code when it returned one. Both are absent when no exchange occurred.
	// Neither is credential-bearing, and the response BODY is deliberately not
	// here - it belongs in the redacted transcript artifact, never in a caller's
	// durable diagnostic.
	Status int
	Code   string
	// Param names WHICH request field the provider rejected. It is a short,
	// provider-authored field path (the observed 400 named "tools[0].name"),
	// never a message body: without it an operator has to open the raw
	// transcript to learn what was actually wrong with the request.
	Param string
}

func (e *ProviderStopError) Error() string {
	return string(e.Reason) + ": " + e.Detail
}
