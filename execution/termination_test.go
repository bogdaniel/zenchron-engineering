package execution_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// The cancellation mapping moved out of runtime/termination_owner.go (#521).
// This table is that file's mapping, sentinel by sentinel, so the move cannot
// have changed which owner or class any cause stands for. A context keeps the
// cause it was cancelled with, wrapped or not, so the wrapped rows matter.
func TestCancellationMappingIsUnchangedByTheMove(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		owner execution.TerminationOwner
		class execution.FailureClass
	}{
		"operator stop":         {execution.ErrRunStopped, execution.OwnerOperatorStop, execution.FailureRunCancelled},
		"wrapped operator stop": {fmt.Errorf("watcher: %w", execution.ErrRunStopped), execution.OwnerOperatorStop, execution.FailureRunCancelled},
		"inactivity":            {execution.ErrProviderInactive, execution.OwnerInactivity, execution.FailureProviderNoProgress},
		"wrapped inactivity":    {fmt.Errorf("watchdog: %w", execution.ErrProviderInactive), execution.OwnerInactivity, execution.FailureProviderNoProgress},
		"deadline":              {context.DeadlineExceeded, execution.OwnerDeadline, execution.FailureExecutionIncomplete},
		"controller cancel":     {context.Canceled, execution.OwnerControllerShutdown, execution.FailureControllerShutdown},
		"no cause":              {nil, execution.OwnerControllerShutdown, execution.FailureControllerShutdown},
		"unrelated cause":       {errors.New("anything else"), execution.OwnerControllerShutdown, execution.FailureControllerShutdown},
	} {
		if owner := execution.OwnerOfCause(tc.cause); owner != tc.owner {
			t.Errorf("%s: OwnerOfCause = %q, want %q", name, owner, tc.owner)
		}
		if class := execution.CancellationClass(tc.cause); class != tc.class {
			t.Errorf("%s: CancellationClass = %q, want %q", name, class, tc.class)
		}
	}
}

// NotStartedResult keeps runtime's notStartedResult outcome rule: only the two
// cancellation classes are a Cancelled outcome, a runtime bound is Failed.
func TestNotStartedResultOutcomeFollowsTheCause(t *testing.T) {
	for cause, want := range map[error]execution.Outcome{
		execution.ErrRunStopped:       execution.Cancelled,
		context.Canceled:              execution.Cancelled,
		context.DeadlineExceeded:      execution.Failed,
		execution.ErrProviderInactive: execution.Failed,
	} {
		result := execution.NotStartedResult("p", "m", "a", 3, &execution.NotStartedError{Cause: cause})
		if result.Outcome != want || result.Failure == nil || result.Failure.Classification != execution.CancellationClass(cause) || result.Attempt != 3 {
			t.Errorf("cause %v: got %+v, want outcome %q with class %q", cause, result, want, execution.CancellationClass(cause))
		}
	}
}
