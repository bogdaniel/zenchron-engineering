package runtime

// #474 R6: the automatic trigger's independent review must never run
// under an unattended, unbounded provider budget - the exact #238 shape a
// hanging or silent reviewer would otherwise reproduce, now holding a
// review_verification_claim slot indefinitely too.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// TestReviewBudgetIsAlwaysFinite mirrors
// TestAnUnbudgetedPlanningInvocationStillHasADeadline (planningWallLimit's
// own regression): an engine constructed with NO configured budget - a
// test, an embedder, or any other caller that never went through
// configuration - still resolves to a finite wall and inactivity bound,
// through the SAME RunBudgets.defaults() every other unattended invocation
// already relies on. Zero would mean the reviewer may stall forever,
// holding its verification-capacity claim (R5) for as long as it wants.
func TestReviewBudgetIsAlwaysFinite(t *testing.T) {
	engine := &EngineeringRuntime{deps: Dependencies{}}
	budget := engine.ReviewBudget()
	if budget.WallLimit <= 0 {
		t.Fatalf("ReviewBudget().WallLimit = %v with no configured budget, want a finite positive bound", budget.WallLimit)
	}
	if budget.InactivityLimit <= 0 {
		t.Fatalf("ReviewBudget().InactivityLimit = %v with no configured budget, want a finite positive bound", budget.InactivityLimit)
	}
}

// TestReviewBudgetUsesTheOperatorConfiguredEnvelope proves ReviewBudget is
// not a silently-invented default when the operator DID configure one: it
// reads the SAME RunBudgets every producer and planning invocation is
// bound by, read fresh on every call - "still-finite after restart" is not
// a separate code path, since nothing here is cached across one.
func TestReviewBudgetUsesTheOperatorConfiguredEnvelope(t *testing.T) {
	engine := &EngineeringRuntime{deps: Dependencies{
		Budgets: RunBudgets{WallLimit: 45 * time.Minute, ProviderInactivityLimit: 10 * time.Minute},
	}}
	budget := engine.ReviewBudget()
	if budget.WallLimit != 45*time.Minute {
		t.Fatalf("ReviewBudget().WallLimit = %v, want the operator's configured %v", budget.WallLimit, 45*time.Minute)
	}
	if budget.InactivityLimit != 10*time.Minute {
		t.Fatalf("ReviewBudget().InactivityLimit = %v, want the operator's configured %v", budget.InactivityLimit, 10*time.Minute)
	}
}

// stubReviewPort is the minimal ReviewPort this file's release-visibility
// test needs: no decision has ever been reached and none is needed, so
// ReconcileReviewRemediation returns a clean no-op immediately, exercising
// ReconcileReviewRemediationForRun's own claim/release wrapping in
// isolation from #233's review machinery.
type stubReviewPort struct{}

func (stubReviewPort) RequestReview(context.Context, GitHubRepo, int, string) (ReviewOutcome, error) {
	return ReviewOutcome{}, nil
}
func (stubReviewPort) LatestDecision(GitHubRepo, int) (review.Decision, bool, error) {
	return review.Decision{}, false, nil
}
func (stubReviewPort) IsStale(context.Context, GitHubRepo, int) (bool, error) { return false, nil }
func (stubReviewPort) NeedsReview(context.Context, GitHubRepo, int) (bool, error) {
	return false, nil
}

// releaseFailingStore simulates a release call that fails - a transient
// store error on the DELETE itself - by shadowing only
// ReleaseReviewVerificationClaim on top of a real *SQLiteOperationStore.
type releaseFailingStore struct {
	*SQLiteOperationStore
}

func (releaseFailingStore) ReleaseReviewVerificationClaim(string) error {
	return errors.New("simulated release failure")
}

// TestReconcileReviewRemediationForRunSurfacesAReleaseFailure is R9's
// visibility requirement: a failed release of the review verification
// claim must reach the caller as this call's own error, never be silently
// discarded the way `_ = r.scheduler.releaseReviewVerificationSlot(...)`
// once did. The claim leaking here is exactly what
// claimReviewVerificationSlot's own self-heal (proven in
// TestAFailedReleaseIsRecoveredByTheNextClaimForTheSameOwnerAndRun) makes
// idempotently retryable rather than a permanent capacity loss.
func TestReconcileReviewRemediationForRunSurfacesAReleaseFailure(t *testing.T) {
	f := newAdmissionFixture(t)
	rt := &EngineeringRuntime{
		deps: Dependencies{Store: f.store, Clock: f.clock, GitHub: f.fake},
		scheduler: Scheduler{
			Store: releaseFailingStore{f.store}, Clock: f.clock, Owner: "controller-x",
			LeaseDuration: time.Minute, Liveness: neverAlive(),
			MaxConcurrentRuns: 10, MaxConcurrentVerifications: 10,
		},
	}

	_, _, err := rt.ReconcileReviewRemediationForRun(context.Background(), f.runID, stubReviewPort{}, "reviewer-1")
	if err == nil {
		t.Fatal("expected the simulated release failure to be reported as this call's own error")
	}
	if !strings.Contains(err.Error(), "releasing the review verification claim") {
		t.Fatalf("error %q does not attribute the failure to releasing the review verification claim", err.Error())
	}
}
