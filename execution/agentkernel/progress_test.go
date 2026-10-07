package agentkernel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

var errProgressStore = errors.New("progress store unavailable")

// progressWrites is a host ProgressWriter that fails the writes fail picks.
type progressWrites struct {
	mu     sync.Mutex
	writes []execution.Progress
	fail   func(execution.Progress) bool
}

func (w *progressWrites) write(p execution.Progress) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, p)
	if w.fail != nil && w.fail(p) {
		return errProgressStore
	}
	return nil
}

// An in-flight progress write the host could not make durable settles
// recording_failed exactly as a transcript failure does: the granted command
// never runs and the provider is not called again.
func TestInFlightProgressWriteFailureIsARecordingFailure(t *testing.T) {
	provider := scripted.New(toolUse("c1", "run_command", `{"command":"go-test"}`), done("never"))
	cfg := testConfig(t, provider)
	run := &runner{}
	cfg.Commands, cfg.Catalogue = run, []CommandSpec{{Name: "go-test", Argv: []string{"go", "test"}, Timeout: time.Minute}}
	req := testRequest(t)
	req.RequiredTools = []string{"go"}
	host := &progressWrites{fail: func(p execution.Progress) bool {
		return strings.HasSuffix(p.Key, ":"+string(api.EventProviderResponse))
	}}
	ctx := execution.WithProgressWriter(t.Context(), host.write)
	res, err := newTestAdapter(t, cfg).Execute(ctx, req)
	te := terminationOf(t, err)
	if te.Termination.Cause != api.CauseRecordingFailed || res.Failure.Classification != execution.FailureExecutionIncomplete {
		t.Errorf("termination %+v result %+v", te.Termination, res)
	}
	if run.count() != 0 || len(provider.Requests()) != 1 {
		t.Errorf("side effects after the progress failure: %d commands, %d provider calls", run.count(), len(provider.Requests()))
	}
}

// A failed closing write comes after settlement, so it cannot stop anything;
// it makes the host result incomplete instead of disappearing.
func TestFinalProgressWriteFailureMakesTheResultIncomplete(t *testing.T) {
	host := &progressWrites{fail: func(p execution.Progress) bool { return p.Final }}
	ctx := execution.WithProgressWriter(t.Context(), host.write)
	res, err := newTestAdapter(t, testConfig(t, scripted.New(done("finished")))).Execute(ctx, testRequest(t))
	if !errors.Is(err, errProgressStore) {
		t.Errorf("error %v, want the final write's failure", err)
	}
	if res.Outcome != execution.Failed || res.Failure == nil ||
		res.Failure.Classification != execution.FailureExecutionIncomplete || res.Answer != "" {
		t.Errorf("result %+v: a success whose final progress write failed is incomplete", res)
	}
	if res.Invocation == nil || res.Invocation.TerminationCause != string(execution.OwnerProviderExited) {
		t.Errorf("invocation %+v: a recording failure after settlement must not rewrite why the provider ended", res.Invocation)
	}
	if n := len(host.writes); n == 0 || !host.writes[n-1].Final {
		t.Errorf("writes %+v end without the final write", host.writes)
	}
}

// With both surfaces installed the adapter writes each kernel event once,
// through the fallible writer only; the best-effort recorder, which would
// write the same durable row again, is never called.
func TestBothSurfacesWriteEachEventOnce(t *testing.T) {
	host := &progressWrites{}
	recorderCalls := 0
	ctx := execution.WithProgressWriter(t.Context(), host.write)
	ctx = execution.WithProgressRecorder(ctx, func(execution.Progress) { recorderCalls++ })
	cfg := testConfig(t, scripted.New(done("finished")))
	if _, err := newTestAdapter(t, cfg).Execute(ctx, testRequest(t)); err != nil {
		t.Fatal(err)
	}
	events := len(cfg.Transcript.(*transcript).kinds())
	if recorderCalls != 0 || len(host.writes) != events+1 {
		t.Errorf("%d recorder calls and %d writes for %d events: want 0 and one per event plus the final",
			recorderCalls, len(host.writes), events)
	}
}

// A result that already failed keeps the class that routes the host; the
// final write's failure joins its error rather than replacing it.
func TestFinalProgressWriteFailureKeepsAFailedClass(t *testing.T) {
	failed := execution.Result{Outcome: execution.Failed, Failure: &execution.Failure{Classification: execution.FailureProviderRateLimited}}
	termination := &TerminationError{}
	res, err := finalProgressFailed(failed, termination, errProgressStore)
	if res.Failure.Classification != execution.FailureProviderRateLimited {
		t.Errorf("class %q, want the routing class kept", res.Failure.Classification)
	}
	var te *TerminationError
	if !errors.As(err, &te) || !errors.Is(err, errProgressStore) {
		t.Errorf("error %v must carry both the termination and the write failure", err)
	}
}

// Without a fallible writer the adapter behaves as before: a best-effort
// recorder gets every projection and nothing fails, and with no recorder at
// all the execution still completes.
func TestNoProgressWriterIsBestEffort(t *testing.T) {
	var recorded []execution.Progress
	for name, ctx := range map[string]context.Context{
		"recorder only": execution.WithProgressRecorder(t.Context(), func(p execution.Progress) { recorded = append(recorded, p) }),
		"neither":       t.Context(),
	} {
		res, err := newTestAdapter(t, testConfig(t, scripted.New(done("finished")))).Execute(ctx, testRequest(t))
		if err != nil || res.Outcome != execution.Succeeded {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
	}
	if len(recorded) == 0 || !recorded[len(recorded)-1].Final {
		t.Errorf("recorder got %+v, want the projection ending in a final write", recorded)
	}
}

// A healthy progress write slower than the limit is host time, never the
// provider's silence, exactly like a slow transcript write.
func TestSlowProgressWriteIsNotInactivity(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	slow := func(execution.Progress) error { close(entered); <-release; return nil }
	fc := newFakeClock()
	s := newAttempt(execution.AttemptRef{RunID: "r", OperationID: "o", Attempt: 1}, &transcript{}, slow, fc.clock())
	w := newWorkers()
	kctx, cancel := context.WithCancelCause(context.Background())
	supervise(w, context.Background(), cancel, testLimit, s)
	t.Cleanup(func() { w.stop(); cancel(nil) })
	first := fc.armed(t)
	written := make(chan error, 1)
	go func() {
		written <- s.recordEvent(context.Background(), api.Event{Seq: 1, Kind: api.EventProviderRequest})
	}()
	<-entered
	fc.advance(3 * testLimit)
	first.fire <- time.Time{}
	during := fc.armed(t)
	assertLive(t, kctx, "a progress write in flight for 3x the limit")
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	during.fire <- time.Time{}
	fc.armed(t)
	assertLive(t, kctx, "right after a slow progress write completed")
}
