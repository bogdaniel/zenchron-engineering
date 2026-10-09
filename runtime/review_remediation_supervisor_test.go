package runtime

// #474 B3: the automatic trigger is wired into the supervisor's own
// existing per-run tick (driveOne), not a second scheduler. These tests
// exercise that wiring directly - ReviewTrigger's own composition (the real
// ReviewPort, the real admission gate) is already proven end to end by
// TestReviewRemediationEndToEndH1BlockH2Approve.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// newReviewTriggerSupervisor builds a minimal supervisor over one real run,
// identical in shape to supervisorFixture, with the one field that test
// does not set: ReviewTrigger.
func newReviewTriggerSupervisor(t *testing.T, fixture *phase8Fixture, trigger func(context.Context, *EngineeringRuntime, EngineeringRun) error) *Supervisor {
	t.Helper()
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:          OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:      []GitHubRepo{repo},
		MaxConcurrentRuns: 2,
		PollInterval:      time.Minute,
		Agents:            supervisorRegistry(t),
		Runtime:           func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) { return fixture.runtime, nil },
		ReviewTrigger:     trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

// TestReviewTriggerRunsOncePerTickPerRun proves the "repeated tick" half of
// B3: a nil ReviewTrigger is the pre-#474 default (never called, matching
// Discovery/AgentProber's own nil-disabled convention), and a non-nil one
// runs exactly once per run per Tick, given the run's own resolved engine -
// not a copy, not a fresh one built from scratch.
func TestReviewTriggerRunsOncePerTickPerRun(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	var mu sync.Mutex
	var calls int
	supervisor := newReviewTriggerSupervisor(t, fixture, func(_ context.Context, engine *EngineeringRuntime, run EngineeringRun) error {
		mu.Lock()
		calls++
		mu.Unlock()
		if run.ID != runID {
			t.Fatalf("ReviewTrigger called for an unexpected run %q, want %q", run.ID, runID)
		}
		if engine != fixture.runtime {
			t.Fatal("ReviewTrigger was not given the run's own resolved engine")
		}
		return nil
	})

	for i := 0; i < 2; i++ {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("ReviewTrigger ran %d times across 2 ticks of 1 run, want 2", got)
	}
}

// TestReviewTriggerErrorIsReportedSeparatelyFromDriveFailure is
// FeedbackError's own regression (TestFeedbackFailureIsReportedRatherThan-
// LookingLikeSilence), for #474's automatic progression: a failure reaching
// GitHub for the review trigger must be visible, and must never abort the
// SAME pass's ordinary Reconcile for that run.
func TestReviewTriggerErrorIsReportedSeparatelyFromDriveFailure(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	supervisor := newReviewTriggerSupervisor(t, fixture, func(context.Context, *EngineeringRuntime, EngineeringRun) error {
		return errors.New("github unavailable")
	})

	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, driven := range report.Driven {
		if driven.RunID != runID {
			continue
		}
		found = true
		if driven.ReviewTriggerError == "" {
			t.Fatal("a review trigger failure was not reported")
		}
		if driven.Error != "" {
			t.Fatalf("the review trigger's failure also aborted this pass's ordinary drive: %q", driven.Error)
		}
	}
	if !found {
		t.Fatalf("run %q was never driven this tick: %#v", runID, report.Driven)
	}
}

// TestReviewTriggerSurvivesSupervisorRestart is B3's "restart" regression:
// nothing about the trigger's own correctness may depend on in-process
// supervisor state, since a crash loses exactly that. A second, entirely
// independent *Supervisor - simulating the process that replaces a crashed
// one - still runs the trigger normally against the SAME durable store.
func TestReviewTriggerSurvivesSupervisorRestart(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.start()

	var firstCalls, secondCalls int
	first := newReviewTriggerSupervisor(t, fixture, func(context.Context, *EngineeringRuntime, EngineeringRun) error {
		firstCalls++
		return nil
	})
	if _, err := first.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 {
		t.Fatalf("the first supervisor's ReviewTrigger ran %d times, want 1", firstCalls)
	}

	// A brand-new Supervisor value, sharing no Go state with `first` beyond
	// the same durable store - the restart this test is named for.
	second := newReviewTriggerSupervisor(t, fixture, func(context.Context, *EngineeringRuntime, EngineeringRun) error {
		secondCalls++
		return nil
	})
	if _, err := second.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondCalls != 1 {
		t.Fatalf("the restarted supervisor's ReviewTrigger ran %d times, want 1", secondCalls)
	}
}

// TestReviewTriggerRespectsMaxConcurrentVerifications is the independent
// review's required follow-up to B3: RunIndependentReview invokes a full
// reviewer provider bound to no run's own operation row, so it sits outside
// the scheduler's durable AcquireOperation capacity accounting entirely.
// driveOne drives each run in its own goroutine, so without a bound as many
// reviewer invocations could run at once as MaxConcurrentRuns allows,
// uncounted against any operator ceiling. This proves the bound: two runs,
// MaxConcurrentRuns=2, MaxConcurrentVerifications=1 - the second run's
// trigger is skipped (not queued, not retried, not reported as an error)
// for the entire pass while the first holds the only slot, and the observed
// concurrent-call count never exceeds the ceiling.
func TestReviewTriggerRespectsMaxConcurrentVerifications(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.start()
	fixture.issue = phase8Issue + 1
	fixture.forge.Issues[fixture.issue] = GitHubIssue{
		Number: fixture.issue, URL: "https://github.com/acme/repo/issues/99",
		Title: "second", Body: "second body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	fixture.start()

	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var inFlight, maxInFlight, calls int
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	trigger := func(context.Context, *EngineeringRuntime, EngineeringRun) error {
		mu.Lock()
		inFlight++
		calls++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:                   OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:               []GitHubRepo{repo},
		MaxConcurrentRuns:          2,
		PollInterval:               time.Minute,
		Agents:                     supervisorRegistry(t),
		Runtime:                    func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) { return fixture.runtime, nil },
		ReviewTrigger:              trigger,
		MaxConcurrentVerifications: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	reportCh := make(chan SupervisorReport, 1)
	go func() {
		report, tickErr := supervisor.Tick(context.Background())
		if tickErr != nil {
			t.Error(tickErr)
		}
		reportCh <- report
	}()

	<-entered
	// The second run's own goroutine reaches the non-blocking select within
	// microseconds of the first entering; this bounded wait gives it that
	// time before the assertion below, never a retry-until poll.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	callsWhileHeld := calls
	mu.Unlock()
	if callsWhileHeld != 1 {
		t.Fatalf("expected exactly 1 ReviewTrigger call while the only verification slot is held, got %d", callsWhileHeld)
	}
	close(release)
	report := <-reportCh

	mu.Lock()
	finalCalls, finalMax := calls, maxInFlight
	mu.Unlock()
	if finalMax > 1 {
		t.Fatalf("MaxConcurrentVerifications=1 was exceeded: observed %d concurrent ReviewTrigger calls", finalMax)
	}
	if finalCalls != 1 {
		t.Fatalf("expected the second run's trigger to be skipped this pass (never queued or retried within it), got %d total calls", finalCalls)
	}
	for _, driven := range report.Driven {
		if driven.ReviewTriggerError != "" {
			t.Fatalf("a skipped review trigger must never be reported as an error: run %q: %q", driven.RunID, driven.ReviewTriggerError)
		}
	}
}
