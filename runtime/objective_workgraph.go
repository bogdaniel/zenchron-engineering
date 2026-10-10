package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// BindObjectivePlan compiles operational units without granting execution
// authority. The existing plan approval, assignment and budget owners still
// decide whether a stage may start.
func (s PlanService) BindObjectivePlan(plan domain.EngineeringPlan, issue int) (string, error) {
	stored, found, err := s.Store.PlanRevision(plan.ID, plan.Revision)
	if err != nil {
		return "", err
	}
	if !found || stored.Digest != plan.Digest {
		return "", errors.New("objective graph requires the exact stored plan revision")
	}
	if err := s.RefuseConfigurationHold(plan.ID); err != nil {
		return "", err
	}
	snapshot, err := s.Store.ReplayPlan(plan.ID)
	if err != nil {
		return "", err
	}
	for _, stage := range snapshot.Stages {
		if stage.RunID != "" {
			return "", errors.New("objective intake cannot adopt or duplicate already-started plan work")
		}
	}
	repository, source, found, err := s.Store.PlanSource(plan.ID)
	if err != nil {
		return "", err
	}
	if !found || source != issue || repository != plan.Subject.Repository {
		return "", errors.New("objective graph source differs from the plan source")
	}
	graph, err := compileObjectiveGraph(plan, issue, s.DefaultAgent, s.now())
	if err != nil {
		return "", err
	}
	current, err := objectiveGraphFor(s.Store, plan.ID)
	if err != nil {
		return "", err
	}
	if current != nil {
		if *current.ObjectivePlan == *graph.ObjectivePlan {
			return current.ID, nil
		}
		batches, err := s.Store.OrchestrationBatches()
		if err != nil {
			return "", err
		}
		for _, batch := range batches {
			if batch.Origin != nil && batch.Origin.GraphID == current.ID {
				return "", errors.New("objective graph has started work; its immutable plan binding cannot be replaced")
			}
		}
		if plan.Revision <= current.ObjectivePlan.Revision {
			return "", errors.New("objective graph requires a newer unstarted plan revision")
		}
		// Retain every old graph revision; a new proposal still requires its
		// own exact plan approval before any operational unit can activate.
		graph.Revision = current.Revision + 1
	}
	_, _, err = s.Store.AdoptWorkGraphRevision(graph)
	return graph.ID, err
}

// Reviewers and typed gates consume candidate units; they retain their own
// plan semantics rather than pretending to produce a candidate handoff.
func objectiveCandidateStage(stage domain.PlanStage) bool {
	return stage.Kind == domain.StageAgent && stage.InvocationMode != domain.InvocationModeNonMutatingPlanning && stage.Role != domain.RoleReviewer
}

func compileObjectiveGraph(plan domain.EngineeringPlan, issue int, agent string, at time.Time) (orchestration.WorkGraph, error) {
	name := "objective-" + plan.ID
	id, err := orchestration.WorkGraphID(plan.Subject.Repository, agent, name)
	if err != nil {
		return orchestration.WorkGraph{}, err
	}
	graph := orchestration.WorkGraph{SchemaVersion: orchestration.WorkGraphSchemaVersion, ID: id, Name: name,
		Repository: plan.Subject.Repository, AgentID: agent, Revision: 1, CreatedAt: at,
		ObjectivePlan: &domain.PlanRef{ID: plan.ID, Revision: plan.Revision, Digest: plan.Digest}}
	for _, stage := range plan.Stages {
		if !objectiveCandidateStage(stage) {
			continue
		}
		graph.Units = append(graph.Units, orchestration.WorkUnit{ID: stage.ID, Purpose: stage.Objective, Role: stage.Role, Issue: issue,
			DependsOn: objectiveDependencies(plan, stage), ExecutionKind: orchestration.WorkUnitExecutionKind(stage.ExecutionKind)})
	}
	if err := graph.Validate(); err != nil {
		return orchestration.WorkGraph{}, err
	}
	return graph, nil
}

func objectiveDependencies(plan domain.EngineeringPlan, stage domain.PlanStage) []string {
	byID := make(map[string]domain.PlanStage, len(plan.Stages))
	for _, item := range plan.Stages {
		byID[item.ID] = item
	}
	seen := map[string]bool{}
	units := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		item := byID[id]
		if objectiveCandidateStage(item) {
			units[id] = true
			return
		}
		for _, dependency := range item.DependsOn {
			visit(dependency)
		}
	}
	for _, dependency := range stage.DependsOn {
		visit(dependency)
	}
	result := make([]string, 0, len(units))
	for id := range units {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// ponytail: one bounded graph scan per plan read/start, fine for tens of
// operator graphs; add an indexed plan lookup if that becomes measurable.
func objectiveGraphFor(store *SQLiteOperationStore, planID string) (*orchestration.WorkGraph, error) {
	graphs, err := store.WorkGraphs()
	if err != nil {
		return nil, err
	}
	var result *orchestration.WorkGraph
	for i := range graphs {
		graph := &graphs[i]
		if graph.ObjectivePlan == nil || graph.ObjectivePlan.ID != planID {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("plan %s has multiple objective graphs", planID)
		}
		result = graph
	}
	return result, nil
}

// startObjectiveStage retains the plan reconciler's frozen assignment and
// narrowed budget, adding the existing graph's admitted-input and handoff
// binding to the ordinary run before its genesis is recorded.
func (r PlanReconciler) startObjectiveStage(ctx context.Context, engine *EngineeringRuntime, plan domain.EngineeringPlan, stage domain.PlanStage, binding RunPlanBinding) (StartOutcome, error) {
	graph, err := objectiveGraphFor(r.Store, plan.ID)
	if err != nil {
		return StartOutcome{}, err
	}
	if graph == nil {
		return engine.StartPlanStageRun(ctx, r.Issue, binding)
	}
	if graph.ObjectivePlan.Revision != plan.Revision || graph.ObjectivePlan.Digest != plan.Digest {
		return StartOutcome{}, errors.New("objective graph does not bind the currently approved plan revision")
	}
	holds, err := r.Store.WorkGraphHolds(graph.ID)
	if err != nil {
		return StartOutcome{}, err
	}
	view, err := WorkGraphStatus(r.Store, r.StateDir, graph.ID, r.now(), holds)
	if err != nil {
		return StartOutcome{}, err
	}
	byID := make(map[string]WorkGraphUnitView, len(view.Units))
	for _, unit := range view.Units {
		byID[unit.UnitID] = unit
	}
	for _, dependency := range objectiveDependencies(plan, stage) {
		unit, found := byID[dependency]
		if !found || unit.State != orchestration.UnitState(orchestration.ItemCompleted) || unit.Output == nil {
			return StartOutcome{}, fmt.Errorf("stage %s awaits the admitted handoff of objective unit %s", stage.ID, dependency)
		}
	}
	if !objectiveCandidateStage(stage) {
		return engine.StartPlanStageRun(ctx, r.Issue, binding)
	}
	if binding.Generation != 0 {
		return StartOutcome{}, errors.New("objective graph unit was invalidated; another execution cannot replace its admitted inputs")
	}
	unit, found := byID[stage.ID]
	if !found || (unit.State != orchestration.UnitReady && unit.RunID == "") {
		return StartOutcome{}, fmt.Errorf("objective unit %s is not runnable", stage.ID)
	}
	if unit.State == orchestration.UnitInvalidated || unit.State == orchestration.UnitUnknown {
		return StartOutcome{}, fmt.Errorf("objective unit %s is %s: %s", stage.ID, unit.State, unit.Reason)
	}
	if !writesTypedResults(engine.deps.Provider) {
		return StartOutcome{}, errors.New("objective worker cannot produce a typed handoff")
	}
	origin := orchestration.BatchOrigin{GraphID: graph.ID, UnitID: stage.ID, Inputs: unit.Inputs, ExecutionKind: unit.ExecutionKind}
	batchID, err := orchestration.WorkUnitBatchID(r.Repository, engine.deps.Agent.ID, r.Issue, origin)
	if err != nil {
		return StartOutcome{}, err
	}
	runID, err := derivedRunID(r.Repository, r.Issue, engine.deps.ConfigDigest, 0, &planStageIdentity{Plan: plan.ID, Revision: plan.Revision, Stage: stage.ID, Generation: binding.Generation})
	if err != nil {
		return StartOutcome{}, err
	}
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion, ID: batchID, Repository: r.Repository, AgentID: engine.deps.Agent.ID,
		RequestedBy: graph.RequestedBy, CreatedAt: r.now(), Origin: &origin, Items: []orchestration.BatchItem{{Issue: r.Issue, RunID: runID}}}
	stored, _, err := r.Store.CreateOrchestrationBatch(batch)
	if err != nil {
		return StartOutcome{}, err
	}
	if len(stored.Items) != 1 || stored.Items[0].RunID != runID {
		return StartOutcome{}, errors.New("objective batch belongs to another plan run")
	}
	outcome, err := engine.startPlanStageRun(ctx, r.Issue, binding, &RunOrchestrationBinding{BatchID: batchID})
	if err != nil {
		return StartOutcome{}, err
	}
	_, err = r.Store.ActivateWorkUnit(WorkUnitActivation{GraphID: graph.ID, UnitID: stage.ID, BatchID: batchID, RunID: outcome.RunID, InputsDigest: unit.InputsDigest, ActivatedAt: r.now()})
	return outcome, err
}

// An interrupted objective dispatch is recovered by the plan reconciler,
// which can prove approval, frozen assignments and remaining aggregate budget.
// The generic batch materializer must never recover it as a flat issue run.
func (r *EngineeringRuntime) refuseFlatObjectiveRecovery(batchID string) error {
	batch, found, err := r.deps.Store.OrchestrationBatch(batchID)
	if err != nil {
		return err
	}
	if !found || batch.Origin == nil {
		return nil
	}
	graph, found, err := r.deps.Store.WorkGraph(batch.Origin.GraphID)
	if err != nil {
		return err
	}
	if found && graph.ObjectivePlan != nil {
		return errors.New("objective child awaits recovery through its approved plan")
	}
	return nil
}

// Composition starts at the plan's trusted base and transfers every admitted
// input through the existing integration algorithm. It cannot select one
// sibling candidate as though that candidate already contained the others.
func (r PlanReconciler) objectiveStageBase(plan domain.EngineeringPlan, stage domain.PlanStage, assignment domain.AgentAssignment) (string, *CandidateRef, error) {
	if stage.ExecutionKind != string(orchestration.ExecutionKindIntegrationCompose) {
		return r.upstreamBase(assignment)
	}
	graph, err := objectiveGraphFor(r.Store, plan.ID)
	if err != nil {
		return "", nil, err
	}
	if graph == nil || graph.ObjectivePlan.Revision != plan.Revision || graph.ObjectivePlan.Digest != plan.Digest {
		return "", nil, errors.New("deterministic plan composition requires its exact objective work graph")
	}
	return plan.Subject.Revision, nil, nil
}
