package api

import (
	"context"
	"errors"
)

// CancelCause is the error a host passes to a context.CancelCauseFunc to state
// why it cancelled an execution.
type CancelCause struct{ Provenance CancellationProvenance }

func (c *CancelCause) Error() string { return "execution cancelled: " + string(c.Provenance) }

// Cancellation returns a cause for context.WithCancelCause.
func Cancellation(p CancellationProvenance) error { return &CancelCause{Provenance: p} }

// CancellationOf reports the provenance of ctx's cancellation. A bare cancel
// is unknown; only a host-supplied cause or an expired deadline is more.
func CancellationOf(ctx context.Context) CancellationProvenance {
	cause := context.Cause(ctx)
	var c *CancelCause
	if errors.As(cause, &c) {
		return c.Provenance
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return CancelDeadline
	}
	return CancelUnknown
}
