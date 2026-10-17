package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// remediationJourney varies the conflict -> epoch journey where a scenario needs
// the plan or the operator registry to differ from the approved defaults.
type remediationJourney struct {
	// envelope is what the plan is approved for; zero means room for the three
	// stage runs and both epochs.
	envelope domain.PlanBudgetEnvelope
	// epochPlans rewrites the plan service the supervisor admits epochs with, as
	// the registry projection it would hold after the operator changed a worker.
	epochPlans func(PlanService) PlanService
	// beforeEpochs runs after the epoch is authorized and before it is activated.
	beforeEpochs func(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, graphID, unitID string)
	// afterEpochs replaces the success assertions once the epochs have been driven.
	afterEpochs func(t *testing.T, fleet *fleetFixture, plan *planRunFixture, graphID, unitID string)
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

// The stage was approved against one worker. The same id re-pointed at another
// provider, a weaker trust mode, or a model nobody named is a replacement the plan
// never granted - judged by the reconciler's own refuseWorkerDrift.
func TestARemediationEpochUnderADriftedWorkerIsRefusedAndCreatesNoRun(t *testing.T) {
	for name, drift := range map[string]func(*domain.ExecutionAgentDescriptor){
		"provider kind": func(a *domain.ExecutionAgentDescriptor) { a.ProviderKind = "claude_code" },
		"vendor family": func(a *domain.ExecutionAgentDescriptor) { a.VendorFamily = "anthropic" },
		"trust mode":    func(a *domain.ExecutionAgentDescriptor) { a.TrustMode = domain.TrustRequirementProtected },
		"model":         func(a *domain.ExecutionAgentDescriptor) { a.Model = "a-model-nobody-approved" },
	} {
		t.Run(name, func(t *testing.T) {
			runObjectiveRemediationJourney(t,
				remediationJourney{refused: true, epochPlans: func(service PlanService) PlanService {
					agents := planAgents()
					for i := range agents {
						if agents[i].ID == "codex" {
							drift(&agents[i])
						}
					}
					service.Agents = agents
					return service
				}},
				func(t *testing.T, fleet *fleetFixture, envelope domain.PlanBudgetEnvelope) *planRunFixture {
					return newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
				})
		})
	}
}

// The frozen assignment is what an epoch runs under. A stage whose assignment can
// no longer be read back has no authority to grant, and no generic substitute.
func TestARemediationEpochWithoutItsFrozenAssignmentIsRefusedAndCreatesNoRun(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{refused: true, beforeEpochs: func(t *testing.T, fleet *fleetFixture, _ *Supervisor, _, _ string) {
			if _, err := fleet.store.db.Exec(`DELETE FROM plan_assignments WHERE stage_id = 'frontend'`); err != nil {
				t.Fatal(err)
			}
		}},
		func(t *testing.T, fleet *fleetFixture, envelope domain.PlanBudgetEnvelope) *planRunFixture {
			return newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
		})
}

// A stage's allowance is spent across ALL its runs. An epoch is the remainder, not
// a fresh allowance, and a stage that has spent its own allowance gets no epoch.
func TestAnEpochGetsOnlyWhatItsStageAndPlanHaveLeft(t *testing.T) {
	plan := domain.EngineeringPlan{ID: "p", BudgetEnvelope: domain.PlanBudgetEnvelope{MaxChildRuns: 6, MaxProviderInvocations: 10, MaxWallSeconds: 1000}}
	stage := domain.PlanStage{ID: "s"}
	assignment := domain.AgentAssignment{Budget: domain.StageBudget{MaxProviderInvocations: 5, MaxWallSeconds: 600, MaxExecutionAttempts: 2}}
	snapshot := func(stageInvocations, stageWall, planInvocations, planWall int) PlanSnapshot {
		return PlanSnapshot{
			StageConsumed: map[string]domain.PlanConsumption{"s": {ProviderInvocations: stageInvocations, WallSeconds: stageWall}},
			Consumed:      domain.PlanConsumption{ProviderInvocations: planInvocations, WallSeconds: planWall},
		}
	}
	budget, err := remainingStageBudget(plan, snapshot(2, 100, 2, 100), stage, assignment)
	if err != nil || budget.MaxProviderInvocations != 3 || budget.MaxWallSeconds != 500 || budget.MaxExecutionAttempts != 2 {
		t.Fatalf("a stage with 3 invocations and 500s left got %+v (%v)", budget, err)
	}
	// the plan's remainder narrows it further
	if budget, err = remainingStageBudget(plan, snapshot(2, 100, 9, 100), stage, assignment); err != nil || budget.MaxProviderInvocations != 1 {
		t.Fatalf("a plan with 1 invocation left granted %+v (%v)", budget, err)
	}
	for name, spent := range map[string]PlanSnapshot{
		"stage invocations": snapshot(5, 0, 5, 0),
		"stage wall":        snapshot(0, 600, 0, 600),
		"plan invocations":  snapshot(0, 0, 10, 0),
		"plan wall":         snapshot(0, 0, 0, 1000),
	} {
		if budget, err = remainingStageBudget(plan, spent, stage, assignment); err == nil {
			t.Fatalf("%s exhausted, and an epoch was still granted %+v", name, budget)
		}
	}
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

// assertEpochRunsUnderTheStagesAuthority proves the epoch run is the stage's own
// run - the frozen assignment's objective, the same assignment binding, a budget
// that is the remainder - and that an interrupted admission can neither run an
// uncharged epoch nor charge a charged one twice.
func assertEpochRunsUnderTheStagesAuthority(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, plan *planRunFixture,
	graphID, unitID, originalRunID string, epoch RemediationEpoch, worker *objectivePlanRemediationWorker) {
	t.Helper()
	original, ok := storedRun(t, fleet.phase8Fixture, originalRunID)
	if !ok || original.Plan == nil {
		t.Fatalf("the original run %s is not a plan-bound run", originalRunID)
	}
	run, ok := storedRun(t, fleet.phase8Fixture, epoch.RunID)
	if !ok || run.Plan == nil {
		t.Fatalf("epoch run %s is not bound to the plan stage it continues", epoch.RunID)
	}
	if run.Plan.PlanID != original.Plan.PlanID || run.Plan.StageID != unitID || run.Plan.AssignmentID != original.Plan.AssignmentID ||
		run.Plan.Revision != original.Plan.Revision || run.Plan.Generation != original.Plan.Generation {
		t.Fatalf("epoch run is bound to %+v, not to the stage's own binding %+v", run.Plan, original.Plan)
	}
	worker.mu.Lock()
	epochObjective, originalObjective := worker.objectives[epoch.RunID], worker.objectives[originalRunID]
	worker.mu.Unlock()
	if epochObjective == "" || epochObjective != originalObjective {
		t.Fatalf("the epoch was asked to do %q, not the frozen stage objective %q", epochObjective, originalObjective)
	}
	// 12 invocations are approved for the plan and the other producer's and this
	// stage's first runs have already spent some: the epoch is the remainder.
	if granted := run.Plan.StageBudget.MaxProviderInvocations; granted <= 0 || granted >= plan.service.Envelope.MaxProviderInvocations {
		t.Fatalf("the epoch was granted %d provider invocations of the plan's %d, which is not a remainder", granted, plan.service.Envelope.MaxProviderInvocations)
	}
	if run.Budgets == nil || run.Budgets.MaxProviderInvocations != run.Plan.StageBudget.MaxProviderInvocations {
		t.Fatalf("the epoch run persisted budgets %+v, not its stage grant %+v", run.Budgets, run.Plan.StageBudget)
	}

	// The overlay trusts only an epoch of this graph and unit whose run is the
	// stage's own.
	ref := domain.PlanRef{ID: plan.plan.ID, Revision: plan.plan.Revision, Digest: plan.plan.Digest}
	wrongUnit, wrongRun, wrongEpoch := epoch, epoch, epoch
	wrongUnit.UnitID = "integrate"
	wrongRun.RunID = originalRunID
	wrongEpoch.Epoch++
	futureRef := ref
	futureRef.Revision++
	for name, foreign := range map[string]struct {
		ref   domain.PlanRef
		epoch RemediationEpoch
	}{
		"another unit":                      {ref, wrongUnit},
		"a run of another batch":            {ref, wrongRun},
		"a batch of another epoch":          {ref, wrongEpoch},
		"a run under another plan revision": {futureRef, epoch},
	} {
		if _, err := fleet.store.refuseForeignEpoch(foreign.ref, graphID, unitID, foreign.epoch); err == nil {
			t.Fatalf("an epoch naming %s was accepted as the stage's current run", name)
		}
	}

	// ...and ReplayPlan itself refuses a stage whose epoch row names a run that is
	// not bound to that batch.
	tamper := func(runID string) {
		t.Helper()
		if _, err := fleet.store.db.Exec(`UPDATE work_graph_remediation_epochs SET run_id = ? WHERE graph_id = ? AND unit_id = ?`, runID, graphID, unitID); err != nil {
			t.Fatal(err)
		}
	}
	tamper(originalRunID)
	_, replayErr := fleet.store.ReplayPlan(plan.plan.ID)
	tamper(epoch.RunID)
	if replayErr == nil {
		t.Fatal("ReplayPlan accepted an epoch row naming a run that is not the epoch's own")
	}

	view := fleet.graph(graphID)
	for range 2 {
		if err := supervisor.admitEpochToPlan(view.ObjectivePlan, view.AgentID, unitID, epoch.RunID); err != nil {
			t.Fatalf("re-admitting an admitted epoch: %v", err)
		}
	}
	snapshot, err := fleet.store.ReplayPlan(plan.plan.ID)
	if err != nil || snapshot.RunConsumed[epoch.RunID].ChildRuns != 1 || snapshot.Consumed.ChildRuns != 5 {
		t.Fatalf("a replayed admission changed the charge: epoch=%+v total=%d (%v)", snapshot.RunConsumed[epoch.RunID], snapshot.Consumed.ChildRuns, err)
	}
	// An epoch batch whose run was never charged - a crash between the writes, or
	// older code - must not become an executable run.
	engine, err := supervisor.engine("acme/repo", view.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	const uncharged = "run-never-charged"
	if err := engine.materializeOrchestratedRun(context.Background(), epoch.BatchID, orchestration.BatchItem{Issue: plan.issue, RunID: uncharged}); err == nil {
		t.Fatal("an uncharged remediation epoch was materialized into a run")
	}
	if _, found := storedRun(t, fleet.phase8Fixture, uncharged); found {
		t.Fatal("an uncharged remediation epoch left a run behind")
	}
	assertExecutionBoundaryEnforcesTheFrozenAssignment(t, fleet, engine, plan, run, unitID)
}

func objectivePlanFixture(t *testing.T, fleet *fleetFixture, envelope domain.PlanBudgetEnvelope) *planRunFixture {
	t.Helper()
	return newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
}

func chargedChildRuns(t *testing.T, fleet *fleetFixture) PlanSnapshot {
	t.Helper()
	snapshot, err := fleet.store.ReplayPlan("plan-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// The epoch is charged BEFORE its batch is written, so a failure between the two
// leaves a charge and no epoch. Retrying must reuse the charge, never add a second
// one, and the epoch must not exist - or run - in the meantime.
func TestACrashBetweenTheChargeAndTheEpochAdmissionRecoversExactlyOnce(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{beforeEpochs: func(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, graphID, unitID string) {
			if _, err := fleet.store.db.Exec(`CREATE TRIGGER crash_epoch BEFORE INSERT ON work_graph_remediation_epochs
				BEGIN SELECT RAISE(ABORT, 'crash between the charge and the admission'); END`); err != nil {
				t.Fatal(err)
			}
			runsBefore := len(storedRuns(t, fleet.phase8Fixture))
			for range 3 {
				if _, err := supervisor.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				fleet.clock.advance(61 * time.Second)
			}
			if epochs, err := fleet.store.WorkUnitRemediationEpochs(graphID, unitID); err != nil || len(epochs) != 0 {
				t.Fatalf("an epoch was admitted through the crash: %+v (%v)", epochs, err)
			}
			if after := len(storedRuns(t, fleet.phase8Fixture)); after != runsBefore {
				t.Fatalf("the crashed admission created a run: %d before, %d after", runsBefore, after)
			}
			if got := chargedChildRuns(t, fleet).Consumed.ChildRuns; got != 4 {
				t.Fatalf("three retries left %d child runs charged, want the three stages plus ONE epoch", got)
			}
			if _, err := fleet.store.db.Exec(`DROP TRIGGER crash_epoch`); err != nil {
				t.Fatal(err)
			}
		}},
		objectivePlanFixture)
}

// admitEpochWithoutCharge writes an epoch the way the code before the charge rule
// did: batch and epoch row, no charge. It returns the epoch's run id.
func admitEpochWithoutCharge(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, graphID, unitID string) (string, *EngineeringRuntime) {
	t.Helper()
	// With no workforce configured the supervisor authorizes the epoch but cannot
	// admit it, which leaves exactly the state older code would have admitted from.
	configured := supervisor.deps.Plans
	supervisor.deps.Plans = PlanService{}
	var authorization RemediationAuthorization
	var pending bool
	for range 20 {
		var err error
		if authorization, pending, err = pendingRemediationEpoch(fleet.store, graphID, unitID); err != nil {
			t.Fatal(err)
		}
		if pending {
			break
		}
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
	}
	supervisor.deps.Plans = configured
	if !pending {
		t.Fatal("the epoch was never authorized")
	}
	view := fleet.graph(graphID)
	unit := view.unit(t, unitID)
	engine, err := supervisor.engine(view.Repository, view.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	origin := orchestration.BatchOrigin{GraphID: graphID, UnitID: unitID, Inputs: unit.Inputs, ExecutionKind: unit.ExecutionKind, Epoch: authorization.ProposedEpoch}
	batchID, err := orchestration.WorkUnitBatchID(view.Repository, view.AgentID, unit.Issue, origin)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := reservedRunIDs(fleet.store)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := engine.planRemediationEpochBatch(batchID, unit.Issue, view.RequestedBy, reserved, origin)
	if err != nil {
		t.Fatal(err)
	}
	epoch := RemediationEpoch{GraphID: graphID, UnitID: unitID, Epoch: authorization.ProposedEpoch, BatchID: batchID,
		RunID: planned.Items[0].RunID, InputsDigest: unit.InputsDigest, AuthorizationID: authorization.ID, ActivatedAt: fleet.clock.Now()}
	if _, _, err := fleet.store.AdmitRemediationEpochBatch(planned, epoch); err != nil {
		t.Fatal(err)
	}
	return epoch.RunID, engine
}

// An epoch stored by code that predates the charge is charged and materialized
// under the stage's binding - once - and until then it cannot become a run.
func TestAnEpochStoredBeforeTheChargeRuleIsRecoveredAndRunsUnderTheStage(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{beforeEpochs: func(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, graphID, unitID string) {
			runID, engine := admitEpochWithoutCharge(t, fleet, supervisor, graphID, unitID)
			epochs, err := fleet.store.WorkUnitRemediationEpochs(graphID, unitID)
			if err != nil || len(epochs) != 1 {
				t.Fatalf("the stored epoch is missing: %+v (%v)", epochs, err)
			}
			item := orchestration.BatchItem{Issue: fleet.issue, RunID: runID}
			if err := engine.materializeOrchestratedRun(context.Background(), epochs[0].BatchID, item); err == nil {
				t.Fatal("an uncharged stored epoch was materialized into a run")
			}
			if _, found := storedRun(t, fleet.phase8Fixture, runID); found {
				t.Fatal("an uncharged stored epoch left a run behind")
			}
		}},
		objectivePlanFixture)
}

// A run created before the rule is unbound and already spent a child run: it is
// charged as the fact it is, exactly once, and never adopted as the stage's run.
func TestAnEpochRunCreatedBeforeTheChargeRuleIsChargedOnceAndNotAdopted(t *testing.T) {
	var legacyRunID string
	runObjectiveRemediationJourney(t,
		remediationJourney{
			beforeEpochs: func(t *testing.T, fleet *fleetFixture, supervisor *Supervisor, graphID, unitID string) {
				var engine *EngineeringRuntime
				legacyRunID, engine = admitEpochWithoutCharge(t, fleet, supervisor, graphID, unitID)
				epochs, _ := fleet.store.WorkUnitRemediationEpochs(graphID, unitID)
				if _, err := engine.createRun(context.Background(), legacyRunID, issueGoal(engine.deps.Repository.Identity, fleet.issue), nil,
					&RunOrchestrationBinding{BatchID: epochs[0].BatchID}, domain.StageBudget{}, fleet.store.ClaimRun); err != nil {
					t.Fatal(err)
				}
			},
			afterEpochs: func(t *testing.T, fleet *fleetFixture, plan *planRunFixture, graphID, unitID string) {
				snapshot := chargedChildRuns(t, fleet)
				if got := snapshot.RunConsumed[legacyRunID].ChildRuns; got != 1 {
					t.Fatalf("the pre-existing epoch run was charged %d times, want once", got)
				}
				if run, _ := storedRun(t, fleet.phase8Fixture, legacyRunID); run.Plan != nil {
					t.Fatalf("an immutable run was rebound to %+v", run.Plan)
				}
				if got := snapshot.Stages[unitID].RunID; got == legacyRunID || got == "" {
					t.Fatalf("the stage adopted run %q, which is not bound to the plan", got)
				}
			},
		},
		objectivePlanFixture)
}

// The stage has spent ITS allowance while the plan still has aggregate headroom:
// the epoch must be refused on the stage's own ceiling, before any run exists.
func TestAnEpochIsRefusedWhenItsStageIsSpentEvenWithAggregateBudgetLeft(t *testing.T) {
	runObjectiveRemediationJourney(t,
		remediationJourney{refused: true, beforeEpochs: func(t *testing.T, fleet *fleetFixture, _ *Supervisor, _, unitID string) {
			snapshot := chargedChildRuns(t, fleet)
			if snapshot.StageConsumed[unitID].ProviderInvocations < 1 || snapshot.Consumed.ProviderInvocations >= 12 {
				t.Fatalf("precondition: stage spent %+v, plan spent %+v of 12", snapshot.StageConsumed[unitID], snapshot.Consumed)
			}
			assignment, found, err := fleet.store.PlanAssignment("plan-fixture", 1, 0, unitID)
			if err != nil || !found {
				t.Fatalf("frozen assignment: %v", err)
			}
			assignment.Budget.MaxProviderInvocations = 1
			document, err := CanonicalJSON(assignment)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fleet.store.db.Exec(`UPDATE plan_assignments SET document = ? WHERE stage_id = ?`, string(document), unitID); err != nil {
				t.Fatal(err)
			}
		}},
		objectivePlanFixture)

}

// assertExecutionBoundaryEnforcesTheFrozenAssignment exercises the preparation the
// epoch's own execution goes through (planStage, ahead of every provider
// invocation), independent of any admission check: with a valid assignment it
// delivers the approved objective, model preference and stage budget, and with
// the stored assignment gone or corrupt - the worker itself unchanged - it
// refuses, so nothing is invoked.
func assertExecutionBoundaryEnforcesTheFrozenAssignment(t *testing.T, fleet *fleetFixture, engine *EngineeringRuntime, plan *planRunFixture, run EngineeringRun, unitID string) {
	t.Helper()
	frozen, found, err := fleet.store.PlanAssignment(run.Plan.PlanID, run.Plan.Revision, run.Plan.Generation, unitID)
	if err != nil || !found {
		t.Fatalf("frozen assignment: %v", err)
	}
	state, err := engine.load(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := engine.planStage(state)
	if err != nil || stage.assignment == nil {
		t.Fatalf("a valid assignment was not delivered to the epoch's execution: %v", err)
	}
	delivered, err := CanonicalJSON(*stage.assignment)
	approved, approvedErr := CanonicalJSON(frozen)
	if err != nil || approvedErr != nil || string(delivered) != string(approved) {
		t.Fatalf("the epoch executes under %+v, not the approved assignment %+v", stage.assignment, frozen)
	}
	if stage.assignment.ID != run.Plan.AssignmentID || stage.assignment.Context.Objective == "" || stage.assignment.Context.Objective != frozen.Context.Objective ||
		stage.assignment.Agent.Model != frozen.Agent.Model || stage.assignment.TrustRequirement != frozen.TrustRequirement || stage.assignment.Budget != frozen.Budget {
		t.Fatalf("objective, model, trust or budget differ from the approval: %+v", stage.assignment)
	}
	if want := run.Plan.StageBudget; want.MaxProviderInvocations <= 0 || run.Budgets == nil || run.Budgets.MaxProviderInvocations != want.MaxProviderInvocations {
		t.Fatalf("the epoch's effective budget %+v is not its stage grant %+v", run.Budgets, want)
	}
	var document string
	if err := fleet.store.db.QueryRow(`SELECT document FROM plan_assignments WHERE stage_id = ?`, unitID).Scan(&document); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if _, err := fleet.store.db.Exec(`UPDATE plan_assignments SET document = ? WHERE stage_id = ?`, document, unitID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	for name, damage := range map[string]string{
		"corrupt": `UPDATE plan_assignments SET document = '{"not":"an assignment"}' WHERE stage_id = ?`,
		"other":   `UPDATE plan_assignments SET document = REPLACE(document, '"id":"` + frozen.ID + `"', '"id":"assignment-rewritten"') WHERE stage_id = ?`,
	} {
		if _, err := fleet.store.db.Exec(damage, unitID); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.planStage(state); err == nil {
			t.Fatalf("an epoch's execution was prepared under a %s frozen assignment", name)
		}
		restore()
	}
	if _, err := fleet.store.db.Exec(`DELETE FROM plan_assignments WHERE stage_id = ?`, unitID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.planStage(state); err == nil {
		t.Fatal("an epoch's execution was prepared with no frozen assignment")
	}
	if _, err := fleet.store.db.Exec(`INSERT INTO plan_assignments (plan_id, revision, stage_id, generation, assignment_id, document) VALUES (?, ?, ?, ?, ?, ?)`,
		run.Plan.PlanID, run.Plan.Revision, unitID, run.Plan.Generation, frozen.ID, document); err != nil {
		t.Fatal(err)
	}
}
