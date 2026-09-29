package runtime

import (
	"context"
	"errors"
	"time"
)

// errRunStopped is the cancellation CAUSE the execution watcher gives the
// attempt context, and the only evidence anything downstream accepts that an
// execution was interrupted by an operator stop. It is never inferred from a
// run being cancelled: a stop the watcher did not observe while the attempt was
// running interrupted nothing, and an error that merely coincides with a stop
// is still that error.
var errRunStopped = errors.New("the run was stopped while its execution was running")

// runStopObserved reports whether ctx was cancelled by the execution watcher
// because it observed the run's durable disposition as Cancelled.
func runStopObserved(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errRunStopped)
}

// watchExecution lets an operator stop reach a RUNNING execution.invoke (#213).
//
// It is scoped to that one operation kind on purpose. Commit, push, pull
// request creation and publication are runtime effects, not a provider process
// to interrupt, and whether a stop should gate them is #215 - a different
// question this does not answer.
//
// It reads the durable run document, so a stop written by another store handle
// or process is seen, and cancels the attempt context with errRunStopped only
// after SUCCESSFULLY reading the run as Cancelled - the same predicate the
// admission gate in executionAuthorityRevoked uses. A read that fails, or a run
// that is not found, is not a stop: the provider keeps running and the next
// tick reads again. A store that stays unreadable surfaces through the
// admission gate's own durable read once the provider returns. Drain and
// controller shutdown change no durable intent, so neither is observed here;
// shutdown reaches the provider through the parent context instead.
//
// The first observation is synchronous, so a stop committed after Start and
// before the handler never reaches the provider at all. The returned function
// ends the watch, joins its goroutine, and reports whether the stop was
// observed while the attempt was still running.
func (r *EngineeringRuntime) watchExecution(parent context.Context, runID string) (context.Context, func() bool) {
	ctx, cancel := context.WithCancelCause(parent)
	observe := func() bool {
		run, found, err := r.deps.Store.Run(runID)
		if err == nil && found && run.Disposition == Cancelled {
			cancel(errRunStopped)
			return true
		}
		return false
	}
	done := make(chan struct{})
	if observe() {
		close(done)
	} else {
		go func() {
			defer close(done)
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if observe() {
						return
					}
				}
			}
		}()
	}
	return ctx, func() bool {
		cancel(nil)
		<-done
		return runStopObserved(ctx)
	}
}
