package agentkernel

import (
	"context"
	"errors"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// Every termination the kernel emits (execution spec §5), row by row, onto
// the host's outcome, failure class and termination owner.
func TestTerminationMappingTable(t *testing.T) {
	perr := func(c api.ProviderErrorClass) *api.ProviderError { return &api.ProviderError{Class: c} }
	term := func(o api.Outcome, c api.Cause, d api.BudgetDimension) api.Termination {
		return api.Termination{Outcome: o, Cause: c, Dimension: d}
	}
	stop, shutdown, inactive := execution.ErrRunStopped, context.Canceled, execution.ErrProviderInactive
	for name, tc := range map[string]struct {
		t       api.Termination
		perr    *api.ProviderError
		cause   error
		outcome execution.Outcome
		class   execution.FailureClass
		owner   execution.TerminationOwner
	}{
		"completed":                 {term(api.OutcomeCompleted, api.CauseLoopCompleted, ""), nil, nil, execution.Succeeded, "", execution.OwnerProviderExited},
		"completed despite a stop":  {term(api.OutcomeCompleted, api.CauseLoopCompleted, ""), nil, stop, execution.Succeeded, "", execution.OwnerProviderExited},
		"failed despite a stop":     {term(api.OutcomeFailed, api.CauseProviderRefused, ""), nil, stop, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"exhausted deadline":        {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionDeadline), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerDeadline},
		"exhausted iterations":      {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionIterations), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"exhausted tool calls":      {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionToolCalls), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"exhausted input tokens":    {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionInputTokens), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"exhausted output tokens":   {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionOutputTokens), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"exhausted artifact bytes":  {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionArtifactBytes), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"exhausted money":           {term(api.OutcomeExhausted, api.CauseBudgetExhausted, api.DimensionMoney), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"provider refused":          {term(api.OutcomeFailed, api.CauseProviderRefused, ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"provider stopped early":    {term(api.OutcomeFailed, api.CauseProviderFailed, ""), nil, nil, execution.Failed, execution.FailureTransientProvider, execution.OwnerProviderExited},
		"provider auth":             {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderAuth), nil, execution.Failed, execution.FailureProviderAccountUnavailable, execution.OwnerProviderExited},
		"provider transport":        {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderTransport), nil, execution.Failed, execution.FailureConnectivity, execution.OwnerProviderExited},
		"provider server":           {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderServer), nil, execution.Failed, execution.FailureTransientProvider, execution.OwnerProviderExited},
		"provider malformed":        {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderMalformed), nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"provider oversized":        {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderOversized), nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"provider rejected":         {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderRejected), nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"provider deadline":         {term(api.OutcomeFailed, api.CauseProviderFailed, ""), perr(api.ProviderDeadline), nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"rate limited":              {term(api.OutcomeBlocked, api.CauseProviderUnavailable, ""), perr(api.ProviderRateLimited), nil, execution.Failed, execution.FailureProviderRateLimited, execution.OwnerProviderExited},
		"unavailable":               {term(api.OutcomeBlocked, api.CauseProviderUnavailable, ""), perr(api.ProviderUnavailable), nil, execution.Failed, execution.FailureProviderUnavailable, execution.OwnerProviderExited},
		"tool failed":               {term(api.OutcomeFailed, api.CauseToolFailed, ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"invalid request":           {term(api.OutcomeBlocked, api.CauseInvalidRequest, ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"insufficient capacity":     {term(api.OutcomeBlocked, api.CauseInsufficientCapacity, ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"no eligible provider":      {term(api.OutcomeBlocked, api.CauseNoEligibleProvider, ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
		"recording failed":          {term(api.OutcomeIncomplete, api.CauseRecordingFailed, ""), nil, nil, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerProviderExited},
		"cancelled: operator stop":  {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, stop, execution.Cancelled, execution.FailureRunCancelled, execution.OwnerOperatorStop},
		"cancelled: shutdown":       {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, shutdown, execution.Cancelled, execution.FailureControllerShutdown, execution.OwnerControllerShutdown},
		"cancelled: inactivity":     {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, inactive, execution.Failed, execution.FailureProviderNoProgress, execution.OwnerInactivity},
		"cancelled: host deadline":  {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, context.DeadlineExceeded, execution.Failed, execution.FailureExecutionIncomplete, execution.OwnerDeadline},
		"cancelled: wrapped stop":   {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, kernelCause(stop), execution.Cancelled, execution.FailureRunCancelled, execution.OwnerOperatorStop},
		"cancelled: wrapped silent": {term(api.OutcomeCancelled, api.CauseHostCancelled, ""), nil, kernelCause(inactive), execution.Failed, execution.FailureProviderNoProgress, execution.OwnerInactivity},
		"unknown future outcome":    {term("paused", "something", ""), nil, nil, execution.Failed, execution.FailureUnknown, execution.OwnerProviderExited},
	} {
		outcome, class := classify(tc.t, tc.perr, tc.cause)
		if outcome != tc.outcome || class != tc.class {
			t.Errorf("%s: classified %s/%s, want %s/%s", name, outcome, class, tc.outcome, tc.class)
		}
		if owner := ownerOf(tc.t, tc.cause); owner != tc.owner {
			t.Errorf("%s: owner %q, want %q", name, owner, tc.owner)
		}
	}
}

// The kernel provenance travels with the host cause; the host owner is
// still read from the host cause, so no sixth owner exists.
func TestKernelCauseCarriesMatchingProvenance(t *testing.T) {
	for cause, want := range map[error]api.CancellationProvenance{
		execution.ErrRunStopped:       api.CancelOperatorStop,
		context.Canceled:              api.CancelControllerShutdown,
		context.DeadlineExceeded:      api.CancelDeadline,
		execution.ErrProviderInactive: api.CancelUnknown,
	} {
		joined := kernelCause(cause)
		var c *api.CancelCause
		if !errors.As(joined, &c) || c.Provenance != want {
			t.Errorf("cause %v: kernel provenance %v, want %q", cause, c, want)
		}
		if !errors.Is(joined, cause) {
			t.Errorf("cause %v lost from %v", cause, joined)
		}
	}
}

// Usage is observation only: unknown stays nil, estimates never report.
func TestUsageIsObservationOnly(t *testing.T) {
	tokens, cost := usage(api.Usage{
		Reported:  api.TokenUsage{Input: api.Count(100), Output: nil},
		Estimated: api.TokenUsage{Input: api.Count(90), Output: api.Count(10)},
		Cost:      api.Cost{Known: false, Micros: 42},
	})
	if tokens != nil || cost != nil {
		t.Errorf("unknown usage reported as tokens %v cost %v", tokens, cost)
	}
	tokens, cost = usage(api.Usage{
		Reported: api.TokenUsage{Input: api.Count(100), Output: api.Count(20)},
		Cost:     api.Cost{Known: true, Micros: 0},
	})
	if tokens == nil || *tokens != 120 || cost == nil || *cost != 0 {
		t.Errorf("known usage reported as tokens %v cost %v", tokens, cost)
	}
}

func TestUncertainSideEffectIsNeverNothingChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		res  api.ExecutionResult
		want bool
	}{
		"tool failed": {api.ExecutionResult{Termination: api.Termination{Cause: api.CauseToolFailed}}, true},
		"observed before a stop": {api.ExecutionResult{
			Termination:  api.Termination{Cause: api.CauseHostCancelled},
			Observations: []api.Observation{{Kind: api.EventToolExecuted, Detail: uncertainSideEffect + ": error boom"}},
		}, true},
		"ordinary tool": {api.ExecutionResult{
			Observations: []api.Observation{{Kind: api.EventToolExecuted, Detail: "ok "}},
		}, false},
	} {
		if got := uncertain(tc.res); got != tc.want {
			t.Errorf("%s: uncertain = %t, want %t", name, got, tc.want)
		}
	}
}
