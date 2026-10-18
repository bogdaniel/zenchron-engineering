package runtime

// #480 acceptance: ONE governed journey for a dynamically composed team. A
// product's approved objective plan compiles to two parallel producers, a
// deterministic integrator and an independent reviewer. One producer asks a
// human decision through #508; a newer configuration and newer knowledge land
// while it waits; the store is reopened; the operator's resolution resumes the
// SAME run. Every worker sees only its own admitted context, and the durable
// record names the objective, the pins, the handoffs and the delivered decision.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// journeyWorker writes one file per run and a valid handoff. It records every
// request, and writes a decision document into the runtime-owned message slot
// of the runs it was told to question, on their first invocation only.
type journeyWorker struct {
	mu       sync.Mutex
	asks     map[string]string
	requests map[string]ExecutionRequest
	attempts map[string]int
	// reviewer answers the invocations that carry a verdict channel, whichever
	// engine the host drove them through.
	reviewer *recordingReviewer
}

func (w *journeyWorker) WritesTypedResults() bool { return true }

func (w *journeyWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven}
}

func (w *journeyWorker) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if request.ReviewerResultPath != "" {
		return w.reviewer.Execute(ctx, request)
	}
	w.mu.Lock()
	w.requests[request.RunID] = request
	w.attempts[request.RunID]++
	document := w.asks[request.RunID]
	attempt := w.attempts[request.RunID]
	w.mu.Unlock()
	if document != "" && attempt == 1 {
		// A worker that has a question and nothing else to report: it changes
		// nothing, so no candidate or plan stage can settle before the answer.
		return ExecutionResult{ProviderID: "journey-worker", Outcome: execution.Succeeded},
			os.WriteFile(request.MessagePath, []byte(document), 0o600)
	}
	file := filepath.Join(request.CandidateDir, fmt.Sprintf("work-%s-%d.go", request.RunID, attempt))
	if err := os.WriteFile(file, []byte("package candidate\n"), 0o600); err != nil {
		return ExecutionResult{}, err
	}
	if request.HandoffPath != "" {
		if err := os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{ProviderID: "journey-worker", Outcome: execution.Succeeded}, nil
}

func (w *journeyWorker) reached(runID string) (ExecutionRequest, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests[runID], w.attempts[runID]
}

// recordingReviewer is the second agent: it writes real structured verdicts
// through the runtime-owned result path and remembers what it was handed.
type recordingReviewer struct {
	*FakeReviewerProvider
	mu      sync.Mutex
	request ExecutionRequest
	calls   int
}

func (r *recordingReviewer) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	r.mu.Lock()
	r.request = request
	r.calls++
	r.mu.Unlock()
	return r.FakeReviewerProvider.Execute(ctx, request)
}

func (r *recordingReviewer) reached() (ExecutionRequest, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.request, r.calls
}

func journeyStages() []domain.PlanStage {
	code := []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis}
	return []domain.PlanStage{
		{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "Implement the backend half.",
			RequiresCapabilities: code, InvocationMode: domain.InvocationModeMutating},
		{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "Implement the frontend half.",
			RequiresCapabilities: code, InvocationMode: domain.InvocationModeMutating},
		{ID: "integrate", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "Compose the candidates.",
			ExecutionKind: string(orchestration.ExecutionKindIntegrationCompose), DependsOn: []string{"backend", "frontend"},
			InvocationMode: domain.InvocationModeMutating},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer, Objective: "Review the composed candidate.",
			DependsOn: []string{"integrate"}, RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis},
			InvocationMode: domain.InvocationModeMutating,
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceVendorFamily, DifferentFrom: []string{"backend", "frontend", "integrate"},
			}},
	}
}

func TestADynamicTeamRunsOneGovernedJourneyWithIsolatedProductContextAndADurableHumanDecision(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	reviewerProvider := &recordingReviewer{FakeReviewerProvider: NewFakeReviewerProvider(ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted, Reason: "every obligation was observed"})}
	worker := &journeyWorker{reviewer: reviewerProvider, asks: map[string]string{}, requests: map[string]ExecutionRequest{}, attempts: map[string]int{}}
	fleet.deps.Provider = worker
	fleet.runtime = fleet.newRuntime(fleet.deps)
	f := &productUnitFixture{fleetFixture: fleet, at: time.Unix(1700000000, 0).UTC()}
	owner := f.seedProduct("storefront", map[string]string{"owner": "STOREFRONT"})
	rival := f.seedProduct("checkout", map[string]string{"owner": "RIVAL-ONLY"})
	f.widelyPromote(owner.ID, "CLAIM-STOREFRONT")
	f.widelyPromote(rival.ID, "CLAIM-RIVAL-ONLY")

	plan := newNamedPlanRunFixture(t, fleet.phase8Fixture, "plan-team", journeyStages(), fleet.deps.ConfigDigest, defaultPlanFixtureEnvelope)
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.service.AssociateProductGraph(owner.ID, graphID); err != nil {
		t.Fatal(err)
	}
	f.widelyPromote(rival.ID, "RIVAL-AUDIENCE-NOISE")
	units := []string{"backend", "frontend", "integrate"}
	entries := map[string]string{}
	for _, unit := range units {
		entries[unit] = f.audiencedPromote(owner.ID, graphID, unit, "PRIVATE-"+strings.ToUpper(unit)).ID
		f.rawDiscovery(owner.ID, graphID, unit, "RAW-"+strings.ToUpper(unit))
	}
	plan.approve(t)

	supervisor := fleet.supervisor()
	tick := func() {
		t.Helper()
		plan.reconcile(t)
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
	}
	runOf := func(stage string) string { return fleet.graph(graphID).unit(t, stage).RunID }

	// Admission only: both producers get a pinned batch and a run; nothing runs.
	for range 5 {
		plan.reconcile(t)
		if runOf("backend") != "" && runOf("frontend") != "" {
			break
		}
	}
	backendRun, frontendRun := runOf("backend"), runOf("frontend")
	if backendRun == "" || frontendRun == "" {
		t.Fatal("the producers were never admitted")
	}
	if _, attempts := worker.reached(backendRun); attempts != 0 {
		t.Fatal("a producer executed before the world changed")
	}
	worker.asks[backendRun] = decisionWaitDocument(t)

	// The backend asks a human; the frontend completes; the integrator waits.
	var open []orchestration.EngineeringMessage
	for range 40 {
		tick()
		open, _ = fleet.store.OpenDecisionRequestsForRun(backendRun)
		if len(open) == 1 && fleet.graph(graphID).unit(t, "frontend").Output != nil {
			break
		}
	}
	if len(open) != 1 || fleet.graph(graphID).unit(t, "frontend").Output == nil {
		t.Fatalf("expected a parked backend and a finished frontend: open=%d", len(open))
	}
	if fleet.graph(graphID).unit(t, "integrate").RunID != "" {
		t.Fatal("the integrator started before its producer's human decision was resolved")
	}

	// The world moves while the human decides, and the process restarts.
	f.adoptConfiguration(owner.ID, 2, map[string]string{"owner": "NEWER-REVISION"})
	f.widelyPromote(owner.ID, "PROMOTED-AFTER-ADMISSION")
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption,
		DecisionOutcomeValue: "approach-b", Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}
	store, _ := reopen(t, fleet.phase8Fixture)
	plan.service.Store, plan.reconciler.Store = store, store
	plan.reconciler.Service = plan.service
	supervisor = fleet.supervisor()
	reviewerDeps := fleet.deps
	reviewerDeps.Provider = reviewerProvider
	reviewerDeps.Agent = ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}
	plan.engines["claude"] = fleet.newRuntime(reviewerDeps)

	reviewRun := ""
	for range 80 {
		tick()
		snapshot, err := fleet.store.ReplayPlan(plan.plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reviewRun = snapshot.Stages["review"].RunID; reviewRun != "" {
			if _, calls := reviewerProvider.reached(); calls > 0 {
				break
			}
		}
	}
	if _, calls := reviewerProvider.reached(); calls == 0 {
		t.Fatal("the reviewer never reached its worker")
	}

	// Producers: own admitted context, own private knowledge, nothing else's.
	for _, unit := range []string{"backend", "frontend"} {
		runID := runOf(unit)
		request, attempts := worker.reached(runID)
		wantAttempts := 1
		if unit == "backend" {
			wantAttempts = 2 // the question, then exactly one decision resumption
		}
		if attempts != wantAttempts {
			t.Fatalf("%s reached the worker %d times, want %d", unit, attempts, wantAttempts)
		}
		prompt := providerPrompt(request)
		delivered := productOf(t, request)
		if delivered.ProductID != owner.ID || delivered.ConfigurationRevision != 1 {
			t.Fatalf("%s ran as %s revision %d", unit, delivered.ProductID, delivered.ConfigurationRevision)
		}
		for _, want := range []string{"STOREFRONT", "CLAIM-STOREFRONT", "PRIVATE-" + strings.ToUpper(unit)} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s lacks its own admitted context %q", unit, want)
			}
		}
		requireAbsent(t, prompt, "RIVAL", "NEWER-REVISION", "PROMOTED-AFTER-ADMISSION", "RAW-")
		for _, other := range units {
			if other != unit {
				requireAbsent(t, prompt, "PRIVATE-"+strings.ToUpper(other))
			}
		}
	}
	if request, _ := worker.reached(runOf("backend")); !strings.Contains(providerPrompt(request), "approach-b") {
		t.Fatal("the resumed backend did not receive the human's resolution")
	}

	// The integrator is deterministic: no worker, but its admission is pinned
	// to its own private knowledge and to no other unit's.
	view := fleet.graph(graphID)
	integrate := view.unit(t, "integrate")
	if _, attempts := worker.reached(integrate.RunID); attempts != 0 || integrate.Output == nil {
		t.Fatalf("the integrator must complete without a worker: attempts=%d output=%v", attempts, integrate.Output)
	}

	// Reviewer: independent run, admitted after the update, no private or raw
	// knowledge of any producer or of the integrator, none of the rival's.
	request, _ := reviewerProvider.reached()
	prompt := providerPrompt(request)
	if delivered := productOf(t, request); delivered.ProductID != owner.ID {
		t.Fatalf("the reviewer ran as %s", delivered.ProductID)
	}
	if reviewRun == integrate.RunID || reviewRun == runOf("backend") || reviewRun == runOf("frontend") {
		t.Fatal("the reviewer shares a run with a producer")
	}
	requireAbsent(t, prompt, "PRIVATE-", "RAW-", "RIVAL", "approach-b")

	// Independence is real: the reviewer's frozen assignment is a different
	// vendor family and agent from every stage it reviews, and the run that
	// executed it is that other agent's.
	snapshot, err := fleet.store.ReplayPlan(plan.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := snapshot.Stages["review"]
	for _, unit := range units {
		produced := snapshot.Stages[unit]
		if reviewer.VendorFamily == produced.VendorFamily || reviewer.AgentID == produced.AgentID {
			t.Fatalf("the reviewer (%s/%s) is not independent of %s (%s/%s)", reviewer.AgentID, reviewer.VendorFamily, unit, produced.AgentID, produced.VendorFamily)
		}
	}

	if row, _ := storedRun(t, fleet.phase8Fixture, reviewRun); row.AgentID != reviewer.AgentID {
		t.Fatalf("the reviewer's run executed as %s, not its assigned %s", row.AgentID, reviewer.AgentID)
	}

	// Provenance: objective plan -> graph -> pinned batch -> run -> handoff, and
	// the delivered decision on the run that asked.
	graph, _, err := fleet.store.WorkGraph(graphID)
	if err != nil || graph.ObjectivePlan == nil || graph.ObjectivePlan.Digest != plan.plan.Digest {
		t.Fatalf("the graph does not bind the approved plan: %+v %v", graph.ObjectivePlan, err)
	}
	for _, unit := range units {
		u := view.unit(t, unit)
		// The integrator is admitted when its producers finish, after the update,
		// so it starts from the revision then current; the producers never move.
		wantRevision := 1
		if unit == "integrate" {
			wantRevision = 2
		}
		batch, found, err := fleet.store.OrchestrationBatch(u.BatchID)
		if err != nil || !found || batch.Product == nil || batch.Product.ProductID != owner.ID || batch.Product.ConfigurationRevision != wantRevision {
			t.Fatalf("%s has no durable pin: %+v %v", unit, batch.Product, err)
		}
		if batch.Origin == nil || batch.Origin.GraphID != graphID || batch.Origin.UnitID != unit {
			t.Fatalf("%s's batch does not name its graph and unit: %+v", unit, batch.Origin)
		}
		holds := map[string]bool{}
		for _, id := range batch.Product.KnowledgeIDs {
			holds[id] = true
		}
		for other, id := range entries {
			if holds[id] != (other == unit) {
				t.Fatalf("%s's pin holds %s's private entry = %t", unit, other, holds[id])
			}
		}
		run, ok := storedRun(t, fleet.phase8Fixture, u.RunID)
		if !ok || run.Plan == nil || run.Plan.PlanDigest != plan.plan.Digest || run.Plan.StageID != unit || u.Output == nil || u.Output.RunID != u.RunID {
			t.Fatalf("%s's run or handoff is not bound to the approved plan: %+v", unit, run.Plan)
		}
	}
	if len(integrate.Inputs) != 2 {
		t.Fatalf("the integrator consumed %d inputs, want the two producers' handoffs", len(integrate.Inputs))
	}
	for _, input := range integrate.Inputs {
		if produced := view.unit(t, input.UnitID).Output; produced == nil || input.HandoffID != produced.HandoffID {
			t.Fatalf("the integrator's input %s is not the admitted handoff", input.UnitID)
		}
	}
	reviewRow, _ := storedRun(t, fleet.phase8Fixture, reviewRun)
	if reviewRow.Plan == nil || reviewRow.Plan.Product == nil || reviewRow.Plan.Product.Pin.ProductID != owner.ID ||
		reviewRow.Plan.Product.Pin.ConfigurationRevision != 2 {
		t.Fatalf("the reviewer's run has no durable pin of the revision it was admitted with: %+v", reviewRow.Plan)
	}
	// The reviewer's verdict about the integrator's exact head is admitted, which
	// is what releases publication.
	for range 40 {
		if snap, _ := fleet.store.ReplayPlan(plan.plan.ID); snap.Stages["review"].Review != nil {
			break
		}
		tick()
	}
	snapshot, err = fleet.store.ReplayPlan(plan.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review := snapshot.Stages["review"].Review; review == nil || review.Verdict != StageReviewAccepted ||
		review.Candidate != integrate.Output.CandidateRevision {
		t.Fatalf("no accepting verdict is bound to the integrator's admitted head: %+v", snapshot.Stages["review"].Review)
	}
	// The final candidate: the integrator's admitted head is what is published.
	for range 20 {
		plan.reconcile(t)
		if _, err := fleet.runtime.Reconcile(context.Background(), integrate.RunID); err != nil {
			t.Fatal(err)
		}
	}
	final := fleet.state(integrate.RunID)
	if final.projection.PullRequest == nil || final.projection.CandidateRevision != integrate.Output.CandidateRevision {
		t.Fatalf("the integrator candidate %s was not published: deferred=%v reason=%v", integrate.Output.CandidateRevision, final.publicationDeferred, final.snapshot.Reason)
	}
	events, err := fleet.store.Events(backendRun)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, event := range events {
		if event.Type == EventDecisionsDelivered && strings.Contains(string(event.Payload), open[0].ID) {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("the backend run records %d deliveries of its resolved decision, want 1", delivered)
	}
}
