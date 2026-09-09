package runtime

import (
	"context"
	"errors"
	"testing"

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
