package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestObjectiveGraphRequiresApprovalAndRetainsFrozenStageBindings(t *testing.T) {
	fleet := newFleetFixture(t, 2)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	engine, err := NewEngineeringRuntime(fleet.deps)
	if err != nil {
		t.Fatal(err)
	}
	fleet.runtime = engine
	fixture := newPlanRunFixtureOn(t, fleet.phase8Fixture, parallelStages(), fleet.deps.ConfigDigest)
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	if report := fixture.reconcile(t); len(report.Started) != 0 || report.Waiting != "awaiting_operator_approval" {
		t.Fatalf("unapproved objective executed: %+v", report)
	}
	runs, err := fixture.store.Runs()
	if err != nil || len(runs) != 0 {
		t.Fatalf("unapproved objective persisted runs: %+v, %v", runs, err)
	}
	fixture.approve(t)
	report := fixture.reconcile(t)
	if len(report.Started) != 2 {
		t.Fatalf("approved independent stages did not start: %+v", report)
	}
	activations, err := fixture.store.WorkUnitActivations(graphID)
	if err != nil || len(activations) != 2 {
		t.Fatalf("objective activation records: %+v, %v", activations, err)
	}
	runs, err = fixture.store.Runs()
	if err != nil || len(runs) != 2 {
		t.Fatalf("objective runs: %+v, %v", runs, err)
	}
	objectives := map[string]bool{}
	for _, run := range runs {
		if run.Plan == nil || run.Plan.PlanID != fixture.plan.ID || run.Plan.PlanDigest != fixture.plan.Digest || run.Orchestration == nil {
			t.Fatalf("run lost one of its authority or operational bindings: %+v", run)
		}
		assignment, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, run.Plan.StageID)
		if err != nil || !found || assignment.ID != run.Plan.AssignmentID {
			t.Fatalf("frozen assignment disagrees with run: %+v, %v", assignment, err)
		}
		objectives[assignment.Context.Objective] = true
		activation := activations[run.Plan.StageID]
		if activation.RunID != run.ID || activation.BatchID != run.Orchestration.BatchID {
			t.Fatalf("unit activation differs from bound run: %+v, %+v", activation, run)
		}
		batch, found, err := fixture.store.OrchestrationBatch(run.Orchestration.BatchID)
		if err != nil || !found || batch.Origin == nil || batch.Origin.GraphID != graphID || batch.Origin.UnitID != run.Plan.StageID {
			t.Fatalf("batch lost graph origin: %+v, %v", batch, err)
		}
	}
	if !objectives["Implement the backend half."] || !objectives["Implement the frontend half."] || len(objectives) != 2 {
		t.Fatalf("workers did not receive distinct responsibilities: %+v", objectives)
	}
}

func TestObjectiveDependencyRequiresAdmittedHandoffDespitePlanCompletion(t *testing.T) {
	fleet := newFleetFixture(t, 2)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	engine, err := NewEngineeringRuntime(fleet.deps)
	if err != nil {
		t.Fatal(err)
	}
	fleet.runtime = engine
	stages := parallelStages()[:2]
	stages[1].DependsOn = []string{stages[0].ID}
	fixture := newPlanRunFixtureOn(t, fleet.phase8Fixture, stages, fleet.deps.ConfigDigest)
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	fixture.approve(t)
	report := fixture.reconcile(t)
	if len(report.Started) != 1 || report.Started[0].StageID != "backend" {
		t.Fatalf("dependent stage started before producer: %+v", report)
	}
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID, EventPlanStageSettled,
		PlanStageSettledPayload{StageID: "backend", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	report = fixture.reconcile(t)
	if len(report.Started) != 0 {
		t.Fatalf("plan completion bypassed handoff admission: %+v", report)
	}
	view, err := WorkGraphStatus(fixture.store, fixture.stateDir, graphID, fixture.clock.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dependent := view.unit(t, "frontend")
	if dependent.RunID != "" || dependent.State != orchestration.UnitBlocked {
		t.Fatalf("dependent acquired execution without admitted input: %+v", dependent)
	}
	_, err = fixture.reconciler.startObjectiveStage(context.Background(), engine, fixture.plan, stages[1],
		RunPlanBinding{PlanID: fixture.plan.ID, Revision: fixture.plan.Revision, PlanDigest: fixture.plan.Digest, StageID: "frontend"})
	if err == nil || !strings.Contains(err.Error(), "awaits the admitted handoff of objective unit backend") {
		t.Fatalf("objective dispatch did not enforce handoff admission: %v", err)
	}
}

func TestObjectiveInterruptedBatchCannotRecoverAsFlatIssueRun(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	fixture.runtime.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	origin := orchestration.BatchOrigin{GraphID: graphID, UnitID: "backend"}
	batchID, err := orchestration.WorkUnitBatchID("acme/repo", fixture.runtime.deps.Agent.ID, fixture.issue, origin)
	if err != nil {
		t.Fatal(err)
	}
	item := orchestration.BatchItem{Issue: fixture.issue, RunID: "run-objective-crash"}
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion, ID: batchID,
		Repository: "acme/repo", AgentID: fixture.runtime.deps.Agent.ID, CreatedAt: fixture.clock.Now(),
		Origin: &origin, Items: []orchestration.BatchItem{item}}
	if _, _, err := fixture.store.CreateOrchestrationBatch(batch); err != nil {
		t.Fatal(err)
	}
	err = fixture.runtime.materializeOrchestratedRun(context.Background(), batchID, item)
	if err == nil || !strings.Contains(err.Error(), "objective child awaits recovery through its approved plan") {
		t.Fatalf("generic materializer did not refuse objective recovery: %v", err)
	}
	if _, found, err := fixture.store.Run(item.RunID); err != nil || found {
		t.Fatalf("generic recovery persisted an unauthorized flat run: found=%v err=%v", found, err)
	}
}

func TestObjectiveGraphDoesNotRelaxOrdinaryDuplicateIssueValidation(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	graph, err := compileObjectiveGraph(fixture.plan, fixture.issue, "claude", fixture.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantID, err := orchestration.WorkGraphID(fixture.plan.Subject.Repository, "claude", "objective-"+fixture.plan.ID)
	if err != nil || graph.AgentID != "claude" || graph.ID != wantID {
		t.Fatalf("selected agent did not determine objective graph identity: graph=%+v wantID=%q err=%v", graph, wantID, err)
	}
	if len(graph.Units) != 2 || graph.Units[0].Issue != graph.Units[1].Issue || graph.ObjectivePlan == nil {
		t.Fatalf("objective decomposition did not bind shared issue units: %+v", graph)
	}
	graph.ObjectivePlan = nil
	if err := graph.Validate(); err == nil {
		t.Fatal("ordinary graph accepted duplicate issue units without an immutable objective plan")
	}
	graph.ObjectivePlan = &domain.PlanRef{ID: fixture.plan.ID, Revision: fixture.plan.Revision, Digest: fixture.plan.Digest}
	if err := graph.Validate(); err != nil {
		t.Fatalf("bound objective graph rejected valid decomposition: %v", err)
	}
}

func TestObjectiveGraphIncludesOnlyMaterialProducerRoles(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	plan := fixture.plan
	plan.Stages = append(plan.Stages,
		domain.PlanStage{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer},
		domain.PlanStage{ID: "security-review", Kind: domain.StageAgent, Role: domain.RoleSecurityReviewer},
		domain.PlanStage{ID: "compose", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "compose admitted outputs",
			ExecutionKind: string(orchestration.ExecutionKindIntegrationCompose), DependsOn: []string{"backend", "frontend"}},
	)
	graph, err := compileObjectiveGraph(plan, fixture.issue, "codex", fixture.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Units) != 3 || graph.Units[2].ID != "compose" {
		t.Fatalf("objective graph treated a non-producing review role as a candidate unit: %+v", graph.Units)
	}
}

func TestObjectiveReviewerMayRestartWithoutReplacingCandidateUnit(t *testing.T) {
	stage := domain.PlanStage{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
		Objective: "Independently review the candidate.", InvocationMode: domain.InvocationModeMutating,
		RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis}}
	fixture := newPlanRunFixture(t, append(parallelStages()[:2], stage))
	if _, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent); err != nil {
		t.Fatal(err)
	}
	fixture.approve(t)
	resolution, err := fixture.service.Resolve(fixture.plan, mustReplayPlan(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok := resolution.Assignment("review")
	if !ok {
		t.Fatal("review responsibility did not resolve")
	}
	if err := fixture.store.PutPlanAssignment(fixture.plan.ID, fixture.plan.Revision, 1, assignment); err != nil {
		t.Fatal(err)
	}
	binding := RunPlanBinding{PlanID: fixture.plan.ID, Revision: fixture.plan.Revision, PlanDigest: fixture.plan.Digest,
		StageID: stage.ID, AssignmentID: assignment.ID, Generation: 1}
	outcome, err := fixture.reconciler.startObjectiveStage(context.Background(), fixture.runtime, fixture.plan, stage, binding)
	if err != nil {
		t.Fatalf("review generation was refused as a candidate-unit replacement: %v", err)
	}
	run, found, err := fixture.store.Run(outcome.RunID)
	if err != nil || !found || run.Plan == nil || run.Plan.Generation != 1 || run.Orchestration != nil {
		t.Fatalf("review restart lost its ordinary plan binding: %+v, %v", run, err)
	}
}

func TestObjectiveCompositionRetainsAlgorithmAndOriginalBase(t *testing.T) {
	stages := parallelStages()[:2]
	stages = append(stages, domain.PlanStage{ID: "compose", Kind: domain.StageAgent, Role: domain.RoleIntegrator,
		Objective: "Compose both independent candidates.", InvocationMode: domain.InvocationModeMutating,
		DependsOn: []string{"backend", "frontend"}, ExecutionKind: string(orchestration.ExecutionKindIntegrationCompose)})
	fixture := newPlanRunFixture(t, stages)
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	graph, found, err := fixture.store.WorkGraph(graphID)
	if err != nil || !found {
		t.Fatalf("objective graph missing: %v", err)
	}
	var compose orchestration.WorkUnit
	for _, unit := range graph.Units {
		if unit.ID == "compose" {
			compose = unit
		}
	}
	if compose.ExecutionKind != orchestration.ExecutionKindIntegrationCompose || len(compose.DependsOn) != 2 {
		t.Fatalf("composition degraded to provider execution: %+v", compose)
	}
	assignment := domain.AgentAssignment{Context: domain.ContextPack{UpstreamOutputs: []domain.UpstreamOutput{
		{StageID: "backend", Candidate: strings.Repeat("a", 40)},
		{StageID: "frontend", Candidate: strings.Repeat("b", 40)},
	}}}
	base, candidate, err := fixture.reconciler.objectiveStageBase(fixture.plan, stages[2], assignment)
	if err != nil || base != fixture.plan.Subject.Revision || candidate != nil {
		t.Fatalf("composition selected one divergent producer instead of original base: base=%q candidate=%+v err=%v", base, candidate, err)
	}
}

func TestObjectiveAdmittedHandoffMakesDependentUnitReady(t *testing.T) {
	fleet := newFleetFixture(t, 2)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	engine, err := NewEngineeringRuntime(fleet.deps)
	if err != nil {
		t.Fatal(err)
	}
	fleet.runtime = engine
	stages := parallelStages()[:2]
	stages[1].DependsOn = []string{"backend"}
	fixture := newPlanRunFixtureOn(t, fleet.phase8Fixture, stages, fleet.deps.ConfigDigest)
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	fixture.approve(t)
	report := fixture.reconcile(t)
	if len(report.Started) != 1 {
		t.Fatalf("producer did not start: %+v", report)
	}
	runID := report.Started[0].RunID
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found || run.Orchestration == nil {
		t.Fatalf("producer lacks batch: %+v, %v", run, err)
	}
	batch, found, err := fixture.store.OrchestrationBatch(run.Orchestration.BatchID)
	if err != nil || !found {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		if err := admitOrchestratedHandoff(fixture.store, fixture.stateDir, batch, batch.Items[0], fixture.clock.Now()); err != nil {
			t.Fatal(err)
		}
		view, err := WorkGraphStatus(fixture.store, fixture.stateDir, graphID, fixture.clock.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if view.unit(t, "frontend").State == orchestration.UnitReady {
			if view.unit(t, "backend").Output == nil {
				t.Fatal("dependent readiness lacks admitted producer output")
			}
			return
		}
	}
	t.Fatal("provider-backed producer did not admit a handoff and release dependent unit")
}

func TestObjectiveReproposalRetainsHistoryAndRefusesStartedGraph(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	graphID, err := fixture.service.BindObjectivePlan(fixture.plan, fixture.issue, fixture.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	next := fixture.plan
	next.Revision = 2
	previous := fixture.plan.Revision
	next.Provenance.PreviousRevision = &previous
	next.Objective = "Make the widget idempotent with explicit bounded stages."
	next.Digest, err = next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	newID, err := fixture.service.BindObjectivePlan(next, fixture.issue, fixture.service.DefaultAgent)
	if err != nil || newID != graphID {
		t.Fatalf("unstarted re-proposal did not retain graph identity: id=%q err=%v", newID, err)
	}
	old, found, err := fixture.store.WorkGraphRevision(graphID, 1)
	if err != nil || !found || old.ObjectivePlan == nil || old.ObjectivePlan.Revision != 1 || old.ObjectivePlan.Digest != fixture.plan.Digest {
		t.Fatalf("old immutable graph revision was lost: %+v, %v", old, err)
	}
	current, found, err := fixture.store.WorkGraph(graphID)
	if err != nil || !found || current.Revision != 2 || current.ObjectivePlan == nil || current.ObjectivePlan.Revision != 2 || current.ObjectivePlan.Digest != next.Digest {
		t.Fatalf("new graph does not bind revised proposal: %+v, %v", current, err)
	}
	origin := orchestration.BatchOrigin{GraphID: graphID, UnitID: "backend"}
	batchID, err := orchestration.WorkUnitBatchID("acme/repo", "codex", fixture.issue, origin)
	if err != nil {
		t.Fatal(err)
	}
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion, ID: batchID,
		Repository: "acme/repo", AgentID: "codex", CreatedAt: fixture.clock.Now(), Origin: &origin,
		Items: []orchestration.BatchItem{{Issue: fixture.issue, RunID: "run-interrupted-objective-reproposal"}}}
	if _, _, err := fixture.store.CreateOrchestrationBatch(batch); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Propose(context.Background(), ProposeInput{
		PlanID: fixture.plan.ID, Objective: "A repeated intake must preserve started objective authority.",
		Subject: next.Subject, Contract: planFixtureContract(fixture.phase8Fixture), Issue: fixture.issue,
		Model: domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: next.Subject},
	})
	if err == nil || !strings.Contains(err.Error(), "objective graph has started work") {
		t.Fatalf("proposal did not refuse a claimed objective child before persistence: %v", err)
	}
	latest, found, err := fixture.store.Plan(fixture.plan.ID)
	if err != nil || !found || latest.Revision != 2 || latest.Digest != next.Digest {
		t.Fatalf("refused repeated intake changed latest plan: %+v, %v", latest, err)
	}
	third := next
	third.Revision = 3
	previous = next.Revision
	third.Provenance.PreviousRevision = &previous
	third.Digest, err = third.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.PutPlanRevision(third); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.BindObjectivePlan(third, fixture.issue, fixture.service.DefaultAgent); err == nil || !strings.Contains(err.Error(), "objective graph has started work") {
		t.Fatalf("claimed batch did not preserve graph plan binding: %v", err)
	}
	current, found, err = fixture.store.WorkGraph(graphID)
	if err != nil || !found || current.Revision != 2 || current.ObjectivePlan.Revision != 2 {
		t.Fatalf("refused replacement changed graph: %+v, %v", current, err)
	}
}
