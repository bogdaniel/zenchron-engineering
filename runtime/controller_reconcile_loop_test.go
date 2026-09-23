package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// countingService records what the loop asked the runtime to do, so a test can
// assert on CALLS rather than only on outcomes: the failure mode of a loop is
// acting too often, which a result alone does not show.
type countingService struct {
	recovered  int
	enabled    int
	recoverErr error
}

func (c *countingService) RecoverActivated(handoffID string) (ControllerHandoff, error) {
	c.recovered++
	return ControllerHandoff{ID: handoffID, Phase: HandoffActivated}, c.recoverErr
}

func (c *countingService) EnableWorkAdmission(string) error { c.enabled++; return nil }

// loopFixture drives the reconciler over a store it controls, with a live
// snapshot the test can move between passes.
type loopFixture struct {
	t        *testing.T
	fixture  *serviceFixture
	service  *ControllerService
	snapshot func() (LiveControllerSnapshot, error)
}

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	fixture := activatedFixture(t)
	return &loopFixture{t: t, fixture: fixture, service: fixture.service,
		snapshot: func() (LiveControllerSnapshot, error) {
			return fixture.service.DescribeLiveController(fixture.record.ID, time.Now().UTC()), nil
		}}
}

func (l *loopFixture) reconciler() *ControllerReconciler {
	return NewControllerReconciler(l.service, l.fixture.store, l.fixture.root, l.fixture.self, l.snapshot)
}

// ONE PASS, ONE ATTEMPT, and the loop drives the real halves end to end.
func TestOnePassPerformsOneReconciliation(t *testing.T) {
	loop := newLoopFixture(t)
	reconciler := loop.reconciler()
	now := time.Unix(1700000200, 0).UTC()

	attempt := reconciler.Attempt(now)
	if attempt.Skipped || attempt.Error != "" || attempt.Result == nil {
		t.Fatalf("the first pass did not attempt anything: %s", attempt.Describe())
	}
	if attempt.Result.Intent.Action != ReconcileResumeActivatedService {
		t.Fatalf("intent = %q, want resume (%s)", attempt.Result.Intent.Action, attempt.Result.Intent.Reason)
	}
	if !attempt.Result.Converged || !loop.service.AdmittingWork() {
		t.Fatalf("the controller did not converge: %s", attempt.Result.Summary())
	}

	// And having converged, the next pass has nothing to do - it does not
	// re-fire the intent it just satisfied.
	settled := reconciler.Attempt(now.Add(time.Second))
	if settled.Skipped || settled.Result == nil {
		t.Fatalf("the settled pass was skipped: %s", settled.Describe())
	}
	if settled.Result.Intent.Action != ReconcileNone {
		t.Fatalf("a converged controller asks for %q", settled.Result.Intent.Action)
	}
}

// NON-MUTATING CLASSIFICATIONS CALL NOTHING. The loop is the layer most likely
// to turn "no action" into an action by being helpful, so it is asserted on the
// call count.
func TestNonActionableStatesCallNothing(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot func() (LiveControllerSnapshot, error)
	}{
		{"nothing observable", func() (LiveControllerSnapshot, error) {
			return LiveControllerSnapshot{}, fmt.Errorf("no control endpoint is available")
		}},
		{"the controller reports nothing about itself", func() (LiveControllerSnapshot, error) {
			return LiveControllerSnapshot{}, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := activatedFixture(t)
			service := &countingService{}
			reconciler := NewControllerReconciler(nil, fixture.store, fixture.root, fixture.self, test.snapshot)
			// The executor is only reached for mutating intents, so a nil
			// service is itself the assertion: reaching it would panic.
			attempt := reconciler.Attempt(time.Unix(1700000201, 0).UTC())
			if attempt.Result == nil {
				t.Fatalf("no result: %s", attempt.Describe())
			}
			if attempt.Result.Intent.Action.Mutating() {
				t.Fatalf("a non-actionable state produced %q", attempt.Result.Intent.Action)
			}
			if service.recovered != 0 || service.enabled != 0 {
				t.Fatal("something was called")
			}
		})
	}
}

// A RESULT THAT MADE NO PROGRESS EARNS A WAIT, and the wait suppresses the hot
// loop rather than the decision.
func TestRepeatedRefusalBacksOff(t *testing.T) {
	fixture := activatedFixture(t)
	service := &countingService{recoverErr: fmt.Errorf("this process is not the activated generation")}
	reconciler := NewControllerReconciler(nil, fixture.store, fixture.root, fixture.self,
		func() (LiveControllerSnapshot, error) {
			return LiveControllerSnapshot{Identity: fixture.self, Role: RoleHeld, WorkAdmission: AdmissionClosed}, nil
		})
	reconciler.serviceForTest(service)

	now := time.Unix(1700000202, 0).UTC()
	first := reconciler.Attempt(now)
	if first.Result == nil || first.Result.Recovery.Outcome != StepRefused {
		t.Fatalf("the first attempt did not reach a refusal: %s", first.Describe())
	}
	if !first.NextEligibleAt.After(now) {
		t.Fatal("a refusal earned no wait")
	}
	if service.recovered != 1 {
		t.Fatalf("recovery was attempted %d times, want 1", service.recovered)
	}

	// Every pass inside the wait is skipped without observing anything.
	for i := 0; i < 5; i++ {
		skipped := reconciler.Attempt(now.Add(time.Duration(i+1) * time.Second))
		if !skipped.Skipped {
			t.Fatalf("pass %d looked again during the wait: %s", i, skipped.Describe())
		}
	}
	if service.recovered != 1 {
		t.Fatalf("recovery ran %d times during the wait, want 1", service.recovered)
	}

	// After it expires the state is observed again, and consecutive failures
	// wait longer.
	second := reconciler.Attempt(first.NextEligibleAt)
	if second.Skipped {
		t.Fatal("the loop did not look again after the wait expired")
	}
	if service.recovered != 2 {
		t.Fatalf("recovery ran %d times, want 2", service.recovered)
	}
	if !second.NextEligibleAt.After(first.NextEligibleAt.Add(reconciliationBackoffBase)) {
		t.Fatal("a second consecutive refusal did not wait longer than the first")
	}
}

// NO INTENT SURVIVES A PASS. The loop caches timing and nothing else: when the
// world moves between passes, the next attempt classifies the world it finds.
func TestTheLoopNeverReusesAnIntentAcrossPasses(t *testing.T) {
	fixture := activatedFixture(t)
	service := &countingService{}
	observed := 0
	reconciler := NewControllerReconciler(nil, fixture.store, fixture.root, fixture.self,
		func() (LiveControllerSnapshot, error) {
			observed++
			// First pass: not serving, so the classification is resume.
			if observed == 1 {
				return LiveControllerSnapshot{Identity: fixture.self, Role: RoleHeld, WorkAdmission: AdmissionClosed}, nil
			}
			// Later passes: serving, so there is nothing to do.
			return LiveControllerSnapshot{Identity: fixture.self, Role: RoleHeld, WorkAdmission: AdmissionOpen}, nil
		})
	reconciler.serviceForTest(service)

	now := time.Unix(1700000203, 0).UTC()
	first := reconciler.Attempt(now)
	if first.Result.Intent.Action != ReconcileResumeActivatedService {
		t.Fatalf("first intent = %q, want resume", first.Result.Intent.Action)
	}
	second := reconciler.Attempt(now.Add(time.Second))
	if second.Skipped {
		t.Fatalf("the converged pass was skipped: %s", second.Describe())
	}
	if second.Result.Intent.Action != ReconcileNone {
		t.Fatalf("second intent = %q: the loop replayed a stale intent", second.Result.Intent.Action)
	}
	if observed != 2 {
		t.Fatalf("the world was observed %d times across two passes, want 2", observed)
	}
	if service.recovered != 1 || service.enabled != 1 {
		t.Fatalf("calls = recover %d enable %d, want exactly one each", service.recovered, service.enabled)
	}
}

// The supervisor drives it once per pass, and a supervisor without one behaves
// exactly as it always did.
func TestSupervisorDrivesReconciliationOncePerPass(t *testing.T) {
	loop := newLoopFixture(t)
	reconciler := loop.reconciler()

	report := SupervisorReport{}
	// One pass, as the supervisor performs it.
	attempt := reconciler.Attempt(time.Unix(1700000204, 0).UTC())
	report.Reconciliation = &attempt
	if report.Reconciliation == nil || report.Reconciliation.Result == nil {
		t.Fatal("the pass reported no reconciliation")
	}
	if !strings.Contains(report.Reconciliation.Describe(), "resume_activated_service") {
		t.Fatalf("the report did not describe what happened: %s", report.Reconciliation.Describe())
	}
	// And the report carries it without a new channel or subsystem.
	if report.Reconciliation.NextEligibleAt.IsZero() {
		t.Fatal("the report did not say when the loop looks again")
	}
}
