package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// remediationJourney varies the conflict -> epoch journey where a scenario needs
// the plan or the operator registry to differ from the approved defaults.
type remediationJourney struct {
	// envelope is what the plan is approved for; zero means room for the three
	// stage runs and both epochs.
	envelope domain.PlanBudgetEnvelope
	// epochSupervisor replaces the supervisor that activates the epochs.
	epochSupervisor func(*testing.T, *fleetFixture) *Supervisor
	// refused means the epoch must NOT be admitted.
	refused bool
}

func storedRuns(t *testing.T, fixture *phase8Fixture) []EngineeringRun {
	t.Helper()
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// An epoch is a child run of the plan that approved the stage. A plan approved
// for exactly its own stage runs has nothing left to grant one.
func TestARemediationEpochBeyondThePlansChildRunCeilingIsRefusedAndCreatesNoRun(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{refused: true, envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 3, MaxConcurrency: 3, MaxProviderInvocations: 12}},
		func(t *testing.T, fleet *fleetFixture, envelope domain.PlanBudgetEnvelope) *planRunFixture {
			return newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
		})
}

// The graph's remediation worker is whatever the registry says now; the stage was
// approved against one worker. The same id re-pointed at another provider is a
// replacement the plan never granted.
func TestARemediationEpochUnderAnotherProviderIsRefusedAndCreatesNoRun(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{refused: true, epochSupervisor: func(t *testing.T, fleet *fleetFixture) *Supervisor {
			registry, err := OperatorConfig{
				Agents: map[string]AgentConfig{
					"codex":  {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)},
					"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)},
				},
				DefaultAgent: "codex",
			}.AgentRegistry()
			if err != nil {
				t.Fatal(err)
			}
			return fleet.supervisorOver(registry)
		}},
		func(t *testing.T, fleet *fleetFixture, envelope domain.PlanBudgetEnvelope) *planRunFixture {
			return newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
		})
}

// assertEpochRefused proves a refused epoch left the plan exactly as it was: no
// epoch row, no new run, no charge, and the stage still names its own run.
func assertEpochRefused(t *testing.T, fleet *fleetFixture, plan *planRunFixture, graphID, unitID string, runsBefore int) {
	t.Helper()
	epochs, err := fleet.store.WorkUnitRemediationEpochs(graphID, unitID)
	if err != nil || len(epochs) != 0 {
		t.Fatalf("a refused epoch was recorded for %s: %+v (%v)", unitID, epochs, err)
	}
	if after := len(storedRuns(t, fleet.phase8Fixture)); after != runsBefore {
		t.Fatalf("a refused epoch created a run: %d runs before, %d after", runsBefore, after)
	}
	snapshot, err := fleet.store.ReplayPlan(plan.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns != 3 {
		t.Fatalf("the plan was charged %d child runs; the three stage runs are the only ones it granted", snapshot.Consumed.ChildRuns)
	}
}

// assertStageFollowsEpoch proves the three views of a remediated stage agree -
// the reconciler's, the operator's, and one after a restart - while the journal
// still holds the original run, and that the epoch was charged exactly once.
func assertStageFollowsEpoch(t *testing.T, fleet *fleetFixture, plan *planRunFixture, graphID, unitID, originalRunID, epochRunID string) {
	t.Helper()
	check := func(label string, service PlanService) {
		t.Helper()
		view, err := service.View(plan.plan.ID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got := view.Snapshot.Stages[unitID].RunID; got != epochRunID {
			t.Fatalf("%s: stage %s names run %s, want the epoch run %s", label, unitID, got, epochRunID)
		}
		// three stage runs, the blamed producer's epoch and the integrator's own
		if got := view.Consumed.ChildRuns; got != 5 {
			t.Fatalf("%s: the plan was charged %d child runs, want 5", label, got)
		}
		if got := view.Snapshot.RunConsumed[epochRunID].ChildRuns; got != 1 {
			t.Fatalf("%s: epoch run %s was charged %d times, want once", label, epochRunID, got)
		}
	}
	check("before restart", plan.service)

	events, err := fleet.store.PlanEvents(plan.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	startedOriginal := false
	for _, event := range events {
		if event.Type != EventPlanRunStarted {
			continue
		}
		var payload PlanRunStartedPayload
		if err := decodeJSON(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		startedOriginal = startedOriginal || (payload.StageID == unitID && payload.RunID == originalRunID)
	}
	if !startedOriginal {
		t.Fatalf("the journal no longer records stage %s starting its original run %s", unitID, originalRunID)
	}

	// A second handle on the same directory answers from disk, not from the
	// fixture's warm connection.
	store, err := OpenSQLiteOperationStore(fleet.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	reopened := plan.service
	reopened.Store = store
	check("after restart", reopened)
}
