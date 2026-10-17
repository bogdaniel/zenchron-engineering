package runtime

// #480 dynamic orchestration acceptance: a model-PROPOSED team, not a
// hand-built plan, travels the production path:
//
//	registered planner agent -> InvokePlanner (non-mutating, verified)
//	  -> PlanService.Propose (planning.Compile: vocabulary, budget envelope,
//	     policy, independence, ratchet) -> BindObjectivePlan (WorkGraph)
//	  -> operator approval -> plan reconciler -> supervisor -> runs.
//
// The other plan tests claim a hand-built plan straight into the store, which
// is the right isolation for the reconciler but never exercises the compiler
// on a variable team. Nothing here adds a mechanism: it drives the existing
// planner, compiler, graph, reconciler and supervisor.

import (
	"context"
	"encoding/json"
	"errors"
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

// proposeTeam runs the planner (a scripted registered agent) and then the real
// compiler, over fleet's store. It returns the harness whose plan is the
// compiler's output, plus the planner's output.
func proposeTeam(t *testing.T, base *phase8Fixture, config ConfigDigest, answer string) (*planRunFixture, PlannerOutput, error) {
	t.Helper()
	input, _ := plannerFixture(t, answer)
	output, err := InvokePlanner(context.Background(), input)
	if err != nil {
		t.Fatalf("planning invocation: %v", err)
	}
	fixture := newPlanRunHarness(base, config)
	subject := domain.Subject{Repository: "acme/repo", Revision: base.base}
	plan, err := fixture.service.Propose(context.Background(), ProposeInput{
		PlanID: "plan-fixture", Objective: "Make the widget idempotent.", Subject: subject,
		Contract: planFixtureContract(base), Issue: base.issue,
		Model:    domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject},
		Reasoned: output.Stages, Reasoning: &output.Reasoning,
	})
	fixture.plan = plan
	return fixture, output, err
}

// plannerAnswer renders stages the way a registered planner agent answers: one
// fenced JSON object.
func plannerAnswer(t *testing.T, stages []domain.PlanStage) string {
	t.Helper()
	type proposed struct {
		ID            string   `json:"id"`
		Kind          string   `json:"kind"`
		Role          string   `json:"role,omitempty"`
		Objective     string   `json:"objective,omitempty"`
		ExecutionKind string   `json:"execution_kind,omitempty"`
		DependsOn     []string `json:"depends_on,omitempty"`
		Capabilities  []string `json:"requires_capabilities,omitempty"`
	}
	var out []proposed
	for _, stage := range stages {
		entry := proposed{ID: stage.ID, Kind: string(stage.Kind), Role: string(stage.Role), Objective: stage.Objective,
			ExecutionKind: stage.ExecutionKind, DependsOn: stage.DependsOn}
		for _, capability := range stage.RequiresCapabilities {
			entry.Capabilities = append(entry.Capabilities, string(capability))
		}
		out = append(out, entry)
	}
	raw, err := json.Marshal(map[string]any{"stages": out, "notes": "scripted planner team"})
	if err != nil {
		t.Fatal(err)
	}
	return "```json\n" + string(raw) + "\n```"
}

// producerStage is one planner-proposed material producer. The marker names the
// file its worker writes, so independent producers never conflict.
func producerStage(id, marker string) domain.PlanStage {
	return domain.PlanStage{ID: id, Kind: domain.StageAgent, Role: domain.RoleImplementer,
		Objective:            "Implement " + id + ". " + integrationUnitMarker(marker),
		RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis}}
}

func integratorStage(dependsOn ...string) domain.PlanStage {
	return domain.PlanStage{ID: "integrate", Kind: domain.StageAgent, Role: domain.RoleIntegrator,
		Objective: "Compose the candidates.", ExecutionKind: string(orchestration.ExecutionKindIntegrationCompose), DependsOn: dependsOn}
}

// dynamicTeamWorker is a provider that writes one file per producer and a valid
// handoff. It records the product context and objective of every invocation.
type dynamicTeamWorker struct {
	mu         sync.Mutex
	objectives []string
	// overlap, when positive, makes each producer wait until that many
	// producers are executing at the same moment. A serial scheduler times the
	// first one out, so concurrency is observed rather than inferred.
	overlap  int
	arrived  int
	released chan struct{}
}

func (w *dynamicTeamWorker) WritesTypedResults() bool { return true }

func (w *dynamicTeamWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven}
}

func (w *dynamicTeamWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	w.mu.Lock()
	w.objectives = append(w.objectives, request.Objective)
	if w.overlap > 0 {
		if w.released == nil {
			w.released = make(chan struct{})
		}
		w.arrived++
		if w.arrived == w.overlap {
			close(w.released)
		}
	}
	released := w.released
	w.mu.Unlock()
	if w.overlap > 0 {
		select {
		case <-released:
		case <-time.After(20 * time.Second):
			return ExecutionResult{}, errors.New("producers did not execute concurrently")
		}
	}
	for _, marker := range []string{"a", "b", "c"} {
		if strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			body := "package candidate\nconst " + strings.ToUpper(marker) + " = 1\n"
			if err := os.WriteFile(filepath.Join(request.CandidateDir, marker+".go"), []byte(body), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
	}
	result := ExecutionResult{ProviderID: "dynamic-team-worker", Outcome: execution.Succeeded}
	if request.HandoffPath == "" {
		return result, nil
	}
	return result, os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
}

func (w *dynamicTeamWorker) invocations() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.objectives)
}

// dynamicTeam is one planner-proposed, compiler-admitted, bound and approved
// objective, ready to be driven by the real reconciler and supervisor.
type dynamicTeam struct {
	fleet      *fleetFixture
	plan       *planRunFixture
	worker     *dynamicTeamWorker
	proposed   PlannerOutput
	graphID    string
	supervisor *Supervisor
}

func newDynamicTeam(t *testing.T, stages []domain.PlanStage, overlap int) *dynamicTeam {
	t.Helper()
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	worker := &dynamicTeamWorker{overlap: overlap}
	fleet.deps.Provider = worker
	fleet.runtime = fleet.newRuntime(fleet.deps)
	plan, proposed, err := proposeTeam(t, fleet.phase8Fixture, fleet.deps.ConfigDigest, plannerAnswer(t, stages))
	if err != nil {
		t.Fatalf("the compiler refused the planner's team: %v", err)
	}
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	plan.approve(t)
	return &dynamicTeam{fleet: fleet, plan: plan, worker: worker, proposed: proposed, graphID: graphID, supervisor: fleet.supervisor()}
}

// drive runs the production loop - the plan reconciler, then one supervisor
// tick - until done reports true, and returns every plan pass it made.
func (d *dynamicTeam) drive(t *testing.T, bound int, done func(WorkGraphView) bool) []PlanTickReport {
	t.Helper()
	var reports []PlanTickReport
	for range bound {
		reports = append(reports, d.plan.reconcile(t))
		if _, err := d.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		d.fleet.clock.advance(61 * time.Second)
		if done(d.fleet.graph(d.graphID)) {
			return reports
		}
	}
	t.Fatalf("the team did not reach its goal within %d passes: %+v", bound, d.fleet.graph(d.graphID))
	return nil
}

// publishFinal drives a final unit's run to its published PR, letting the plan
// reconciler settle the downstream gates in between. It returns the PR's head.
func (d *dynamicTeam) publishFinal(t *testing.T, unitID string) (*runState, string) {
	t.Helper()
	unit := d.fleet.graph(d.graphID).unit(t, unitID)
	for range 20 {
		d.plan.reconcile(t)
		if _, err := d.fleet.runtime.Reconcile(context.Background(), unit.RunID); err != nil {
			t.Fatal(err)
		}
	}
	state := d.fleet.state(unit.RunID)
	if state.projection.PullRequest == nil {
		t.Fatalf("unit %s's accepted candidate never reached a published PR (publication deferred=%v)", unitID, state.publicationDeferred)
	}
	return state, state.projection.CandidateRevision
}

func stageIDsOf(stages []domain.PlanStage) []string {
	ids := make([]string, 0, len(stages))
	for _, stage := range stages {
		ids = append(ids, stage.ID)
	}
	return ids
}

func hasStage(stages []domain.PlanStage, id string) bool {
	for _, stage := range stages {
		if stage.ID == id {
			return true
		}
	}
	return false
}

// TestASmallProposedTeamIsNotInflatedIntoACeremony: the planner asks for one
// producer and the system runs exactly one worker. Policy still adds the
// deterministic assurance gate the contract requires - a gate creates no run.
func TestASmallProposedTeamIsNotInflatedIntoACeremony(t *testing.T) {
	team := newDynamicTeam(t, []domain.PlanStage{producerStage("backend", "a")}, 0)
	if hasStage(team.proposed.Stages, "assurance") || !hasStage(team.plan.plan.Stages, "assurance") {
		t.Fatalf("policy must add the contract's assurance gate to a team that did not ask for one: proposed=%v compiled=%v",
			stageIDsOf(team.proposed.Stages), stageIDsOf(team.plan.plan.Stages))
	}
	graph, found, err := team.fleet.store.WorkGraph(team.graphID)
	if err != nil || !found || len(graph.Units) != 1 || graph.Units[0].ID != "backend" {
		t.Fatalf("the graph is not the planner's one-producer team: %+v %v", graph.Units, err)
	}

	reports := team.drive(t, 40, func(view WorkGraphView) bool { return view.unit(t, "backend").Output != nil })
	started := 0
	for _, report := range reports {
		started += len(report.Started)
	}
	if started != 1 || team.worker.invocations() != 1 {
		t.Fatalf("a one-producer team ran %d stage(s) and %d worker invocation(s)", started, team.worker.invocations())
	}
	state, head := team.publishFinal(t, "backend")
	if state.projection.PullRequest == nil || head == "" {
		t.Fatal("the team's only candidate was not published")
	}
	if team.worker.invocations() != 1 {
		t.Fatalf("publishing re-invoked a worker: %d invocations", team.worker.invocations())
	}
}

// TestABroadProposedTeamRunsInParallelIntegratesAndPublishesWithProvenance:
// two independent producers start in the same pass, the integrator waits for
// BOTH admitted handoffs, composes them, and its candidate - bound to the
// objective's approved plan digest through the graph, batch, run and handoff -
// is what reaches a published PR.
func TestABroadProposedTeamRunsInParallelIntegratesAndPublishesWithProvenance(t *testing.T) {
	team := newDynamicTeam(t, []domain.PlanStage{
		producerStage("backend", "a"), producerStage("frontend", "b"), integratorStage("backend", "frontend"),
	}, 2)
	reports := team.drive(t, 60, func(view WorkGraphView) bool { return view.unit(t, "integrate").Output != nil })

	// Parallelism: both producers became runs in one reconciler pass, and the
	// integrator in a strictly later one.
	startedIn := map[string]int{}
	for i, report := range reports {
		for _, stage := range report.Started {
			startedIn[stage.StageID] = i
		}
	}
	if startedIn["backend"] != startedIn["frontend"] {
		t.Fatalf("independent producers did not start together: %v", startedIn)
	}
	if later, ok := startedIn["integrate"]; !ok || later <= startedIn["backend"] {
		t.Fatalf("the integrator started before its producers could have been admitted: %v", startedIn)
	}

	view := team.fleet.graph(team.graphID)
	backend, frontend, integrate := view.unit(t, "backend"), view.unit(t, "frontend"), view.unit(t, "integrate")
	if backend.Output == nil || frontend.Output == nil || integrate.Output == nil {
		t.Fatalf("a unit finished without an admitted handoff: %+v", view)
	}

	// The integrator consumed exactly the two handoffs the producers admitted.
	if len(integrate.Inputs) != 2 {
		t.Fatalf("the integrator consumed %d inputs, want 2", len(integrate.Inputs))
	}
	consumed := map[string]orchestration.UnitOutput{}
	for _, input := range integrate.Inputs {
		consumed[input.UnitID] = input.UnitOutput
	}
	for id, produced := range map[string]*orchestration.UnitOutput{"backend": backend.Output, "frontend": frontend.Output} {
		if consumed[id].HandoffID != produced.HandoffID || consumed[id].CandidateRevision != produced.CandidateRevision {
			t.Fatalf("integrator input for %s is not the admitted handoff: consumed=%+v produced=%+v", id, consumed[id], *produced)
		}
	}

	// Real composition: the integrator's workspace holds both producers' work.
	dir := candidateDir(team.fleet.stateDir, integrate.RunID)
	for _, name := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the composed candidate is missing %s: %v", name, err)
		}
	}

	// Provenance: objective's plan -> graph -> batch -> run -> handoff.
	graph, _, _ := team.fleet.store.WorkGraph(team.graphID)
	if graph.ObjectivePlan == nil || graph.ObjectivePlan.ID != team.plan.plan.ID || graph.ObjectivePlan.Digest != team.plan.plan.Digest {
		t.Fatalf("the graph does not bind the approved plan revision: %+v", graph.ObjectivePlan)
	}
	for _, unit := range []WorkGraphUnitView{backend, frontend, integrate} {
		batch, found, err := team.fleet.store.OrchestrationBatch(unit.BatchID)
		if err != nil || !found || batch.Origin == nil || batch.Origin.GraphID != team.graphID || batch.Origin.UnitID != unit.UnitID {
			t.Fatalf("unit %s's batch does not name its graph and unit: %+v %v", unit.UnitID, batch.Origin, err)
		}
		run, ok := storedRun(t, team.fleet.phase8Fixture, unit.RunID)
		if !ok || run.Plan == nil || run.Plan.PlanDigest != team.plan.plan.Digest || run.Plan.StageID != unit.UnitID {
			t.Fatalf("unit %s's run is not bound to the approved plan stage: %+v", unit.UnitID, run.Plan)
		}
		if unit.Output.RunID != unit.RunID {
			t.Fatalf("unit %s's handoff names run %s, not its own %s", unit.UnitID, unit.Output.RunID, unit.RunID)
		}
	}
	if invoked := team.worker.invocations(); invoked != 2 {
		t.Fatalf("two producers and a deterministic integrator ran %d worker invocations, want 2", invoked)
	}

	// The compiler-added gate is satisfied from real evidence and then the
	// integrator's candidate - the objective's final one - reaches a PR.
	state, head := team.publishFinal(t, "integrate")
	if head != integrate.Output.CandidateRevision {
		t.Fatalf("the published head %s is not the integrator's admitted candidate %s", head, integrate.Output.CandidateRevision)
	}
	if state.projection.PullRequest == nil {
		t.Fatal("no published PR")
	}
}

// TestProposalsOutsideTheGovernedBoundsSpawnNothing: the planner proposes, the
// compiler decides. Each unacceptable team is refused with a durable attempt
// record, and no graph, batch or run exists afterwards.
func TestProposalsOutsideTheGovernedBoundsSpawnNothing(t *testing.T) {
	cases := map[string][]domain.PlanStage{
		"more producers than the plan's child-run envelope": {
			producerStage("p1", "a"), producerStage("p2", "b"), producerStage("p3", "c"),
			producerStage("p4", "a"), producerStage("p5", "b"),
		},
		"a dependency on a stage the planner never proposed": {
			producerStage("backend", "a"),
			{ID: "frontend", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "Implement. " + integrationUnitMarker("b"),
				DependsOn:            []string{"ghost"},
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		},
		"a dependency cycle": {
			{ID: "x", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "x", DependsOn: []string{"y"},
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
			{ID: "y", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "y", DependsOn: []string{"x"},
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		},
		"no material producer at all": {
			{ID: "look", Kind: domain.StageAgent, Role: domain.RoleReviewer, Objective: "Look.",
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis}},
		},
	}
	for name, stages := range cases {
		t.Run(name, func(t *testing.T) {
			fleet := newFleetFixture(t, 4)
			fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
			worker := &dynamicTeamWorker{}
			fleet.deps.Provider = worker
			fleet.runtime = fleet.newRuntime(fleet.deps)

			_, _, err := proposeTeam(t, fleet.phase8Fixture, fleet.deps.ConfigDigest, plannerAnswer(t, stages))
			var refused *PlanAttemptRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("the compiler admitted an unacceptable team (err=%v)", err)
			}
			if _, found, err := fleet.store.Plan("plan-fixture"); err != nil || found {
				t.Fatalf("a refused proposal became a plan: found=%v err=%v", found, err)
			}
			if graph, err := objectiveGraphFor(fleet.store, "plan-fixture"); err != nil || graph != nil {
				t.Fatalf("a refused proposal produced a work graph: %+v %v", graph, err)
			}
			if batches, err := fleet.store.OrchestrationBatches(); err != nil || len(batches) != 0 {
				t.Fatalf("a refused proposal spawned batches: %v %v", batches, err)
			}
			if runs, err := fleet.store.ActiveRuns(); err != nil || len(runs) != 0 || worker.invocations() != 0 {
				t.Fatalf("a refused proposal started work: runs=%v invocations=%d err=%v", runs, worker.invocations(), err)
			}
		})
	}
}
