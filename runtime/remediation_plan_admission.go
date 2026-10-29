package runtime

import (
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// A remediation epoch of a plan-bound objective unit is a CHILD RUN of that
// plan (#480), so it runs under what the plan approved - never under whatever
// the WorkGraph machinery would give a generic unit.
//
// The epoch machinery (#581) knows graphs and units, not plans. Left alone it
// would create a run the plan never counted, bound to no assignment, under the
// graph's current worker, with a fresh budget. This file closes that: the epoch
// is admitted against the plan before its batch is written, and the run it
// becomes carries the stage's own plan binding and the stage's REMAINING
// allowance. A graph with no objective plan is untouched.

// epochGrant is everything the governing plan grants the next run of one stage.
type epochGrant struct {
	plan       domain.EngineeringPlan
	snapshot   PlanSnapshot
	stage      domain.PlanStage
	assignment domain.AgentAssignment
	binding    RunPlanBinding
	budget     domain.StageBudget
}

// planEpochGrant derives a stage's grant from durable plan state alone: the
// approved revision, the assignment frozen for the stage's performance, and what
// the plan and the stage have already spent across every run of the stage.
func planEpochGrant(store *SQLiteOperationStore, ref domain.PlanRef, unitID string) (epochGrant, error) {
	plan, found, err := store.PlanRevision(ref.ID, ref.Revision)
	if err != nil {
		return epochGrant{}, err
	}
	if !found || plan.Digest != ref.Digest {
		return epochGrant{}, fmt.Errorf("plan %s revision %d no longer matches the graph's approved binding", ref.ID, ref.Revision)
	}
	stage, ok := plan.Stage(unitID)
	if !ok {
		return epochGrant{}, fmt.Errorf("plan %s revision %d declares no stage %s", ref.ID, ref.Revision, unitID)
	}
	snapshot, err := store.ReplayPlan(ref.ID)
	if err != nil {
		return epochGrant{}, err
	}
	projection, ok := snapshot.Stages[unitID]
	if !ok || projection.AssignmentID == "" {
		return epochGrant{}, fmt.Errorf("plan %s froze no assignment for stage %s, so no epoch can succeed it", ref.ID, unitID)
	}
	assignment, found, err := store.PlanAssignment(ref.ID, ref.Revision, projection.Generation, unitID)
	if err != nil {
		return epochGrant{}, err
	}
	// Defence in depth: planStage refuses the same loss at the execution boundary
	// (TestObjectivePlanWorkGraphRecoversFromConflict... breaks it separately), so
	// this earlier refusal is what keeps a doomed epoch from being admitted and charged.
	if !found || assignment.ID != projection.AssignmentID {
		return epochGrant{}, fmt.Errorf("the frozen assignment of stage %s is not the one the plan journal recorded", unitID)
	}
	grant := epochGrant{plan: plan, snapshot: snapshot, stage: stage, assignment: assignment}
	if grant.budget, err = remainingStageBudget(plan, snapshot, stage, assignment); err != nil {
		return epochGrant{}, err
	}
	grant.binding = RunPlanBinding{
		PlanID: plan.ID, Revision: plan.Revision, PlanDigest: plan.Digest,
		StageID: unitID, AssignmentID: assignment.ID, Generation: projection.Generation,
		StageBudget: grant.budget,
	}
	return grant, nil
}

// remainingStageBudget is the stage's frozen allowance less what every run of the
// stage has already spent, narrowed again by what the plan has left. An epoch
// continues the stage; it does not get a fresh allowance.
func remainingStageBudget(plan domain.EngineeringPlan, snapshot PlanSnapshot, stage domain.PlanStage, assignment domain.AgentAssignment) (domain.StageBudget, error) {
	budget := assignment.Budget
	spent := snapshot.StageConsumed[stage.ID]
	if budget.MaxProviderInvocations > 0 {
		if budget.MaxProviderInvocations -= spent.ProviderInvocations; budget.MaxProviderInvocations <= 0 {
			return domain.StageBudget{}, fmt.Errorf("stage %s has spent its %d provider invocations", stage.ID, assignment.Budget.MaxProviderInvocations)
		}
	}
	if budget.MaxWallSeconds > 0 {
		if budget.MaxWallSeconds -= spent.WallSeconds; budget.MaxWallSeconds <= 0 {
			return domain.StageBudget{}, fmt.Errorf("stage %s has spent its %d active wall seconds", stage.ID, assignment.Budget.MaxWallSeconds)
		}
	}
	if block := invocationCeilingReached(plan, snapshot, stage.ID); block != nil {
		return domain.StageBudget{}, errors.New(block.Reason)
	}
	return remainingHeadroom(plan, snapshot).tighten(budget), nil
}

// authorizeEpoch refuses an epoch the governing plan did not grant, and returns
// the grant it would run under. It writes nothing: the charge is committed with
// the epoch itself (SQLiteOperationStore.AdmitChargedRemediationEpoch), so a
// refusal or a failure leaves the budget as it was.
//
// The worker is judged by the reconciler's own refuseWorkerDrift - provider
// kind, vendor family, trust mode and model - against the stage's frozen
// assignment, so there is one definition of "the approved worker".
//
// The caller holds s.plansMu. A grant is derived from the plan's journal and
// the child-run charge is a plan-journal append, so both must be ordered
// against the plan reconciler's own read-then-append; the ceiling is
// additionally re-checked inside the committing transaction. Lock order is
// orchestrationMu, then plansMu: nothing that holds plansMu takes
// orchestrationMu.
func (s *Supervisor) authorizeEpoch(ref domain.PlanRef, agentID, unitID string) (epochGrant, error) {
	grant, err := planEpochGrant(s.deps.Store, ref, unitID)
	if err != nil {
		return epochGrant{}, err
	}
	if agentID != grant.assignment.Agent.ID {
		return epochGrant{}, fmt.Errorf("stage %s was approved for worker %s and the graph's remediation worker is %s",
			unitID, grant.assignment.Agent.ID, agentID)
	}
	if block := (PlanReconciler{Service: s.planServiceForReconcile()}).refuseWorkerDrift(grant.stage, grant.assignment); block != nil {
		return epochGrant{}, fmt.Errorf("stage %s cannot start a remediation epoch: %s", unitID, block.Reason)
	}
	return grant, nil
}

func epochChargeFor(grant epochGrant, unitID, runID string, at time.Time) EpochCharge {
	return EpochCharge{PlanID: grant.plan.ID, StageID: unitID, RunID: runID, At: at,
		MaxChildRuns: grant.plan.BudgetEnvelope.MaxChildRuns, EnforceCeiling: true}
}

// admitPlanBoundEpoch is the one admission of a remediation epoch of a plan-bound
// unit: authorized, then charged and written in a single transaction.
func (s *Supervisor) admitPlanBoundEpoch(ref domain.PlanRef, agentID, unitID string, planned orchestration.Batch, epoch RemediationEpoch) (orchestration.Batch, error) {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	grant, err := s.authorizeEpoch(ref, agentID, unitID)
	if err != nil {
		return orchestration.Batch{}, err
	}
	stored, _, err := s.deps.Store.AdmitChargedRemediationEpoch(planned, epoch, epochChargeFor(grant, unitID, epoch.RunID, s.deps.Clock.Now()))
	return stored, err
}

// recoverUnchargedEpoch brings a stored epoch batch under the charge rule when it
// was written without one - by code that predates the charge. It runs ahead of
// materialization, from the batch alone, because a stored epoch whose run does
// not exist yet cannot be shown by the graph's status view.
//
//   - A run that does not exist yet is authorized exactly like a new epoch -
//     refused if the plan cannot grant it, charged once otherwise, and then
//     materialized under the stage's binding.
//   - A run that already exists was created before this rule, unbound, and has
//     already spent a child run. It is charged as the fact it is - a ceiling
//     cannot un-spend it - and left as it is; an immutable run is never rebound.
func (s *Supervisor) recoverUnchargedEpoch(batch orchestration.Batch) error {
	if batch.Origin == nil || batch.Origin.Epoch < 2 || len(batch.Items) != 1 {
		return nil
	}
	graph, found, err := s.deps.Store.WorkGraph(batch.Origin.GraphID)
	if err != nil || !found || graph.ObjectivePlan == nil {
		return err
	}
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	runID, unitID := batch.Items[0].RunID, batch.Origin.UnitID
	snapshot, err := s.deps.Store.ReplayPlan(graph.ObjectivePlan.ID)
	if err != nil || snapshot.RunConsumed[runID].ChildRuns > 0 {
		return err
	}
	_, exists, err := s.deps.Store.Run(runID)
	if err != nil {
		return err
	}
	if exists {
		return s.deps.Store.ChargeRemediationEpoch(EpochCharge{PlanID: graph.ObjectivePlan.ID, StageID: unitID, RunID: runID, At: s.deps.Clock.Now()})
	}
	grant, err := s.authorizeEpoch(*graph.ObjectivePlan, batch.AgentID, unitID)
	if err != nil {
		return err
	}
	return s.deps.Store.ChargeRemediationEpoch(epochChargeFor(grant, unitID, runID, s.deps.Clock.Now()))
}

// epochPlanGrant is the grant a remediation epoch's run is created under, or nil
// when the batch is not an epoch of a plan-bound objective unit.
//
// It refuses an uncharged epoch outright: reconcileOrchestration materializes
// every stored batch, so a batch admitted without its charge - by a crash, or by
// older code - must not become an executable run the plan never counted.
func (r *EngineeringRuntime) epochPlanGrant(batchID, runID string) (*epochGrant, error) {
	batch, found, err := r.deps.Store.OrchestrationBatch(batchID)
	if err != nil || !found || batch.Origin == nil || batch.Origin.Epoch < 2 {
		return nil, err
	}
	graph, found, err := r.deps.Store.WorkGraph(batch.Origin.GraphID)
	if err != nil || !found || graph.ObjectivePlan == nil {
		return nil, err
	}
	grant, err := planEpochGrant(r.deps.Store, *graph.ObjectivePlan, batch.Origin.UnitID)
	if err != nil {
		return nil, err
	}
	if grant.snapshot.RunConsumed[runID].ChildRuns != 1 {
		return nil, fmt.Errorf("remediation epoch run %s was never charged to plan %s, so it cannot become a run", runID, grant.plan.ID)
	}
	return &grant, nil
}
