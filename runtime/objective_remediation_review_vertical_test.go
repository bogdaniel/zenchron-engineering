package runtime

// #471/#479/#480/#581/#474 vertical acceptance - the one cross-mechanism
// proof the baseline evidence review found missing: every mechanism below is
// already real, merged and independently tested in isolation
// (objective_workgraph_test.go for the #480 BindObjectivePlan bridge,
// remediation_end_to_end_test.go for #581's governed epoch,
// review_remediation_e2e_test.go for #474's head-bound review remediation,
// and objective_product_association_test.go for #471/#479's
// AssociateProductGraph), but nothing before this test ever drove a
// product-scoped, BindObjectivePlan-compiled WorkGraph through a real
// conflict and then through independent review remediation in one real
// Git/SQLite run. No new mechanism is added here: this reuses
// integrationUnitMarker (#475's own marker convention) and
// SupervisorReviewPort/ReconcileReviewRemediation (#474's own port) exactly
// as their own suites do.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// objectivePlanRemediationWorker is remediationWorker's (#581's own
// controlled worker, remediation_end_to_end_test.go) sibling for a
// BindObjectivePlan-compiled graph. A plan-bound producer's FIRST attempt
// carries its frozen stage assignment, so its Objective reliably names
// remediationWorker's marker; its remediation epoch (#581) is instead
// materialized through the generic WorkGraph machinery with no plan
// assignment at all (run.Plan is nil), so that attempt's Objective falls
// back to the plan's own generic text and can never carry a marker - which a
// plan-reconciler-started first attempt's own frozen stage Objective always
// does. The absence of a marker is therefore itself the fact that this is a
// later, remediation-epoch attempt.
type objectivePlanRemediationWorker struct{}

func (w *objectivePlanRemediationWorker) WritesTypedResults() bool { return true }

func (w *objectivePlanRemediationWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (w *objectivePlanRemediationWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	matched := false
	for _, marker := range []string{"a", "b"} {
		if !strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			continue
		}
		shared := filepath.Join(request.CandidateDir, "shared.go")
		if err := os.WriteFile(shared, []byte("package candidate\nconst Shared = \""+marker+"\"\n"), 0o600); err != nil {
			return ExecutionResult{}, err
		}
		matched = true
		break
	}
	if !matched {
		fixed := filepath.Join(request.CandidateDir, "remediated.go")
		if err := os.WriteFile(fixed, []byte("package candidate\n// remediated run "+request.RunID+"\n"), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	if request.HandoffPath == "" {
		return ExecutionResult{ProviderID: "objective-remediation-worker", Outcome: execution.Succeeded}, nil
	}
	return ExecutionResult{ProviderID: "objective-remediation-worker", Outcome: execution.Succeeded},
		os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
}

// objectiveIntegrationStages is parallelStages' shape swapped for two
// producers feeding a real integrator, with each producer's Objective
// carrying the exact marker objectivePlanRemediationWorker matches on for a
// first attempt - the plan-bound equivalent of integrationGraphUnits'
// issue-body marker.
func objectiveIntegrationStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Implement the backend half. " + integrationUnitMarker("a"),
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
			InvocationMode:       domain.InvocationModeMutating},
		{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Implement the frontend half. " + integrationUnitMarker("b"),
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
			InvocationMode:       domain.InvocationModeMutating},
		{ID: "integrate", Kind: domain.StageAgent, Role: domain.RoleIntegrator,
			Objective:      "Compose the backend and frontend candidates.",
			ExecutionKind:  string(orchestration.ExecutionKindIntegrationCompose),
			DependsOn:      []string{"backend", "frontend"},
			InvocationMode: domain.InvocationModeMutating},
	}
}

func TestObjectivePlanWorkGraphRecoversFromConflictThenPassesIndependentReviewRemediation(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	fleet.deps.Provider = &objectivePlanRemediationWorker{}
	fleet.runtime = fleet.newRuntime(fleet.deps)

	plan := newPlanRunFixtureOn(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest)
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	// #471/#479: this objective was proposed under a chosen product. The
	// association must still resolve to the SAME product after the full
	// conflict -> epoch -> reintegration -> independent review journey
	// below, not just at compile time.
	product := composeProduct(t, "storefront", 1, []string{plan.plan.Subject.Repository}, fleet.clock.Now())
	if _, _, err := fleet.store.AdoptProductRevision(product); err != nil {
		t.Fatal(err)
	}
	if err := plan.service.AssociateProductGraph(product.ID, graphID); err != nil {
		t.Fatal(err)
	}
	plan.approve(t)

	supervisor := fleet.supervisor()
	ctx := context.Background()
	var trace []objectiveConflictTick
	t.Cleanup(func() {
		if t.Failed() {
			logObjectiveConflictDiagnostics(t, fleet, plan, graphID, trace)
		}
	})

	// Stage A/B: approval -> two independent producer runs start -> a real
	// textual Git conflict parks the integrator on its unchanged
	// deterministic failure, exactly as the #581 fixture's own producers do.
	var integrateRunID string
	var parked bool
	for iteration := range 60 {
		planReport := plan.reconcile(t)
		report, err := supervisor.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
		graph := fleet.graph(graphID)
		trace = append(trace, objectiveConflictTick{Iteration: iteration, Plan: planReport, Supervisor: report, Graph: graph})
		if unit := graph.unit(t, "integrate"); unit.RunID != "" {
			integrateRunID = unit.RunID
		}
		if integrateRunID == "" {
			continue
		}
		events, err := fleet.store.Events(integrateRunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type != EventRunWaiting {
				continue
			}
			var payload dispositionRecord
			if err := decodeJSON(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Reason == ReasonDeterministicFailureUnchanged {
				parked = true
			}
		}
		if parked {
			break
		}
	}
	if !parked {
		t.Fatal("the plan-bound integrator never parked on a real unchanged deterministic conflict")
	}

	// Stage C: one more tick for delivery, then read which producer the
	// integrator's own Finding blames.
	if _, err := supervisor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	integrateRun, ok := storedRun(t, fleet.phase8Fixture, integrateRunID)
	if !ok || integrateRun.Orchestration == nil {
		t.Fatal("integrator run not found or not orchestrated")
	}
	messages, err := fleet.store.ScopeMessages(integrateRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var blamedID string
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding {
			blamedID = message.Route.Unit
		}
	}
	if blamedID != "backend" && blamedID != "frontend" {
		t.Fatalf("integrator's finding routes to %q, not one of the two plan-bound producers", blamedID)
	}
	blamedUnit := fleet.graph(graphID).unit(t, blamedID)
	blamedOriginalRunID := blamedUnit.RunID
	blamedRun, ok := storedRun(t, fleet.phase8Fixture, blamedUnit.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatalf("producer %s's run not found or not orchestrated", blamedID)
	}

	// Stage D: drive until the governed epoch_authorization decision_request
	// reaches the blamed producer's own inbox - #581's real mechanism, never
	// #508-A's in-run continuation (no run is waiting on its OWN decision
	// here; the request lives in a durable WorkGraph remediation seam).
	var requestID string
	for range 20 {
		plan.reconcile(t)
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
		own, err := fleet.store.ScopeMessages(blamedRun.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(own) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				requestID = message.ID
			}
		}
		if requestID != "" {
			break
		}
	}
	if requestID == "" {
		t.Fatal("no epoch_authorization decision_request ever appeared for the plan-bound blamed producer")
	}

	// An unauthorized resolution is refused (#581's own never-cut), then a
	// genuine operator approves - the ONLY path that may ever create a new
	// epoch/RunID; #508-A's own resumption path is never touched by this.
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, DecisionOutcomeKind: orchestration.DecisionSelectedOption,
		DecisionOutcomeValue: orchestration.EpochAuthorizationApprove,
	}); err == nil {
		t.Fatal("a decision resolution with no operator identity was accepted")
	}
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, Operator: "operator@example.com",
		DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: orchestration.EpochAuthorizationApprove,
	}); err != nil {
		t.Fatal(err)
	}

	// Stage E: the blamed producer's new epoch, then the integrator's own
	// automatic, policy-authorized reintegration, through the ordinary
	// scheduler alone.
	for range 60 {
		plan.reconcile(t)
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
		if unit := fleet.graph(graphID).unit(t, "integrate"); unit.Output != nil {
			break
		}
	}
	finalGraph := fleet.graph(graphID)
	integrateFinal := finalGraph.unit(t, "integrate")
	if integrateFinal.Output == nil {
		t.Fatalf("the plan-bound integrator never admitted a corrected handoff after remediation: integrate=%+v blamed(%s)=%+v",
			integrateFinal, blamedID, finalGraph.unit(t, blamedID))
	}
	blamedFinal := finalGraph.unit(t, blamedID)
	if blamedFinal.RunID == blamedOriginalRunID {
		t.Fatalf("blamed producer %s's run is unchanged after remediation: %s", blamedID, blamedFinal.RunID)
	}
	epochs, err := fleet.store.WorkUnitRemediationEpochs(graphID, blamedID)
	if err != nil || len(epochs) != 1 || epochs[0].Epoch != 2 {
		t.Fatalf("remediation epochs for %s = %+v (%v), want exactly one epoch-2 row", blamedID, epochs, err)
	}
	authorizations, err := fleet.store.RemediationAuthorizationsFor(graphID, blamedID)
	if err != nil || len(authorizations) != 1 || authorizations[0].AuthorizedBy != RemediationAuthorizedByDecision {
		t.Fatalf("remediation authorizations for %s = %+v (%v)", blamedID, authorizations, err)
	}
	integratorAuthorizations, err := fleet.store.RemediationAuthorizationsFor(graphID, "integrate")
	if err != nil || len(integratorAuthorizations) != 1 || integratorAuthorizations[0].AuthorizedBy != RemediationAuthorizedByPolicy {
		t.Fatalf("integrator's own remediation authorization = %+v (%v), want exactly one policy-authorized grant", integratorAuthorizations, err)
	}
	// Real composed evidence on disk, not a status flag: the integrator's
	// final candidate carries both the remediated producer's corrected file
	// and the unaffected producer's original shared.go - a genuine merge.
	dir := candidateDir(fleet.stateDir, integrateFinal.RunID)
	if _, err := os.Stat(filepath.Join(dir, "remediated.go")); err != nil {
		t.Fatalf("final composed candidate is missing the corrected file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.go")); err != nil {
		t.Fatalf("final composed candidate is missing shared.go: %v", err)
	}

	// Drive the integrator's own run to a published PR: with nothing
	// downstream of "integrate" in this plan, objectivePublicationDeferred
	// is false once it is complete, so the ordinary publication path runs.
	for range 20 {
		if _, err := fleet.runtime.Reconcile(ctx, integrateFinal.RunID); err != nil {
			t.Fatal(err)
		}
	}
	integrateState := fleet.state(integrateFinal.RunID)
	if integrateState.projection.PullRequest == nil {
		t.Fatal("the integrator's accepted candidate never reached a published PR")
	}
	repo := GitHubRepo{Owner: "acme", Name: "repo"}
	prNumber := integrateState.projection.PullRequest.Number
	h1 := integrateState.projection.CandidateRevision

	// #474's own port, reused verbatim, closes the journey: a fresh
	// independent review of the integrator's exact composed head.
	// ExecutionKindIntegrationCompose units never invoke a provider at all
	// (bindExecutionInvoke's guard refuses it, workgraph_integration_test.go) -
	// composition is deterministic from admitted inputs, so there is nothing
	// for a review-remediation re-invocation to run. #474's REQUEST_CHANGES ->
	// bounded re-invocation -> new-head re-review loop is already fully
	// proven, head-binding included, by TestReviewRemediationEndToEndH1BlockH2Approve
	// against an ordinary agent-invoked run; this closing step reuses the
	// SAME port and mechanism for the APPROVE path only, against the one
	// kind of unit this vertical journey's own final artifact actually is.
	approveDocument := `{"schema_version":"0.1","verdict":"accepted"}`
	reviewerProvider := &sequencedReviewProvider{documents: []string{approveDocument}}
	port := &SupervisorReviewPort{
		Store: fleet.store, GitHub: fleet.forge, ResolveAgent: e2eResolveAgent,
		ProviderFor: func(ResolvedAgent) (ExecutionProvider, error) { return reviewerProvider, nil },
		StateDir:    filepath.Join(fleet.root, "review-state-vertical"), Source: fleet.origin,
		ControllerID: "controller-a", Clock: fleet.clock,
	}
	outcome, admission, err := fleet.runtime.ReconcileReviewRemediation(ctx, port, repo, prNumber, "claude")
	if err != nil {
		t.Fatalf("ReconcileReviewRemediation: %v", err)
	}
	if outcome.Decision.Verdict != "approve" {
		t.Fatalf("expected the integrator's final composed candidate to be independently APPROVEd, got %+v", outcome.Decision)
	}
	if outcome.Decision.Subject.HeadSHA != h1 {
		t.Fatalf("expected the decision bound to the integrator's exact head %s, got %s", h1, outcome.Decision.Subject.HeadSHA)
	}
	if admission != nil {
		t.Fatalf("an APPROVE must never be routed through remediation admission, got %+v", admission)
	}
	owner, found, err := fleet.store.AssociatedProduct(graphID)
	if err != nil || !found || owner != product.ID {
		t.Fatalf("product association did not survive the conflict/epoch/review journey: owner=%q found=%t err=%v", owner, found, err)
	}
}

// Failure-only output preserves the admission/drive sequence without extra
// clock reads or changing the conflict loop's success criterion or bound.
type objectiveConflictTick struct {
	Iteration  int
	Plan       PlanTickReport
	Supervisor SupervisorReport
	Graph      WorkGraphView
}

func logObjectiveConflictDiagnostics(t *testing.T, fleet *fleetFixture, plan *planRunFixture, graphID string, trace []objectiveConflictTick) {
	t.Helper()
	log := func(label string, value any, err error) {
		raw, encodeErr := json.Marshal(value)
		t.Logf("conflict diagnostic %s: %s (read error=%v; encode error=%v)", label, raw, err, encodeErr)
	}
	log("graph identity", graphID, nil)
	view, err := plan.service.ViewRevision(plan.plan.ID, plan.plan.Revision)
	log("plan revision and approval", view, err)
	for _, tick := range trace {
		log("reconcile/tick", tick, nil)
	}
	runs, err := fleet.store.Runs()
	log("all run identities and final dispositions", runs, err)
	for _, run := range runs {
		events, err := fleet.store.Events(run.ID)
		log("events "+run.ID, events, err)
		for _, event := range events {
			if strings.Contains(string(event.Payload), ReasonDeterministicFailureUnchanged) {
				log("unchanged failure event (any disposition) "+run.ID, event, nil)
			}
		}
		if run.Orchestration != nil {
			messages, err := fleet.store.ScopeMessages(run.Orchestration.BatchID)
			log("messages "+run.ID, messages, err)
		}
		dir := candidateDir(fleet.stateDir, run.ID)
		content, err := os.ReadFile(filepath.Join(dir, "shared.go"))
		log("working shared.go "+run.ID, string(content), err)
		for _, args := range [][]string{
			{"log", "--oneline", "--all"},
			{"status", "--porcelain=v1"},
			{"diff", "--name-status", fleet.base, "HEAD"},
			{"show", "HEAD:shared.go"},
			{"ls-files", "--unmerged"},
		} {
			output, err := gitOutput(dir, args...)
			log("git "+run.ID+" "+strings.Join(args, " "), output, err)
		}
	}
}
