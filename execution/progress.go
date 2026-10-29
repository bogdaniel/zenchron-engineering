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

// ProgressWriter is the FALLIBLE view of the same durable recorder: it
// returns whether the observation was written. A nil error means durable; an
// error means it was not, and a caller that must not claim progress it failed
// to record (a kernel execution's recording guarantee, #518) acts on it.
type ProgressWriter func(Progress) error

type progressWriterKey struct{}

// WithProgressWriter supplies the fallible recorder and, derived from it, the
// best-effort ProgressRecorder view that discards the error. One write path
// serves both, so a provider that only knows the recorder behaves exactly as
// it did before the writer existed: same writes, same blocking, errors
// dropped. Installing a recorder afterwards replaces only the best-effort
// view.
func WithProgressWriter(ctx context.Context, write ProgressWriter) context.Context {
	if write == nil {
		return ctx
	}
	ctx = WithProgressRecorder(ctx, func(p Progress) { _ = write(p) })
	return context.WithValue(ctx, progressWriterKey{}, write)
}

// ProgressWriterFrom returns the caller's fallible progress writer, or nil
// when none was supplied; a caller then falls back to ProgressRecorder. It is
// not named ProgressWriter because the type has that name.
func ProgressWriterFrom(ctx context.Context) ProgressWriter {
	write, _ := ctx.Value(progressWriterKey{}).(ProgressWriter)
	return write
}
