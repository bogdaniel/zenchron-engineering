package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/product"
)

// BindObjectivePlan compiles operational units without granting execution
// authority. The existing plan approval, assignment and budget owners still
// decide whether a stage may start.
func (s PlanService) BindObjectivePlan(plan domain.EngineeringPlan, issue int, agent string) (string, error) {
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
	graph, err := compileObjectiveGraph(plan, issue, agent, s.now())
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
		if err := s.refuseStartedObjectiveRevision(plan.ID); err != nil {
			return "", err
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

// AssociateProductGraph binds an objective's compiled WorkGraph to the
// product the operator selected at intake (#471/#479). Empty productID is
// "no product chosen" and associates nothing - an objective proposed without
// a product stays exactly as unassociated as before this existed.
//
// It fails closed through the existing #476 store boundary: an unknown
// product, or a graph already claimed by a DIFFERENT product, refuses rather
// than silently leaving the graph unassociated or switching ownership.
func (s PlanService) AssociateProductGraph(productID, graphID string) error {
	if productID == "" {
		return nil
	}
	association, err := product.NewGraphAssociation(productID, graphID, s.now())
	if err != nil {
		return err
	}
	_, err = s.Store.AssociateWorkGraph(association)
	return err
}

// RequireProductIfChosen refuses before anything else is spent: an objective
// proposed for a product that was never adopted, or that never named this
// OBJECTIVE'S OWN repository, names a product AssociateProductGraph is
// guaranteed to refuse once the plan and graph are already durably written
// and a provider invocation already ran. Checking it first - once the
// repository is resolved, but before planning - means that cost is never
// spent on a proposal this certain to fail.
//
// This is advisory, not the authority: a product's repository list can be
// revised, so the only thing that can change between this check and the
// later write is repositories being ADDED or REMOVED from a product, never
// the product disappearing (no deletion path exists). AssociateWorkGraph's
// own transactional membership check remains the authoritative final
// defense against that race; this merely avoids paying for a proposal that
// is overwhelmingly certain to be refused there anyway.
func (s PlanService) RequireProductIfChosen(productID, repository string) error {
	if productID == "" {
		return nil
	}
	owner, found, err := s.Store.CurrentProduct(productID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("product %s does not exist", productID)
	}
	if !slices.Contains(owner.Repositories, repository) {
		return fmt.Errorf("repository %s is not a member of product %s", repository, productID)
	}
	return nil
}

// Reviewers and typed gates consume candidate units; they retain their own
// plan semantics rather than pretending to produce a candidate handoff.
func objectiveCandidateStage(stage domain.PlanStage) bool {
	return stage.Kind == domain.StageAgent && stage.InvocationMode != domain.InvocationModeNonMutatingPlanning && planning.ProducesMaterialChange(stage.Role)
}

func hasObjectiveCandidate(plan domain.EngineeringPlan) bool {
	for _, stage := range plan.Stages {
		if objectiveCandidateStage(stage) {
			return true
		}
	}
	return false
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
		// The plan owns the full objective; the graph stores only its bounded stage reference.
		graph.Units = append(graph.Units, orchestration.WorkUnit{ID: stage.ID, Purpose: "Execute approved plan stage " + stage.ID, Role: stage.Role, Issue: issue,
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

// A claimed objective child already binds the approved plan, even when a
// crash has not yet persisted its run. Refuse before proposal writes can
// supersede that approval while leaving the operational graph behind.
func (s PlanService) refuseStartedObjectiveRevision(planID string) error {
	graph, err := objectiveGraphFor(s.Store, planID)
	if err != nil || graph == nil {
		return err
	}
	batches, err := s.Store.OrchestrationBatches()
	if err != nil {
		return err
	}
	for _, batch := range batches {
		if batch.Origin != nil && batch.Origin.GraphID == graph.ID {
			return &PlanRefusedError{PlanID: planID, Detail: "objective graph has started work; its immutable plan binding cannot be replaced"}
		}
	}
	return nil
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
		if binding.Product, err = r.Store.planStageProduct(graph.ID, stage.ID); err != nil {
			return StartOutcome{}, err
		}
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
	origin := orchestration.BatchOrigin{GraphID: graph.ID, UnitID: stage.ID, Inputs: unit.Inputs, ExecutionKind: unit.ExecutionKind, AcceptanceFeature: stage.AcceptanceFeature}
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
	// Frozen at the first write: a replay recovers the stored batch and its pin
	// BEFORE compiling anything, so a configuration adopted (or broken) since
	// admission cannot block recovery of a context that is still valid.
	stored, replayed, err := r.Store.OrchestrationBatch(batchID)
	if err != nil {
		return StartOutcome{}, err
	}
	if !replayed {
		if batch, err = r.Store.pinProductContext(batch); err != nil {
			return StartOutcome{}, err
		}
		if stored, _, err = r.Store.CreateOrchestrationBatch(batch); err != nil {
			return StartOutcome{}, err
		}
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
	// A remediation epoch's batch (#581) is never a flat recovery
	// fabrication: AdmitRemediationEpochBatch is its only writer, and it
	// always commits the batch together with its governing
	// RemediationAuthorization/RemediationEpoch rows in one transaction, so
	// the batch's mere existence already proves it was authorized through
	// the approved plan's own remediation seam, never invented here.
	if batch.Origin.Epoch > 0 {
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
