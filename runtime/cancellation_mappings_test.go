package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// TestBothCancellationMappingsAreUnchanged pins the TWO mappings #521 records
// as contradiction 5 (ADR-0005): the host's execution.CancellationClass and
// the OpenAI loop's narrower cancellationOwner, which keeps FailureUnknown for
// every cause but an operator stop. The vocabulary move must not unify them;
// Gate B picks one.
func TestBothCancellationMappingsAreUnchanged(t *testing.T) {
	for name, tc := range map[string]struct {
		cause  error
		host   FailureClass
		openai FailureClass
	}{
		"operator stop":     {execution.ErrRunStopped, FailureRunCancelled, FailureRunCancelled},
		"inactivity":        {ErrProviderInactive, FailureProviderNoProgress, FailureUnknown},
		"deadline":          {context.DeadlineExceeded, FailureExecutionIncomplete, FailureUnknown},
		"controller cancel": {context.Canceled, FailureControllerShutdown, FailureUnknown},
		"unrelated cause":   {errors.New("anything else"), FailureControllerShutdown, FailureUnknown},
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tc.cause)
		if got := execution.CancellationClass(context.Cause(ctx)); got != tc.host {
			t.Errorf("%s: host mapping = %q, want %q", name, got, tc.host)
		}
		if got := cancellationOwner(ctx); got != tc.openai {
			t.Errorf("%s: OpenAI mapping = %q, want %q", name, got, tc.openai)
		}
	}
}
