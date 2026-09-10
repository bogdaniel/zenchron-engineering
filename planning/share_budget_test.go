package planning

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A stage's share of the envelope is a TOTAL, not a retry allowance.
//
// The share was written into MaxExecutionAttempts, which #63 defines as retries
// of ONE execution binding. A continuation is a different binding with its own
// allowance, so a stage's continuations could each start fresh and one stage
// could legally spend the whole plan remainder instead of its share. It is the
// same misuse the aggregate seam was fixed for, one layer down.
func TestAStageShareOfTheEnvelopeIsATotal(t *testing.T) {
	stages := shareBudget([]domain.PlanStage{
		{ID: "one", Kind: domain.StageAgent, Role: domain.RoleImplementer},
		{ID: "two", Kind: domain.StageAgent, Role: domain.RoleReviewer},
		{ID: "gate", Kind: domain.StageAssuranceGate},
	}, domain.PlanBudgetEnvelope{MaxChildRuns: 2, MaxConcurrency: 2, MaxProviderInvocations: 6, MaxWallSeconds: 600})

	for _, stage := range stages {
		if stage.Kind != domain.StageAgent {
			if stage.Budget.MaxProviderInvocations != 0 {
				t.Fatalf("a gate was given an invocation budget: %#v", stage.Budget)
			}
			continue
		}
		if stage.Budget.MaxProviderInvocations != 3 {
			t.Fatalf("stage %s has a share of %d invocations, want 3", stage.ID, stage.Budget.MaxProviderInvocations)
		}
		if stage.Budget.MaxExecutionAttempts != 0 {
			t.Fatalf("stage %s carries the share as a per-binding retry ceiling: %#v", stage.ID, stage.Budget)
		}
		if stage.Budget.MaxWallSeconds != 300 {
			t.Fatalf("stage %s has %d wall seconds, want 300", stage.ID, stage.Budget.MaxWallSeconds)
		}
	}

	// It only ever narrows: a stage that already states a tighter total keeps
	// it.
	tighter := shareBudget([]domain.PlanStage{
		{ID: "one", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Budget: domain.StageBudget{MaxProviderInvocations: 1}},
	}, domain.PlanBudgetEnvelope{MaxProviderInvocations: 9})
	if tighter[0].Budget.MaxProviderInvocations != 1 {
		t.Fatalf("a stage's own tighter total was widened to %d", tighter[0].Budget.MaxProviderInvocations)
	}
}
