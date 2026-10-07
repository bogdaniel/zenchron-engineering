package agentkernel

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

func TestCompletedExecutionIsAnObservationNotAcceptance(t *testing.T) {
	var progress []execution.Progress
	ctx := execution.WithProgressRecorder(t.Context(), func(p execution.Progress) { progress = append(progress, p) })
	cfg := testConfig(t, scripted.New(done("finished")))
	res, err := newTestAdapter(t, cfg).Execute(ctx, testRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != execution.Succeeded || res.Failure != nil || res.Answer != "finished" || !res.Executed || res.Attempt != 1 {
		t.Errorf("result %+v", res)
	}
	if res.Tokens == nil || *res.Tokens != 120 || res.CostMicros != nil {
		t.Errorf("usage tokens %v cost %v: reported tokens are observed, unpriced cost stays unknown", res.Tokens, res.CostMicros)
	}
	if res.Invocation == nil || res.Invocation.TerminationCause != string(execution.OwnerProviderExited) || res.Invocation.AgentID != "kernel-agent" {
		t.Errorf("invocation %+v", res.Invocation)
	}
	if len(progress) == 0 || !progress[len(progress)-1].Final {
		t.Errorf("progress %+v: events project to the host recorder and end with a final write", progress)
	}
	if kinds := cfg.Transcript.(*transcript).kinds(); !slices.Contains(kinds, api.EventSettled) {
		t.Errorf("transcript %v lacks the settlement", kinds)
	}
}

// Operator stop and controller shutdown reach the kernel as its matching
// provenance and come back as the host's own owner and class.
func TestHostCancellationKeepsTheHostOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		cause      error
		provenance api.CancellationProvenance
		outcome    execution.Outcome
		class      execution.FailureClass
		owner      execution.TerminationOwner
	}{
		"operator stop":       {execution.ErrRunStopped, api.CancelOperatorStop, execution.Cancelled, execution.FailureRunCancelled, execution.OwnerOperatorStop},
		"controller shutdown": {context.Canceled, api.CancelControllerShutdown, execution.Cancelled, execution.FailureControllerShutdown, execution.OwnerControllerShutdown},
	} {
		entered := make(chan struct{})
		ctx, cancel := context.WithCancelCause(t.Context())
		a := newTestAdapter(t, testConfig(t, scripted.New(scripted.Step{Block: true, Entered: entered})))
		go func() { <-entered; cancel(tc.cause) }()
		res, err := a.Execute(ctx, testRequest(t))
		te := terminationOf(t, err)
		if te.Termination.Outcome != api.OutcomeCancelled || te.Termination.Cancellation != tc.provenance {
			t.Errorf("%s: kernel termination %+v, want cancellation %q", name, te.Termination, tc.provenance)
		}
		if res.Outcome != tc.outcome || res.Failure.Classification != tc.class ||
			res.Invocation == nil || res.Invocation.TerminationCause != string(tc.owner) {
			t.Errorf("%s: result %+v invocation %+v", name, res, res.Invocation)
		}
	}
}

// A cancellation that arrives after the provider answered does not rewrite
// the observed outcome or claim the termination.
func TestLateCancellationDoesNotRewriteAnObservedOutcome(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	step := done("finished")
	step.Before = func() { cancel(execution.ErrRunStopped) }
	res, err := newTestAdapter(t, testConfig(t, scripted.New(step))).Execute(ctx, testRequest(t))
	if err != nil || res.Outcome != execution.Succeeded || res.Invocation.TerminationCause != string(execution.OwnerProviderExited) {
		t.Errorf("late stop rewrote the outcome: %+v, %v", res, err)
	}
}

// The host's inactivity bound has no kernel equivalent; the adapter enforces
// it from kernel progress and reports the existing class and owner.
func TestInactivityIsEnforcedByTheAdapter(t *testing.T) {
	req := testRequest(t)
	req.Budgets.InactivityLimit = 50 * time.Millisecond
	res, err := newTestAdapter(t, testConfig(t, scripted.New(scripted.Step{Block: true}))).Execute(t.Context(), req)
	te := terminationOf(t, err)
	if te.Termination.Outcome != api.OutcomeCancelled || te.Termination.Cancellation != api.CancelUnknown {
		t.Errorf("kernel termination %+v", te.Termination)
	}
	if res.Outcome != execution.Failed || res.Failure.Classification != execution.FailureProviderNoProgress ||
		res.Invocation.TerminationCause != string(execution.OwnerInactivity) || res.Invocation.InactivityLimit != req.Budgets.InactivityLimit {
		t.Errorf("result %+v invocation %+v", res, res.Invocation)
	}
}

// Progress renews the inactivity bound: a run whose every step is shorter
// than the bound completes even when the whole run is longer.
func TestProgressRenewsTheInactivityBound(t *testing.T) {
	cfg := testConfig(t, nil)
	pause := func(s scripted.Step) scripted.Step {
		s.Before = func() { time.Sleep(60 * time.Millisecond) }
		return s
	}
	cfg.Provider = scripted.New(pause(toolUse("c1", "read_file", `{"path":"missing.txt"}`)), pause(done("ok")))
	req := testRequest(t)
	req.Budgets.InactivityLimit = 100 * time.Millisecond
	if res, err := newTestAdapter(t, cfg).Execute(t.Context(), req); err != nil || res.Outcome != execution.Succeeded {
		t.Errorf("result %+v, %v", res, err)
	}
}

// A transcript write failure settles recording_failed and stops every
// further side effect: the granted command never runs.
func TestRecordingFailureStopsFurtherSideEffects(t *testing.T) {
	provider := scripted.New(toolUse("c1", "run_command", `{"command":"go-test"}`), done("never"))
	cfg := testConfig(t, provider)
	cfg.Transcript = &transcript{failOn: api.EventProviderResponse}
	run := &runner{}
	cfg.Commands, cfg.Catalogue = run, []CommandSpec{{Name: "go-test", Argv: []string{"go", "test"}, Timeout: time.Minute}}
	req := testRequest(t)
	req.RequiredTools = []string{"go"}
	res, err := newTestAdapter(t, cfg).Execute(t.Context(), req)
	te := terminationOf(t, err)
	if te.Termination.Cause != api.CauseRecordingFailed || res.Failure.Classification != execution.FailureExecutionIncomplete {
		t.Errorf("termination %+v result %+v", te.Termination, res)
	}
	if run.count() != 0 || len(provider.Requests()) != 1 {
		t.Errorf("side effects after the recording failure: %d commands, %d provider calls", run.count(), len(provider.Requests()))
	}
}

// A command whose outcome is unknown surfaces as an uncertain side effect
// the host must reconcile, never as nothing changed.
func TestUncertainMutationIsSurfaced(t *testing.T) {
	entered := make(chan struct{})
	cfg := testConfig(t, scripted.New(toolUse("c1", "run_command", `{"command":"go-test"}`), done("never")))
	run := &runner{block: true, entered: entered}
	cfg.Commands, cfg.Catalogue = run, []CommandSpec{{Name: "go-test", Argv: []string{"go", "test"}, Timeout: time.Minute}}
	req := testRequest(t)
	req.RequiredTools = []string{"go"}
	req.ScratchDir = realDir(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	go func() { <-entered; cancel(execution.ErrRunStopped) }()
	res, err := newTestAdapter(t, cfg).Execute(ctx, req)
	te := terminationOf(t, err)
	if !te.UncertainSideEffect || !strings.Contains(err.Error(), "reconciled") || !res.Executed || res.Invocation == nil {
		t.Errorf("uncertain mutation hidden: %v, result %+v", err, res)
	}
	if got := run.calls[0]; got.Attempt != req.AttemptRef() || got.Dir != req.CandidateDir || got.ScratchDir != req.ScratchDir {
		t.Errorf("command not bound to the invocation: %+v", got)
	}
}

// The same host attempt presented again is refused by the durable admission
// store, before any provider call, and nothing reads as executed.
func TestReenteredAttemptIsRefused(t *testing.T) {
	provider := scripted.New(done("first"), done("second"))
	cfg := testConfig(t, provider)
	req := testRequest(t)
	if _, err := newTestAdapter(t, cfg).Execute(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	// A second adapter on the same state dir: a restarted controller.
	res, err := newTestAdapter(t, cfg).Execute(t.Context(), req)
	te := terminationOf(t, err)
	if te.Termination.Cause != api.CauseInvalidRequest || !strings.Contains(te.Termination.Detail, "already admitted") {
		t.Errorf("re-entry not refused: %+v", te.Termination)
	}
	if res.Executed || res.Invocation != nil || len(provider.Requests()) != 1 {
		t.Errorf("refused re-entry reads as executed: %+v, %d provider calls", res, len(provider.Requests()))
	}
}

// Two handles admitting the same attempt at once: exactly one runs.
func TestConcurrentAdmissionAdmitsOne(t *testing.T) {
	stateDir := realDir(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	first := done("first")
	first.Entered, first.Before = entered, func() { <-release }
	adapterFor := func(p api.Provider) *Adapter {
		cfg := testConfig(t, p)
		cfg.StateDir = stateDir
		return newTestAdapter(t, cfg)
	}
	req := testRequest(t)
	var wg sync.WaitGroup
	var firstErr error
	wg.Go(func() { _, firstErr = adapterFor(scripted.New(first)).Execute(t.Context(), req) })
	<-entered
	second := scripted.New(done("second"))
	_, err := adapterFor(second).Execute(t.Context(), req)
	close(release)
	wg.Wait()
	if firstErr != nil {
		t.Errorf("first handle: %v", firstErr)
	}
	if te := terminationOf(t, err); te.Termination.Cause != api.CauseInvalidRequest || len(second.Requests()) != 0 {
		t.Errorf("second handle admitted beside the first: %+v", te.Termination)
	}
}

// Contradiction evidence for #518: the host re-anchors each retry's deadline
// at its lease, so a retry after a wait carries a later deadline, and the
// kernel refuses a later deadline within one execution as a widening.
func TestRetryWithALaterDeadlineIsRefusedByTheKernelEnvelope(t *testing.T) {
	cfg := testConfig(t, scripted.New(done("first"), done("second")))
	first := testRequest(t)
	first.Budgets.WallLimit = 10 * time.Second
	if _, err := newTestAdapter(t, cfg).Execute(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	retry := first
	retry.Attempt = 2
	retry.Budgets.WallLimit = 20 * time.Second
	_, err := newTestAdapter(t, cfg).Execute(t.Context(), retry)
	if te := terminationOf(t, err); !strings.Contains(te.Termination.Detail, "budget widens deadline") {
		t.Errorf("termination %+v", te.Termination)
	}
}

func TestNoWorkerOutlivesExecute(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Commands, cfg.Catalogue = &runner{}, []CommandSpec{{Name: "go-test", Argv: []string{"go", "test"}, Timeout: time.Minute}}
	cfg.Provider = scripted.New(toolUse("c1", "run_command", `{"command":"go-test"}`), done("ok"))
	a := newTestAdapter(t, cfg)
	req := testRequest(t)
	req.RequiredTools = []string{"go"}
	req.Budgets.InactivityLimit = time.Minute
	before := runtime.NumGoroutine()
	if _, err := a.Execute(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	// Joined goroutines may still be returning; poll briefly, never sleep blind.
	for range 100 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("%d goroutines after Execute, %d before", runtime.NumGoroutine(), before)
}

func TestNotStartedWhenTheHostContextAlreadyEnded(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(execution.ErrRunStopped)
	provider := scripted.New(done("never"))
	res, err := newTestAdapter(t, testConfig(t, provider)).Execute(ctx, testRequest(t))
	var ns *execution.NotStartedError
	if !errors.As(err, &ns) || res.Failure.Classification != execution.FailureRunCancelled || len(provider.Requests()) != 0 {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestMissingToolsProbesTheHostBoundary(t *testing.T) {
	cfg := testConfig(t, scripted.New())
	if got := newTestAdapter(t, cfg).MissingTools(t.Context(), []string{"go"}); !slices.Equal(got, []string{"go"}) {
		t.Errorf("without a runner: missing %v", got)
	}
	run := &runner{}
	cfg.Commands, cfg.Catalogue = run, []CommandSpec{{Name: "go-test", Argv: []string{"go", "test"}, Timeout: time.Minute}}
	if got := newTestAdapter(t, cfg).MissingTools(t.Context(), []string{"go", "npm"}); !slices.Equal(got, []string{"npm"}) {
		t.Errorf("missing %v, want npm (not catalogued)", got)
	}
	run.exit = 1
	if got := newTestAdapter(t, cfg).MissingTools(t.Context(), []string{"go"}); !slices.Equal(got, []string{"go"}) {
		t.Errorf("missing %v, want go (unresolved in the runner)", got)
	}
}

func TestConfigRefusesImplicitBounds(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"zero limit":         func(c *Config) { c.Limits.MaxToolCalls = 0 },
		"relative state dir": func(c *Config) { c.StateDir = "state" },
		"no transcript":      func(c *Config) { c.Transcript = nil },
		"catalogue without runner": func(c *Config) {
			c.Catalogue = []CommandSpec{{Name: "x", Argv: []string{"x"}, Timeout: time.Second}}
		},
		"sub-second command": func(c *Config) {
			c.Commands, c.Catalogue = &runner{}, []CommandSpec{{Name: "x", Argv: []string{"x"}, Timeout: time.Millisecond}}
		},
	} {
		cfg := testConfig(t, scripted.New())
		edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
