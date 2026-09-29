package runtime

import (
	"context"
	"fmt"
	"time"
)

// watchAttempt observes durable intent, including stops written by another
// store handle or process. It owns no run state and is joined before returning.
// Drain does not change durable intent and therefore does not cancel attempts.
func (r *EngineeringRuntime) watchAttempt(parent context.Context, runID string) (context.Context, func() error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	var watchErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, found, err := r.deps.Store.Run(runID)
				if err != nil || !found || run.Disposition == Cancelled {
					watchErr = err
					if err == nil && !found {
						watchErr = fmt.Errorf("attempt run %q disappeared", runID)
					}
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() error { cancel(); <-done; return watchErr }
}

func (r *EngineeringRuntime) cancelledAttempt(runID string) (Outcome, bool, error) {
	run, found, err := r.deps.Store.Run(runID)
	if err != nil {
		return Outcome{}, false, err
	}
	if found && run.Disposition == Cancelled {
		return Outcome{RunID: runID, Disposition: Cancelled, Reason: run.Reason}, true, nil
	}
	return Outcome{}, false, nil
}
