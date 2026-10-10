package runtime

// #474 R6: the automatic trigger's independent review must never run
// under an unattended, unbounded provider budget - the exact #238 shape a
// hanging or silent reviewer would otherwise reproduce, now holding a
// review_verification_claim slot indefinitely too.

import (
	"testing"
	"time"
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
