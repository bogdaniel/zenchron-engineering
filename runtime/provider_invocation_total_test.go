package runtime

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A plan's remaining provider invocations are a TOTAL for the run.
//
// They were carried in MaxExecutionAttempts, which #63 defines as retries of
// ONE execution binding. A continuation is a different binding with its own
// attempt allowance, so a run with two invocations left could spend two on its
// initial binding and two more on a continuation: the aggregate ceiling was
// enforced per binding, which is not enforcement of an aggregate.
//
// The fixture is built from durable operations and a projection rather than by
// driving a provider four times: what is under test is how the runtime reads
// its own state.
func totalInvocationState(t *testing.T, total, attemptsSpent int, head string) *runState {
	t.Helper()
	configured := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 2, MaxAssuranceAttempts: 2, MaxRemediationAttempts: 2}
	persisted := configured
	persisted.MaxExecutionContinuations = 8
	persisted.MaxProviderInvocations = total
	operations := map[string]RunOperation{
		"op-candidate-create": succeededCandidateCreate(),
		"op-initial":          initialOperation(2, OperationFailed),
	}
	if attemptsSpent > 2 {
		operations["op-continuation"] = continuationOperation("checkpoint-a", attemptsSpent-2, 2, OperationFailed)
	}
	return &runState{
		rt: &EngineeringRuntime{deps: Dependencies{Budgets: configured, Clock: newSteppingClock()}},
		run: EngineeringRun{
			ID: "run-total", SchemaVersion: SchemaVersion,
			Base:      Ref{ID: "main", Revision: "base-revision"},
			Contract:  Ref{ID: "contract-total", Revision: "1"},
			Budgets:   &persisted,
			CreatedAt: time.Unix(1_800_000_000, 0).UTC(),
		},
		snapshot: RunSnapshot{EngineeringRun: EngineeringRun{Disposition: Active}, Operations: operations},
		sources:  []sourceRecord{{Repository: "acme/repo", Issue: 54, BaseRevision: "base-revision", State: "open"}},
		projection: RunProjection{
			Contract:          Ref{ID: "contract-total", Revision: "1"},
			BaseRevision:      "base-revision",
			CandidateRevision: head,
			Attempts:          map[string]int{OpExecutionInvoke: attemptsSpent},
		},
	}
}

func TestTheRunTotalBoundsInvocationsAcrossDistinctBindings(t *testing.T) {
	// Two attempts on the initial binding, one on a continuation: three
	// invocations were made, and the run was created with two.
	spent := totalInvocationState(t, 2, 3, "checkpoint-a")

	// The defect, stated: every individual binding is inside the per-binding
	// retry allowance, so the old ceiling sees nothing wrong.
	for _, operation := range spent.snapshot.Operations {
		if operation.Kind == OpExecutionInvoke && operation.Attempt > spent.budgets().MaxExecutionAttempts {
			t.Fatalf("the fixture exceeds a per-binding allowance and no longer isolates the total: %#v", operation)
		}
	}
	if spent.continuationCeilingReached() {
		t.Fatal("the fixture is bounded by continuation depth and no longer isolates the total")
	}

	if !spent.providerInvocationCeilingReached() {
		t.Fatal("three invocations under a total of two were permitted")
	}
	if disposition, reason := spent.conditions(); disposition != Failed || reason != "run_provider_invocations_exhausted" {
		t.Fatalf("terminal decision = %q/%q, want failed/run_provider_invocations_exhausted", disposition, reason)
	}

	// Below the total, nothing is refused.
	within := totalInvocationState(t, 4, 3, "checkpoint-a")
	if within.providerInvocationCeilingReached() {
		t.Fatal("a run three invocations into a total of four was refused")
	}
	if disposition, reason := within.conditions(); disposition == Failed && reason == "run_provider_invocations_exhausted" {
		t.Fatal("a run inside its total was terminated")
	}

	// A run persisted before the bound existed is unbounded by it, exactly as
	// it was when it ran.
	legacy := totalInvocationState(t, 0, 9, "checkpoint-a")
	if legacy.providerInvocationCeilingReached() {
		t.Fatal("a run that persisted no total was judged by one")
	}
}

// The plan's headroom reaches the run as a total, not as a retry allowance.
func TestPlanHeadroomNarrowsTheRunTotal(t *testing.T) {
	stage := headroom{invocations: 2}.tighten(domain.StageBudget{MaxExecutionAttempts: 5})
	if stage.MaxProviderInvocations != 2 {
		t.Fatalf("stage total = %d, want 2", stage.MaxProviderInvocations)
	}
	if stage.MaxExecutionAttempts != 5 {
		t.Fatalf("the plan headroom overwrote the per-binding retry allowance: %#v", stage)
	}
	budgets := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 5}.tightenedBy(stage)
	if budgets.MaxProviderInvocations != 2 {
		t.Fatalf("run total = %d, want 2", budgets.MaxProviderInvocations)
	}
	// It only ever narrows.
	wider := RunBudgets{MaxProviderInvocations: 1}.tightenedBy(domain.StageBudget{MaxProviderInvocations: 99})
	if wider.MaxProviderInvocations != 1 {
		t.Fatalf("a stage widened a run total to %d", wider.MaxProviderInvocations)
	}
}
