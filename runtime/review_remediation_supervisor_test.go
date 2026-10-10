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

// The verification-capacity bound itself (#474 R5) is now the durable
// ReconcileReviewRemediationForRun claim, proven in
// review_verification_claim_test.go against the real scheduler/store -
// not a Supervisor-local semaphore, so it is no longer testable by handing
// driveOne a fake ReviewTrigger closure that bypasses the real claim
// entirely.
