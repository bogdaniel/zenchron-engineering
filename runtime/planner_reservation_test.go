package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A planner invocation is spent when it BEGINS, not when it returns.
//
// The consumption event was appended after the provider call returned. That
// call crosses a real provider account boundary and takes minutes, so a crash
// inside it lost the fact that it had happened: neither the aggregate nor the
// stage moved, a restart found an apparently unspent ceiling, and a profile
// narrowing this stage to one planning attempt was bypassed for exactly as
// long as the window lasted.
func TestAPlannerInvocationIsReservedBeforeTheProviderRuns(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "decomposition", Kind: domain.StageAgent, Role: domain.RolePlanner,
			Objective:            "Decide how this should be split.",
			InvocationMode:       domain.InvocationModeNonMutatingPlanning,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis, domain.CapabilityRequirementsAnalysis}},
	})
	fixture.approve(t)

	calls := 0
	// The seam stands in for the crash window: the provider has run - the
	// account was charged - and the process never records a result.
	fixture.reconciler.Planner = func(context.Context, PlanDecompositionRequest) (PlannerOutput, error) {
		calls++
		durable, err := fixture.store.ReplayPlan(fixture.plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if durable.Consumed.ProviderInvocations < calls {
			t.Fatalf("the provider was entered with %d invocations recorded on call %d: the spend is not durable yet",
				durable.Consumed.ProviderInvocations, calls)
		}
		if durable.StageConsumed["decomposition"].ProviderInvocations < calls {
			t.Fatalf("the stage records %d invocations on call %d", durable.StageConsumed["decomposition"].ProviderInvocations, calls)
		}
		return PlannerOutput{}, errors.New("the provider ran and this process died before it could record anything")
	}

	fixture.reconcile(t)
	durable, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Consumed.ProviderInvocations != 1 {
		t.Fatalf("a crashed invocation left %d spent, want 1: the ceiling was reset by dying", durable.Consumed.ProviderInvocations)
	}

	// A second pass invokes again - the ceiling is not reached - and the first
	// spend is not given back, nor is either one double counted.
	fixture.reconcile(t)
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2", calls)
	}
	durable, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Consumed.ProviderInvocations != 2 {
		t.Fatalf("two invocations recorded %d, want 2", durable.Consumed.ProviderInvocations)
	}
}

// A planning invocation is never unbounded.
//
// The stage's wall bound - narrowed by the profile, narrowed again by the
// plan's remaining headroom - bounds the invocation. Where none of the three
// states one, the computed bound is zero, and zero meant no deadline at all:
// the one stage type that runs unattended against a provider was the only one
// that could run forever. The operator's configured run wall limit, which
// bounds every producer invocation, is the floor.
func TestAnUnbudgetedPlanningInvocationStillHasADeadline(t *testing.T) {
	engine := &EngineeringRuntime{deps: Dependencies{Budgets: RunBudgets{WallLimit: 42 * time.Minute}}}
	if got := engine.planningWallLimit(0); got != 42*time.Minute {
		t.Fatalf("an unbudgeted planning invocation is bounded by %s, want the configured %s", got, 42*time.Minute)
	}
	// A stated bound still wins, in both directions: it is the stage's, and a
	// stage may state less than the configured default or more.
	if got := engine.planningWallLimit(60); got != time.Minute {
		t.Fatalf("a stage stating 60 seconds was bounded by %s", got)
	}
}

// The planning wall bound narrows and never widens.
//
// A stage stating more than the operator configured was the one place a stated
// bound could raise a configured one - the asymmetry tightenedBy refuses
// everywhere else in this runtime.
func TestAStatedPlanningWallBoundOnlyNarrows(t *testing.T) {
	engine := &EngineeringRuntime{deps: Dependencies{Budgets: RunBudgets{WallLimit: 10 * time.Minute}}}
	if got := engine.planningWallLimit(20 * 60); got != 10*time.Minute {
		t.Fatalf("a stage widened the configured bound to %s", got)
	}
	if got := engine.planningWallLimit(60); got != time.Minute {
		t.Fatalf("a tighter stated bound was ignored: %s", got)
	}
}
