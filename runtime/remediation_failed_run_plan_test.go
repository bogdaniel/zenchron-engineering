package runtime

// #693: a plan-bound objective whose producer FAILED before any handoff is
// recovered through the existing governed epoch, end to end, and nothing the
// failure cost or recorded is lost on the way.

import (
	"context"
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

// failingFirstRunWorker writes each producer's own non-conflicting file. The
// FIRST run to carry the failing marker ends FAILED - after writing a perfect
// handoff that must never be read - and every later run of that stage succeeds.
type failingFirstRunWorker struct {
	mu          sync.Mutex
	failMarker  string
	firstRun    map[string]string
	invocations int
}

func (w *failingFirstRunWorker) WritesTypedResults() bool { return true }

func (w *failingFirstRunWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (w *failingFirstRunWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	w.mu.Lock()
	w.invocations++
	if w.firstRun == nil {
		w.firstRun = map[string]string{}
	}
	failing := false
	for _, marker := range []string{"a", "b"} {
		if !strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			continue
		}
		if _, claimed := w.firstRun[marker]; !claimed {
			w.firstRun[marker] = request.RunID
		}
		failing = marker == w.failMarker && w.firstRun[marker] == request.RunID
		if !failing {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, marker+".go"), []byte("package candidate\nconst V = \""+marker+"\"\n"), 0o600); err != nil {
				w.mu.Unlock()
				return ExecutionResult{}, err
			}
		}
	}
	w.mu.Unlock()
	if request.HandoffPath != "" {
		if err := os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	if failing {
		return ExecutionResult{ProviderID: "failing-first-run-worker", Outcome: execution.Failed,
			Failure: &ProviderFailure{Classification: FailureUnknown}}, nil
	}
	return ExecutionResult{ProviderID: "failing-first-run-worker", Outcome: execution.Succeeded}, nil
}

type failedPlanBoundProducer struct {
	fleet      *fleetFixture
	plan       *planRunFixture
	supervisor *Supervisor
	graphID    string
	failedRun  string
	requestID  string
}

func (g *failedPlanBoundProducer) step(t *testing.T) {
	t.Helper()
	g.plan.reconcile(t)
	if _, err := g.supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.fleet.clock.advance(61 * time.Second)
}

func (g *failedPlanBoundProducer) resolve(t *testing.T, value string) {
	t.Helper()
	if _, err := g.supervisor.ResolveDecision(ControlRequest{
		DecisionID: g.requestID, Operator: "operator@example.com",
		DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: value,
	}); err != nil {
		t.Fatal(err)
	}
}

// driveToFailedPlanBoundProducer runs the approved plan until the `frontend`
// producer has failed with no handoff, its sibling's handoff is admitted, the
// integrator is blocked, and the failed-run request is live and unanswered.
func driveToFailedPlanBoundProducer(t *testing.T) (*failedPlanBoundProducer, *failingFirstRunWorker) {
	t.Helper()
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	worker := &failingFirstRunWorker{failMarker: "b"}
	fleet.deps.Provider = worker
	fleet.runtime = fleet.newRuntime(fleet.deps)
	// Three stages plus ONE epoch: the allowance is exactly what the recovery needs.
	envelope := domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 3, MaxProviderInvocations: 12}
	plan := newPlanRunFixtureWith(t, fleet.phase8Fixture, objectiveIntegrationStages(), fleet.deps.ConfigDigest, envelope)
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	plan.approve(t)
	supervisor := fleet.supervisor()
	supervisor.deps.Plans = plan.service
	g := &failedPlanBoundProducer{fleet: fleet, plan: plan, supervisor: supervisor, graphID: graphID}

	for range 80 {
		g.step(t)
		graph := fleet.graph(graphID)
		frontend := graph.unit(t, "frontend")
		if frontend.RunID == "" || frontend.State != orchestration.UnitState(orchestration.ItemFailed) {
			continue
		}
		g.failedRun = frontend.RunID
		run, ok := storedRun(t, fleet.phase8Fixture, frontend.RunID)
		if !ok || run.Orchestration == nil {
			t.Fatal("the failed producer's run is not orchestrated")
		}
		messages, err := fleet.store.ScopeMessages(run.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				g.requestID = message.ID
			}
		}
		if g.requestID != "" {
			return g, worker
		}
	}
	for _, unit := range fleet.graph(graphID).Units {
		t.Logf("unit %s: %s run=%s reason=%s", unit.UnitID, unit.State, unit.RunID, unit.Reason)
		if run, ok := storedRun(t, fleet.phase8Fixture, unit.RunID); ok {
			t.Logf("  run %s disposition=%s reason=%s", unit.UnitID, run.Disposition, run.Reason)
		}
	}
	t.Fatal("the plan-bound producer never failed into a live epoch_authorization request")
	return nil, nil
}

func TestAnApprovedPlanBoundFailedProducerRecoversAndReleasesTheIntegrator(t *testing.T) {
	g, worker := driveToFailedPlanBoundProducer(t)
	fleet, planID := g.fleet, g.plan.plan.ID

	// Before any decision: the sibling's handoff is admitted, the failure is
	// settled on the stage, the integrator waits, and nothing was granted.
	graph := fleet.graph(g.graphID)
	if backend := graph.unit(t, "backend"); backend.State != orchestration.UnitState(orchestration.ItemCompleted) || backend.Output == nil {
		t.Fatalf("the sibling producer is %s with output %v", backend.State, backend.Output)
	}
	if integrate := graph.unit(t, "integrate"); integrate.RunID != "" || integrate.Output != nil {
		t.Fatalf("the integrator started behind a failed dependency: %+v", integrate)
	}
	before, err := fleet.store.ReplayPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Stages["frontend"].State != PlanStageFailed || before.Stages["frontend"].RunID != g.failedRun {
		t.Fatalf("the failed stage before approval = %+v", before.Stages["frontend"])
	}
	invocationsBefore := worker.invocations

	g.resolve(t, orchestration.EpochAuthorizationApprove)
	for range 80 {
		g.step(t)
		if fleet.graph(g.graphID).unit(t, "integrate").Output != nil {
			break
		}
	}
	graph = fleet.graph(g.graphID)
	frontend, integrate := graph.unit(t, "frontend"), graph.unit(t, "integrate")
	if frontend.RunID == g.failedRun || frontend.State != orchestration.UnitState(orchestration.ItemCompleted) || frontend.Output == nil {
		t.Fatalf("the recovery epoch is %s on %s with output %v", frontend.State, frontend.RunID, frontend.Output)
	}
	if integrate.Output == nil {
		t.Fatalf("the integrator was never released: %s %s", integrate.State, integrate.Reason)
	}

	after, err := fleet.store.ReplayPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	// The stage follows the new run; the original run is history, listed once.
	if got := after.Stages["frontend"]; got.State != PlanStageCompleted || got.RunID != frontend.RunID {
		t.Fatalf("the recovered stage = %+v, want completed on %s", got, frontend.RunID)
	}
	listed := map[string]int{}
	for _, run := range after.ChildRuns() {
		listed[run]++
	}
	if listed[g.failedRun] != 1 || listed[frontend.RunID] != 1 {
		t.Fatalf("child runs %v do not list the failed run and its replacement once each", after.ChildRuns())
	}
	retired := false
	for _, run := range after.RetiredRuns {
		retired = retired || run == g.failedRun
	}
	if !retired {
		t.Fatalf("the failed run is not retained as history: %v", after.RetiredRuns)
	}
	// The failure itself is still in the journal.
	events, err := fleet.store.PlanEvents(planID)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, event := range events {
		if event.Type != EventPlanStageSettled {
			continue
		}
		var settled PlanStageSettledPayload
		if err := decodeJSON(event.Payload, &settled); err != nil {
			t.Fatal(err)
		}
		if settled.StageID == "frontend" && settled.Outcome == string(Failed) {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("the journal holds %d failed settlements of the stage, want the original one", failures)
	}
	if run, ok := storedRun(t, fleet.phase8Fixture, g.failedRun); !ok || run.Disposition != Failed {
		t.Fatalf("the failed run was rewritten: %+v", run)
	}
	// The recovery spent exactly one more child run, under the approved ceiling,
	// and the failed run's own spend is still on the stage.
	if after.Consumed.ChildRuns != 4 || after.StageConsumed["frontend"].ChildRuns != 2 {
		t.Fatalf("child runs consumed: plan %d, stage %d; want 4 and 2", after.Consumed.ChildRuns, after.StageConsumed["frontend"].ChildRuns)
	}
	if after.Consumed.ChildRuns > g.plan.plan.BudgetEnvelope.MaxChildRuns ||
		after.Consumed.ProviderInvocations > g.plan.plan.BudgetEnvelope.MaxProviderInvocations {
		t.Fatalf("the recovery exceeded the approved envelope: %+v", after.Consumed)
	}
	if after.StageConsumed["frontend"].ProviderInvocations < before.StageConsumed["frontend"].ProviderInvocations ||
		worker.invocations <= invocationsBefore {
		t.Fatalf("the recovery spent no provider invocation of its own: before %d, after %d", invocationsBefore, worker.invocations)
	}
	// One decision, one grant: the unit's ceiling rose by exactly one epoch.
	authorizations, err := fleet.store.RemediationAuthorizationsFor(g.graphID, "frontend")
	if err != nil || len(authorizations) != 1 || authorizations[0].ProposedEpoch != 2 || authorizations[0].AuthorizedBy != RemediationAuthorizedByDecision {
		t.Fatalf("authorizations = %+v (%v)", authorizations, err)
	}
}

func TestARejectedPlanBoundFailedProducerLeavesThePlanBlocked(t *testing.T) {
	g, _ := driveToFailedPlanBoundProducer(t)
	fleet, planID := g.fleet, g.plan.plan.ID
	before, err := fleet.store.ReplayPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	g.resolve(t, orchestration.EpochAuthorizationReject)
	for range 30 {
		g.step(t)
	}
	after, err := fleet.store.ReplayPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Stages["frontend"]; got.State != PlanStageFailed || got.RunID != g.failedRun {
		t.Fatalf("a rejection moved the stage: %+v", got)
	}
	// Wall time of the failed run may still be attributed as it winds down; what
	// a rejection must not do is start anything.
	if after.Consumed.ChildRuns != before.Consumed.ChildRuns || after.Consumed.ProviderInvocations != before.Consumed.ProviderInvocations {
		t.Fatalf("a rejection spent budget: %+v -> %+v", before.Consumed, after.Consumed)
	}
	if epochs, err := fleet.store.WorkUnitRemediationEpochs(g.graphID, "frontend"); err != nil || len(epochs) != 0 {
		t.Fatalf("a rejection produced epochs %+v (%v)", epochs, err)
	}
	if authorizations, err := fleet.store.RemediationAuthorizationsFor(g.graphID, "frontend"); err != nil || len(authorizations) != 0 {
		t.Fatalf("a rejection produced authorizations %+v (%v)", authorizations, err)
	}
	if integrate := fleet.graph(g.graphID).unit(t, "integrate"); integrate.RunID != "" || integrate.Output != nil {
		t.Fatalf("the integrator moved after a rejection: %+v", integrate)
	}
}

// An approval that arrives after the unit has already moved to a later epoch is
// about work that was superseded. The request has no handoff subject to go
// stale, so the grant itself must refuse it.
func TestAStaleFailedRunApprovalGrantsNoLaterEpoch(t *testing.T) {
	g := driveToFailedProducerRequest(t)
	// The unit moves on without this decision: a policy grant starts epoch 2.
	if err := admitAutomaticReintegrationAuthorization(g.fixture.store, g.graphID, "c", g.fixture.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for range 80 {
		if _, err := g.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		g.fixture.clock.advance(61 * time.Second)
		if epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c"); err == nil && len(epochs) == 1 &&
			g.fixture.graph(g.graphID).unit(t, "c").State == orchestration.UnitState(orchestration.ItemCompleted) {
			break
		}
	}
	if epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c"); err != nil || len(epochs) != 1 || epochs[0].Epoch != 2 {
		t.Fatalf("the unit never reached epoch 2: %+v (%v)", epochs, err)
	}

	g.resolve(t, orchestration.EpochAuthorizationApprove)
	for range 20 {
		if _, err := g.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		g.fixture.clock.advance(61 * time.Second)
	}
	authorizations, err := g.fixture.store.RemediationAuthorizationsFor(g.graphID, "c")
	if err != nil {
		t.Fatal(err)
	}
	for _, authorization := range authorizations {
		if authorization.ProposedEpoch > 2 {
			t.Fatalf("a stale approval granted epoch %d: %+v", authorization.ProposedEpoch, authorization)
		}
	}
	if epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c"); err != nil || len(epochs) != 1 {
		t.Fatalf("a stale approval started another epoch: %+v (%v)", epochs, err)
	}
}
