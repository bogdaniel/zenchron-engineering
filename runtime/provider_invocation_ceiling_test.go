package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #514 proofs: the provider-invocation ceiling refuses the next
// invocation the planner would still dispatch, never the one that already
// succeeded and is waiting for its output to be committed.

// driveCeilingRun reconciles one issue run under budgets until it settles.
func driveCeilingRun(t *testing.T, f *phase8Fixture, budgets RunBudgets) (EngineeringRun, *runState) {
	t.Helper()
	f.deps.Budgets = budgets
	f.runtime = f.newRuntime(f.deps)
	runID := f.start()
	for range 6 {
		if outcome := f.reconcile(runID); terminalDisposition(outcome.Disposition) || outcome.Disposition == Waiting {
			break
		}
	}
	run, _, err := f.store.Run(runID)
	if err != nil {
		t.Fatal(err)
	}
	return run, f.state(runID)
}

// TestTheLastPermittedInvocationIsCommitted: a run whose only permitted
// provider invocation succeeds commits that work and goes on, whether the
// ceiling is its own, a plan stage's headroom, or an orchestrated child's.
func TestTheLastPermittedInvocationIsCommitted(t *testing.T) {
	for name, tighten := range map[string]func(RunBudgets) RunBudgets{
		"run budget": func(b RunBudgets) RunBudgets { b.MaxProviderInvocations = 1; return b },
		// A plan stage's run takes its ceiling through exactly this path.
		"plan stage headroom": func(b RunBudgets) RunBudgets {
			return b.tightenedBy(domain.StageBudget{MaxProviderInvocations: 1})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPhase8Fixture(t)
			run, state := driveCeilingRun(t, f, tighten(f.deps.Budgets))
			if run.Budgets == nil || run.Budgets.MaxProviderInvocations != 1 {
				t.Fatalf("the run was not created under a ceiling of one: %+v", run.Budgets)
			}
			if run.Disposition == Failed {
				t.Fatalf("the run whose one permitted invocation succeeded failed: %s", run.Reason)
			}
			if len(f.provider.requests) != 1 || countType(state.events, EventCandidateCommitted) != 1 {
				t.Fatalf("%d invocations and %d commits, want the one invocation committed",
					len(f.provider.requests), countType(state.events, EventCandidateCommitted))
			}
			if state.projection.Assurance == nil || !state.projection.Assurance.Passed {
				t.Fatal("the committed work never reached assurance")
			}
		})
	}
	t.Run("orchestrated child", func(t *testing.T) {
		fixture := newFleetFixture(t, 10)
		fixture.deps.Budgets.MaxProviderInvocations = 1
		supervisor := fixture.supervisor()
		view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
		runID := view.Items[0].RunID
		fixture.worker.set(runID, fleetValidHandoff)
		item := itemFor(t, fixture.drive(supervisor, view.BatchID), runID)
		if item.State != orchestration.ItemCompleted {
			t.Fatalf("item %s (%s), want completed", item.State, item.Reason)
		}
		events, err := fixture.store.Events(runID)
		if err != nil {
			t.Fatal(err)
		}
		if engineering, _ := fixture.worker.counts(runID); engineering != 1 || countType(events, EventCandidateCommitted) != 1 {
			t.Fatalf("%d invocations and %d commits", engineering, countType(events, EventCandidateCommitted))
		}
	})
}

// TestAnUnsatisfiedInvocationAtTheCeilingStillStops: the ceiling is still a
// ceiling. The one permitted invocation is committed and judged; a failing
// verdict wants a remediation, which would be a second invocation, and the run
// settles run_provider_invocations_exhausted without dispatching it.
func TestAnUnsatisfiedInvocationAtTheCeilingStillStops(t *testing.T) {
	f := newPhase8Fixture(t)
	f.deps.Assurance = &FakeAssuranceProvider{Results: []AssuranceResult{
		{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: false, FailureClass: FailureCompileTest},
		{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: false, FailureClass: FailureCompileTest},
	}}
	budgets := f.deps.Budgets
	budgets.MaxProviderInvocations = 1
	run, state := driveCeilingRun(t, f, budgets)
	if run.Disposition != Failed || run.Reason != ReasonProviderInvocationsExhausted {
		t.Fatalf("run %s/%s, want failed %s", run.Disposition, run.Reason, ReasonProviderInvocationsExhausted)
	}
	if len(f.provider.requests) != 1 || countType(state.events, EventCandidateCommitted) != 1 {
		t.Fatalf("%d invocations and %d commits, want the permitted one committed and no remediation",
			len(f.provider.requests), countType(state.events, EventCandidateCommitted))
	}
}
