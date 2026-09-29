package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errRunStopped is the cancellation CAUSE the execution watcher gives the
// provider's context. It is set only after a successful durable read of the
// run as Cancelled, never inferred from an error that coincides with a stop.
var errRunStopped = errors.New("the run was stopped while its execution was running")

// errStoppedBeforeProvider is the diagnostic of an attempt whose run was
// already stopped when its provider would have started. No provider ran, so no
// provider termination is attributed to the stop.
var errStoppedBeforeProvider = errors.New("the run was stopped before the provider was started")

// runStopObserved reports whether ctx was cancelled by the execution watcher
// because it observed the run's durable disposition as Cancelled.
func runStopObserved(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errRunStopped)
}

// executionWatch is what the watcher saw while one provider invocation ran.
type executionWatch struct {
	// observed: the stop was read, and the provider's context cancelled,
	// before Provider.Execute returned.
	observed bool
	// ended: the stop, and nothing else, ended this attempt - either no
	// provider was started, or the provider reported only its cancelled
	// context. Set by invokeExecution; it is what makes the attempt
	// interrupted.
	ended bool
	// readFailures counts durable reads that failed; none of them was
	// treated as a stop.
	readFailures int
	lastReadErr  error
}

// settle applies the watch to the attempt's effect. The attempt counts as
// interrupted only when the stop ended it; a provider that returned by itself,
// or was ended by a runtime bound first, keeps its own outcome even if a stop
// was observed too.
//
// Read failures the watcher tolerated are noted, bounded and redacted, in the
// attempt's existing diagnostic message. LIMITATION: an attempt that ends with
// no diagnostic (a successful execution) has no existing field to carry them
// without a contract change, so they are not recorded there; the admission
// gate's own durable read after the provider returned is the read that
// decided anything. Surfacing them for successful attempts needs a follow-up.
func (w executionWatch) settle(out *effect) {
	record, ok := out.result.(executionRecord)
	if !ok || record.Diagnostic == nil {
		return
	}
	out.interrupted = w.ended
	if w.readFailures > 0 {
		note := fmt.Sprintf("stop watcher: %d durable run read(s) failed, last: %s", w.readFailures, w.lastReadErr)
		if record.Diagnostic.Message != "" {
			note = record.Diagnostic.Message + "; " + note
		}
		record.Diagnostic.Message = sanitizedDetail(note)
	}
}

// watchExecution observes durable run cancellation only while the provider
// execution is active, and may interrupt that provider execution (#213).
// invokeExecution arms it immediately before Provider.Execute and ends it the
// moment Execute returns; nothing before or after the provider, and no other
// operation kind, is watched. Commit, push, pull request creation and
// publication are #215, not this.
//
// It reads the durable run document, so a stop written by another store handle
// or process is seen, and cancels the provider's context with errRunStopped
// only after SUCCESSFULLY reading the run as Cancelled. A failed read or a
// missing run is not a stop: the provider keeps running, the next tick reads
// again, and the failures are counted for the attempt's diagnostic. Drain and
// controller shutdown change no durable intent and are not observed here;
// shutdown reaches the provider through the parent context instead.
//
// The first read is synchronous. The returned function ends the watch, joins
// its goroutine and reports what was seen; call it as soon as Execute returns.
func (r *EngineeringRuntime) watchExecution(parent context.Context, runID string) (context.Context, func() executionWatch) {
	ctx, cancel := context.WithCancelCause(parent)
	var watch executionWatch
	observe := func() bool {
		run, found, err := r.deps.Store.Run(runID)
		if err != nil {
			watch.readFailures++
			watch.lastReadErr = err
			return false
		}
		if found && run.Disposition == Cancelled {
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
	return ctx, func() executionWatch {
		cancel(nil)
		<-done
		watch.observed = runStopObserved(ctx)
		return watch
	}
}
