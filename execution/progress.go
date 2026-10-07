package execution

import (
	"context"
	"time"
)

// Progress is one durable observation of a live provider invocation,
// as the host scheduler receives it.
type Progress struct {
	// Key is the progress fingerprint; the durable instant moves only when it
	// changes.
	Key string
	// Age is how long before this write the progress was OBSERVED. The row
	// must say when the work moved, not when the coalescer wrote it (#352).
	Age time.Duration
	// Suspended reports a structured main-thread tool held open (#322), and
	// SuspendedAge how long before this write it opened.
	Suspended    bool
	SuspendedAge time.Duration
	// Final is the recorder's closing write: the process has ended under an
	// observing controller, so nothing it observed is still unwritten.
	Final bool
}

type progressRecorderKey struct{}

// WithProgressRecorder supplies the DURABLE half of the policy: where
// observed progress is written so that status can report it and a restart can
// read it back.
//
// It is separate from the bound itself because the two come from different
// places. The bound is a budget and travels with the request, like every other
// budget; the recorder is the caller's own durable operation state, which an
// adapter must not know the shape of. A caller that keeps none - the planner,
// a probe, a test - supplies none, and the bound still applies.
func WithProgressRecorder(ctx context.Context, record func(Progress)) context.Context {
	if record == nil {
		return ctx
	}
	return context.WithValue(ctx, progressRecorderKey{}, record)
}

// ProgressRecorder returns the caller's durable progress recorder, or
// nil when none was supplied.
func ProgressRecorder(ctx context.Context) func(Progress) {
	record, _ := ctx.Value(progressRecorderKey{}).(func(Progress))
	return record
}
