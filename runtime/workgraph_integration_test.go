package runtime

// Production acceptance for #475: connecting the merged deterministic Git
// composition core (integration.Contract, runtime.IntegrateInputs) to the
// real WorkGraph (#472), real EngineeringRun (#63) lifecycle, real SQLite
// store and real scheduler - never a synthetic graph fixture and never a
// standalone IntegrateInputs call. It reuses the #470/#472 acceptance fleet
// (orchestration_fleet_test.go, workgraph_test.go) with one addition: a
// controlled worker that writes real, distinct file content per unit, so two
// parallel producers have something genuine to compose or conflict over.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/integration"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// integrationUnitMarker is the exact text #475's own test issues carry in
// their body, so the controlled worker below picks real file content by
// unit rather than by guessing at invocation order.
func integrationUnitMarker(unitID string) string { return "INTEGRATION-UNIT:" + unitID }

// integrationWorker is a controlled ExecutionProvider for producer units
// only: it writes one real file per unit, named and shaped by what the test
// configured for that unit's marker, and a perfect handoff. It is never
// invoked for the integrator unit itself - bindExecutionInvoke's guard
// refuses that before any provider is reached, which is part of what these
// tests prove.
type integrationWorker struct {
	// files maps a unit marker to the path and content it writes.
	files map[string]integrationFile
}

type integrationFile struct {
	path, content string
}

// WritesTypedResults: the controlled worker writes straight to the slot path.
func (w *integrationWorker) WritesTypedResults() bool { return true }

func (w *integrationWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (w *integrationWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	for marker, file := range w.files {
		if !strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			continue
		}
		if err := os.WriteFile(filepath.Join(request.CandidateDir, file.path), []byte(file.content), 0o600); err != nil {
			return ExecutionResult{}, err
		}
		break
	}
	if request.HandoffPath == "" {
		return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded}, nil
	}
	return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded},
		os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
}

// integrationGraphUnits is one graph: two parallel producers and an
// integrator depending on both. issues are distinct, fresh offsets from
// fleetFirstIssue - this test's own fixture, never shared with another
// test's store.
func integrationGraphUnits(issueA, issueB, issueC int) []orchestration.WorkUnit {
	return []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		{ID: "b", Purpose: "land b", Role: domain.RoleImplementer, Issue: issueB},
		{ID: "c", Purpose: "integrate a and b", Role: domain.RoleIntegrator,
			ExecutionKind: orchestration.ExecutionKindIntegrationCompose, Issue: issueC, DependsOn: []string{"a", "b"}},
	}
}

// setIntegrationIssue overrides one stock fleet issue's body with the exact
// marker integrationWorker matches on, so the controlled worker's content
// decision is a fact about the test's own setup, never about invocation
// order or timing.
func (f *fleetFixture) setIntegrationIssue(issue int, unitID string) {
	stored := f.forge.Issues[issue]
	stored.Body = UntrustedText(integrationUnitMarker(unitID))
	f.forge.Issues[issue] = stored
}

// TestWorkGraphIntegrationComposesTwoParallelOutputs is Scenario A: two
// parallel producer units create distinct admitted changes, the integration
// unit consumes both, creates a third exact candidate revision through the
// real supervisor/scheduler/store, and only then releases nothing further
// (c has no dependents here; TestWorkGraphIntegrationReleasesDownstreamOnlyAfterItsOwnHandoff
// proves the release itself).
func TestWorkGraphIntegrationComposesTwoParallelOutputs(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	if got := view.states(); got["a"] != orchestration.UnitReady || got["b"] != orchestration.UnitReady || got["c"] != orchestration.UnitBlocked {
		t.Fatalf("states after adoption = %v", got)
	}

	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	a, b, c := settled.unit(t, "a"), settled.unit(t, "b"), settled.unit(t, "c")
	if c.Output == nil || c.Output.CandidateRevision == "" {
		t.Fatal("integrator unit completed with no admitted output subject")
	}
	// The composed candidate is a NEW exact subject, never one of the inputs'
	// own commits.
	if c.Output.CandidateRevision == a.Output.CandidateRevision || c.Output.CandidateRevision == b.Output.CandidateRevision {
		t.Fatalf("integrator's output %s is one of its inputs' own commits", c.Output.CandidateRevision)
	}
	// It carries BOTH producers' content: a real merge, not a pass-through of
	// one side.
	run, ok := storedRun(t, fixture.phase8Fixture, c.RunID)
	if !ok {
		t.Fatal("integrator run not found")
	}
	dir := candidateDir(fixture.stateDir, run.ID)
	for _, path := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("composed candidate is missing %s from its inputs: %v", path, err)
		}
	}
	// The integrator's own run is ordinary in every way that matters: no
	// plan, an orchestration binding, bound to the named agent - the same
	// assertion TestTheDiamondAdvancesOnAdmittedHandoffsAlone makes of an
	// ordinary unit's run.
	if run.Plan != nil || run.Orchestration == nil || run.AgentID != "claude" {
		t.Fatalf("integrator unit is not an ordinary orchestrated run: %+v", run)
	}
}

// TestWorkGraphIntegrationReleasesDownstreamOnlyAfterItsOwnHandoff extends
// Scenario A with a dependent of the integration unit: it must stay
// unactivated until the integration unit's OWN handoff is admitted, exactly
// as #472 already proves for an ordinary unit (TestTheDiamondAdvancesOnAdmittedHandoffsAlone).
func TestWorkGraphIntegrationReleasesDownstreamOnlyAfterItsOwnHandoff(t *testing.T) {
	const issueA, issueB, issueC, issueD = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2, fleetFirstIssue + 3
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.setIntegrationIssue(issueD, "d")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
		"d": {path: "d.go", content: "package candidate\nconst D = 1\n"},
	}}
	supervisor := fixture.supervisor()
	units := integrationGraphUnits(issueA, issueB, issueC)
	units = append(units, orchestration.WorkUnit{ID: "d", Purpose: "downstream of the integration", Role: domain.RoleImplementer, Issue: issueD, DependsOn: []string{"c"}})
	view := fixture.adoptGraph(supervisor, "claude", 1, units)

	settled := fixture.driveGraph(supervisor, view.GraphID, func(current WorkGraphView) {
		c := current.unit(t, "c")
		if c.Child != nil && c.Child.Handoff == orchestration.HandoffReported && c.Output == nil {
			if d := current.unit(t, "d"); d.RunID != "" || d.State != orchestration.UnitBlocked {
				t.Fatalf("d is %s with run %q while c's handoff is only REPORTED, not admitted", d.State, d.RunID)
			}
		}
	})
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
}

// TestWorkGraphIntegrationNeverInvokesAProviderForTheIntegratorUnit proves
// the explicit, structural dispatch guard by breaking it conceptually: the
// controlled worker given to the fixture would fail the test outright if it
// were ever invoked for issueC, because it writes no file this assertion
// expects and the integrator's own run would then carry no merge of a and
// b's content. This is #475's version of "prove guards by breaking them" -
// the guard is bindExecutionInvoke's role check (runtime/reconciler.go), and
// TestWorkGraphIntegrationComposesTwoParallelOutputs already demonstrates the
// positive: a provider WAS reached for a and b (they carry real content) and
// the merge still produced both files, which is only possible if c's own
// provider never ran and so never raced the deterministic composition.
func TestWorkGraphIntegrationNeverInvokesAProviderForTheIntegratorUnit(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	invoked := map[string]bool{}
	fixture.deps.Provider = providerFunc(func(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
		invoked[request.RunID] = true
		if strings.Contains(request.Objective, integrationUnitMarker("a")) {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, "a.go"), []byte("package candidate\nconst A = 1\n"), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		if strings.Contains(request.Objective, integrationUnitMarker("b")) {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, "b.go"), []byte("package candidate\nconst B = 1\n"), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		if request.HandoffPath == "" {
			return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded}, nil
		}
		return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded},
			os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
	})
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	c := settled.unit(t, "c")
	if invoked[c.RunID] {
		t.Fatalf("a provider was invoked for the integrator unit's own run %s; it must receive deterministic composition only", c.RunID)
	}
}

// TestWorkGraphOrdinaryRoleIntegratorUnitStillUsesAProvider is R3-1's
// required counterexample: domain.RoleIntegrator alone - with no explicit
// ExecutionKind - names a worker RESPONSIBILITY, never an execution
// algorithm. A unit with that role and no ExecutionKind set must still be an
// ordinary execution.invoke, exactly like any other role, proving Role and
// ExecutionKind are genuinely independent axes rather than one overloaded
// field.
func TestWorkGraphOrdinaryRoleIntegratorUnitStillUsesAProvider(t *testing.T) {
	const issueA, issueB = fleetFirstIssue, fleetFirstIssue + 1
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	invoked := map[string]bool{}
	fixture.deps.Provider = providerFunc(func(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
		invoked[request.RunID] = true
		if strings.Contains(request.Objective, integrationUnitMarker("a")) {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, "a.go"), []byte("package candidate\nconst A = 1\n"), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		if strings.Contains(request.Objective, integrationUnitMarker("b")) {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, "b.go"), []byte("package candidate\nconst B = 1\n"), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		if request.HandoffPath == "" {
			return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded}, nil
		}
		return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded},
			os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
	})
	supervisor := fixture.supervisor()
	units := []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		// Role is domain.RoleIntegrator, same responsibility as c in the other
		// scenarios, but ExecutionKind is absent: this must behave exactly like
		// an ordinary RoleImplementer unit, never deterministic composition.
		{ID: "b", Purpose: "integrate a, the ordinary way", Role: domain.RoleIntegrator, Issue: issueB, DependsOn: []string{"a"}},
	}
	view := fixture.adoptGraph(supervisor, "claude", 1, units)
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	b := settled.unit(t, "b")
	if !invoked[b.RunID] {
		t.Fatalf("an ordinary RoleIntegrator unit with no ExecutionKind never reached a provider; run %s", b.RunID)
	}
	run, ok := storedRun(t, fixture.phase8Fixture, b.RunID)
	if !ok {
		t.Fatal("ordinary integrator-role run not found")
	}
	dir := candidateDir(fixture.stateDir, run.ID)
	if _, err := os.Stat(filepath.Join(dir, "b.go")); err != nil {
		t.Fatalf("provider-written content missing from an ordinary RoleIntegrator unit's candidate: %v", err)
	}
}

// providerFunc adapts a function to ExecutionProvider for a test-local
// one-off, the same pattern fleetProvider itself is a heavier version of.
type providerFunc func(context.Context, ExecutionRequest) (ExecutionResult, error)

func (f providerFunc) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	return f(ctx, request)
}

// WritesTypedResults: the controlled worker writes straight to the slot path.
func (f providerFunc) WritesTypedResults() bool { return true }

func (f providerFunc) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

// TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff is Scenario
// B: a and b modify the same content incompatibly. The integration unit
// never admits a partial or completed handoff, and the conflict is visible
// as a bounded, typed #473 Finding bound to the exact conflicting input.
func TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "shared.go", content: "package candidate\nconst Shared = \"a\"\n"},
		"b": {path: "shared.go", content: "package candidate\nconst Shared = \"b\"\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))

	// This deterministic conflict never clears on its own (#475 review's
	// retry-policy follow-up): the run PARKS (Waiting,
	// deterministic_failure_unchanged) after its first identical-attempt
	// check, rather than spending its whole attempt budget re-merging the
	// same unchanged pair. unitSettled (workgraph_test.go) has no case for
	// that disposition - correctly: a capacity or external wait is expected
	// to clear as the loop keeps ticking, and treating every Waiting state
	// as "done" would stop driveGraph before those clear - so this test
	// drives manually and checks for the exact durable disposition instead
	// of relying on driveGraph's generic settlement detection.
	var cRunID string
	var parked bool
	for range 60 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		current := fixture.graph(view.GraphID)
		if c := current.unit(t, "c"); c.Output != nil {
			t.Fatal("a conflicting integration admitted a handoff")
		} else if c.RunID != "" {
			cRunID = c.RunID
		}
		if cRunID == "" {
			continue
		}
		events, err := fixture.store.Events(cRunID)
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
		t.Fatal("the blocked integration unit never parked on its unchanged deterministic conflict")
	}
	a, b := fixture.graph(view.GraphID).unit(t, "a"), fixture.graph(view.GraphID).unit(t, "b")
	if a.State != orchestration.UnitState(orchestration.ItemCompleted) || b.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("producers did not both complete: a=%s b=%s", a.State, b.State)
	}
	c := fixture.graph(view.GraphID).unit(t, "c")
	if c.Output != nil {
		t.Fatal("blocked integration unit carries an admitted output")
	}
	// Parked, not retried to exhaustion: at most ONE real integration.compose
	// attempt ran before the identical-failure check parked the run.
	ops, err := fixture.store.Operations(cRunID)
	if err != nil {
		t.Fatal(err)
	}
	var composeAttempts int
	for _, op := range ops {
		if op.Kind == OpIntegrationCompose {
			composeAttempts++
		}
	}
	if composeAttempts != 1 {
		t.Fatalf("integration.compose ran %d times before parking, want exactly 1", composeAttempts)
	}
	// Message admission reads the state a pass STARTED with (#470's existing
	// tick-granular admission), so it lags the pass that journalled the run's
	// own last observation by one tick - exactly as handoff admission does.
	// One more tick lets it catch up, the same way driveGraph's own loop would
	// if this unit had a dependent still waiting on it.
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, ok := storedRun(t, fixture.phase8Fixture, c.RunID)
	if !ok || run.Orchestration == nil {
		t.Fatal("integrator run not found or not orchestrated")
	}
	messages, err := fixture.store.ScopeMessages(run.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var finding *orchestration.EngineeringMessage
	for i := range messages {
		if messages[i].Kind == orchestration.KindFinding {
			finding = &messages[i]
		}
	}
	if finding == nil {
		refusals, _ := fixture.store.ScopeMessageRefusals(run.Orchestration.BatchID)
		t.Fatalf("blocked integration emitted no finding; messages=%d refusals=%v", len(messages), refusals)
	}
	if finding.Category != orchestration.FindingDefect || finding.Subject == nil {
		t.Fatalf("finding is not a bounded, sourced defect: %+v", finding)
	}
	if finding.Route.Unit != "a" && finding.Route.Unit != "b" {
		t.Fatalf("finding routes to %q, not one of the conflicting units", finding.Route.Unit)
	}
	// R3-2: this Finding was produced by deterministic composition, never by
	// an invocation of the run's agent. It must say so honestly, never
	// borrow the run's AgentID to look like one.
	if !finding.Source.System {
		t.Fatal("a Finding from deterministic composition is not marked system-authored")
	}
	if finding.Source.AgentID != "" {
		t.Fatalf("a system-authored Finding names agent id %q; it never invoked an agent", finding.Source.AgentID)
	}
	// #475 review B1: honestly, this is admission into the integrator's OWN
	// batch, not delivery into producer a's inbox - #473's per-batch
	// isolation means producer a's own batch scope must NOT see it. This is
	// the regression guard against ever closing that gap by accident with
	// graph-wide visibility (reverted #543) instead of a deliberate,
	// narrow, exact-subject delivery mechanism.
	producerRun, ok := storedRun(t, fixture.phase8Fixture, a.RunID)
	if !ok || producerRun.Orchestration == nil {
		t.Fatal("producer a's run not found or not orchestrated")
	}
	producerMessages, err := fixture.store.ScopeMessages(producerRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range producerMessages {
		if message.ID == finding.ID {
			t.Fatal("the integrator's Finding leaked into producer a's own batch scope - #473 per-batch isolation is broken")
		}
	}
}

// TestWorkGraphIntegrationRestartReproducesTheSameOutcome is Scenario E/H: a
// GENUINELY closed and reopened SQLite store (reopen, authority_matrix_test.go
// - nothing survives out of a connection pool or page cache; everything the
// restarted supervisor reports has to come off disk) replays to the
// identical frontier, admits no second handoff and creates no second
// integration run, whether the first pass already finished or is driven
// again mid-flight.
func TestWorkGraphIntegrationRestartReproducesTheSameOutcome(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	c := settled.unit(t, "c")
	firstRunID, firstCandidate := c.RunID, c.Output.CandidateRevision

	// Genuinely close and reopen the store, then a fresh supervisor over the
	// reopened handle: the restart.
	reopen(t, fixture.phase8Fixture)
	restarted := fixture.supervisor()
	for range 10 {
		if _, err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	again := fixture.graph(view.GraphID)
	replayed := again.unit(t, "c")
	if replayed.RunID != firstRunID {
		t.Fatalf("restart created a second child run for the integration unit: %s then %s", firstRunID, replayed.RunID)
	}
	if replayed.Output == nil || replayed.Output.CandidateRevision != firstCandidate {
		t.Fatalf("restart did not reproduce the same admitted candidate: %+v", replayed.Output)
	}
	handoffs, err := fixture.store.RunHandoffs(firstRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("restart admitted %d handoffs for the integration run, not exactly 1", len(handoffs))
	}
}

// TestWorkGraphIntegrationUnitInvalidatedWhenUpstreamOutputReplaced is
// Scenario D: an upstream admitted output superseded after activation
// invalidates the integration unit through #472's own exact-subject
// invalidation, exactly as it already does for an ordinary unit
// (TestAReplacedUpstreamSubjectInvalidatesDownstreamSatisfaction, whose
// replaceAdmittedOutput helper this test reuses unchanged). An integrator is
// never exempt from that rule: #475 adds no second notion of "stale".
func TestWorkGraphIntegrationUnitInvalidatedWhenUpstreamOutputReplaced(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 3 {
		t.Fatalf("the graph did not complete first: %+v", settled.Counts)
	}
	a := settled.unit(t, "a")
	replaceAdmittedOutput(t, fixture, view.GraphID, a)

	after := fixture.graph(view.GraphID)
	if state := after.unit(t, "c").State; state != orchestration.UnitInvalidated {
		t.Fatalf("integration unit c is %s after its input a was replaced, want invalidated: %s", state, after.unit(t, "c").Reason)
	}
	if len(after.Frontier) != 0 {
		t.Fatalf("an invalidated integration unit is in the frontier: %v", after.Frontier)
	}
	// Invalidated work is not silently re-performed: no second run appears
	// for the integration unit.
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterTick, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTick) != len(runs) {
		t.Fatalf("a pass over an invalidated integration unit created %d runs", len(afterTick)-len(runs))
	}
}

// targetedAssurance fails exactly the composed integration candidate's own
// assurance, identified by CONTENT rather than by run id: composeIntegration
// commits its merge and reaches its own assurance operation within the SAME
// scheduler tick that first creates the run (there is no intervening
// provider invocation to span a tick boundary, unlike an ordinary unit), so
// a run id learned by observing the WorkGraph between ticks is always
// already stale by the time assurance for that run actually executes.
// Checking the checkout for both producers' own files - true only of the
// merged commit - needs no such timing at all.
type targetedAssurance struct {
	failPaths []string
	// failClass is the verdict a matched checkout fails with. Defaults to
	// FailureCompileTest (a real candidate verdict) when left zero.
	failClass FailureClass
}

func (a *targetedAssurance) Assure(_ context.Context, request AssuranceRequest) (AssuranceResult, error) {
	fail := len(a.failPaths) > 0
	for _, path := range a.failPaths {
		if _, err := os.Stat(filepath.Join(request.CheckoutDir, path)); err != nil {
			fail = false
			break
		}
	}
	if fail {
		class := a.failClass
		if class == "" {
			class = FailureCompileTest
		}
		return AssuranceResult{ProviderID: "targeted-assurance", VerifierDefinition: "fake", Passed: false, FailureClass: class}, nil
	}
	return AssuranceResult{ProviderID: "targeted-assurance", VerifierDefinition: "fake", Passed: true}, nil
}

func (a *targetedAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return []domain.EvidenceClass{AssuranceEvidenceClass}
}

// TestWorkGraphIntegrationSemanticAssuranceFailureNeverRoutesToAProvider is
// Scenario C: Git merges cleanly, but the integrated candidate's own fresh,
// independent assurance fails. The system classifies that as the
// integration's own typed failure (never success, never a fabricated
// textual conflict) and never tries to route it to a producer - the
// guard TestWorkGraphIntegrationNeverInvokesAProviderForTheIntegratorUnit
// proves exists - which would otherwise strand the run with nothing
// eligible to plan (RouteProviderRemediation with no provider to remediate
// it).
func TestWorkGraphIntegrationSemanticAssuranceFailureNeverRoutesToAProvider(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	invoked := map[string]bool{}
	fixture.deps.Provider = providerFunc(func(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
		invoked[request.RunID] = true
		for _, marker := range []string{"a", "b"} {
			if strings.Contains(request.Objective, integrationUnitMarker(marker)) {
				path := marker + ".go"
				if err := os.WriteFile(filepath.Join(request.CandidateDir, path), []byte("package candidate\nconst V = 1\n"), 0o600); err != nil {
					return ExecutionResult{}, err
				}
			}
		}
		if request.HandoffPath == "" {
			return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded}, nil
		}
		return ExecutionResult{ProviderID: "integration-fleet-worker", Outcome: execution.Succeeded},
			os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
	})
	assurance := &targetedAssurance{failPaths: []string{"a.go", "b.go"}}
	fixture.deps.Assurance = assurance
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))

	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	a, b, c := settled.unit(t, "a"), settled.unit(t, "b"), settled.unit(t, "c")
	if a.State != orchestration.UnitState(orchestration.ItemCompleted) || b.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("producers did not both complete: a=%s b=%s", a.State, b.State)
	}
	if c.State != orchestration.UnitState(orchestration.ItemFailed) {
		t.Fatalf("integration unit with a failing assurance settled as %s, not failed: %s", c.State, c.Reason)
	}
	if c.Output != nil {
		t.Fatal("an integration whose own assurance failed carries an admitted output")
	}
	// #475 review B2: WorkGraph's own Output being nil is a PROJECTION fact,
	// not proof the durable handoffs table is empty. Assert the actual row
	// count directly - a failing assurance must never let the durable
	// handoff a successful Git merge could have reported slip through.
	if handoffs, err := fixture.store.RunHandoffs(c.RunID); err != nil {
		t.Fatal(err)
	} else if len(handoffs) != 0 {
		t.Fatalf("an integration whose own assurance failed admitted %d durable handoffs, want 0", len(handoffs))
	}
	if invoked[c.RunID] {
		t.Fatalf("a provider was invoked for the integrator unit's run %s after its assurance failed", c.RunID)
	}
	events, err := fixture.store.Events(c.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var sawIntegrationConflict bool
	for _, event := range events {
		if event.Type != EventAssuranceObserved {
			continue
		}
		payload, err := decodePayload[AssuranceObservedPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if !payload.Passed && payload.FailureClass == FailureIntegrationConflict {
			sawIntegrationConflict = true
			// #475 review B5: the runtime's own retry-routing relabel must
			// never erase the fresh verifier's actual verdict.
			if payload.IntegrationOriginalFailureClass != FailureCompileTest {
				t.Fatalf("original verifier class not preserved: got %q, want %q", payload.IntegrationOriginalFailureClass, FailureCompileTest)
			}
		}
	}
	if !sawIntegrationConflict {
		t.Fatal("no assurance observation recorded the integration's own typed failure class")
	}
}

// TestWorkGraphIntegrationTransientAssuranceFailureIsNeverReclassified proves
// the RouteProviderRemediation-only guard in assureCandidate by breaking it
// conceptually: a TRANSIENT infrastructure failure (the sandbox could not
// run, never a verdict about the candidate) must stay exactly that, never
// get relabeled integration_conflict. Reclassifying it would be wrong twice
// over - it misreports why the attempt failed, and integration_conflict's
// own retry disposition is written for an unchanging deterministic
// conflict, not a fault that may clear on its own next attempt.
func TestWorkGraphIntegrationTransientAssuranceFailureIsNeverReclassified(t *testing.T) {
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	fixture.deps.Assurance = &targetedAssurance{failPaths: []string{"a.go", "b.go"}, failClass: FailureTransientInfrastructure}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	c := settled.unit(t, "c")
	if c.Output != nil {
		t.Fatal("an integration whose own assurance never passed carries an admitted output")
	}
	events, err := fixture.store.Events(c.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var sawTransient, sawMisclassified bool
	for _, event := range events {
		if event.Type != EventAssuranceObserved {
			continue
		}
		payload, err := decodePayload[AssuranceObservedPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		switch payload.FailureClass {
		case FailureTransientInfrastructure:
			sawTransient = true
		case FailureIntegrationConflict:
			sawMisclassified = true
		}
	}
	if !sawTransient {
		t.Fatal("no assurance observation recorded the real transient infrastructure class")
	}
	if sawMisclassified {
		t.Fatal("a transient infrastructure assurance failure was relabeled integration_conflict")
	}
}

// integratedHandoffInterleaveStore wraps the real store to inject a second,
// genuinely separate writer connection's competing admission between
// AdmitIntegratedHandoff's freshness check and its own insert (#475 review
// B3's "two database handles" ask) - the same technique
// interleavedCheckpointHandoffStore (orchestration_checkpoint_handoff_test.go)
// already proves for the pre-existing checkpoint-handoff race.
type integratedHandoffInterleaveStore struct {
	*SQLiteOperationStore
	beforeInsert func()
}

func (s integratedHandoffInterleaveStore) AdmitIntegratedHandoff(handoff orchestration.EngineeringHandoff, inputs orchestration.WorkUnitInputs) (bool, error) {
	s.beforeInsert()
	return s.SQLiteOperationStore.AdmitIntegratedHandoff(handoff, inputs)
}

func inputFor(unitID string, handoff orchestration.EngineeringHandoff) orchestration.WorkUnitInput {
	return orchestration.WorkUnitInput{UnitID: unitID, UnitOutput: orchestration.UnitOutput{
		HandoffID: handoff.ID, RunID: handoff.RunID,
		CandidateRevision: handoff.Subject.CandidateRevision, CandidateTree: handoff.Subject.CandidateTree,
		Outcome: orchestration.OutcomeCompleted,
	}}
}

// TestAdmitIntegratedHandoffLosesToSupersededInputBeforeInsert is Scenario
// B3: an upstream input superseded in the GAP between the live freshness
// check and the handoff INSERT must still lose, because the check and the
// write are the SAME SQLite statement - proven with a genuinely separate
// writer connection performing the superseding admission between them, not
// merely a stale read followed by a later write on one connection. (The
// non-superseded, successful case is already proven end to end by every
// other #475 scenario here - this test exercises only the race.)
//
// Two ordinary #470 batch items (not WorkGraph units) stand in for the
// producer and the integrator: AdmitIntegratedHandoff's own contract needs
// only valid run_id/batch_id foreign keys, which an ordinary completed item
// already provides, without any WorkGraph/ExecutionKind machinery this test
// does not need.
func TestAdmitIntegratedHandoffLosesToSupersededInputBeforeInsert(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(2))
	settled := fixture.drive(supervisor, view.BatchID)
	producerHandoffs, err := fixture.store.RunHandoffs(settled.Items[0].RunID)
	if err != nil || len(producerHandoffs) != 1 {
		t.Fatalf("producer item did not admit exactly one handoff: %d (%v)", len(producerHandoffs), err)
	}
	h1 := producerHandoffs[0]
	integratorHandoffs, err := fixture.store.RunHandoffs(settled.Items[1].RunID)
	if err != nil || len(integratorHandoffs) != 1 {
		t.Fatalf("integrator-stand-in item did not admit exactly one handoff: %d (%v)", len(integratorHandoffs), err)
	}
	// A SYNTHETIC second attempt against the same already-admitted run/batch
	// (valid FK anchors), never actually produced by the ordinary flow -
	// AdmitIntegratedHandoff itself decides whether it is admissible.
	synthetic := integratorHandoffs[0]
	synthetic.ID = "handoff-integrator-synthetic"

	inputs := orchestration.WorkUnitInputs{inputFor("a", h1)}
	writer, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	interleaved := false
	store := integratedHandoffInterleaveStore{SQLiteOperationStore: fixture.store, beforeInsert: func() {
		interleaved = true
		// A genuinely separate connection admits producer a's SECOND,
		// superseding handoff for the SAME run - the race window this
		// proves closed.
		superseding := h1
		superseding.ID = "handoff-a-superseding"
		superseding.AdmittedAt = h1.AdmittedAt.Add(time.Second)
		if ok, err := writer.AdmitHandoff(superseding); err != nil || !ok {
			t.Fatalf("competing admission did not land: ok=%t err=%v", ok, err)
		}
	}}
	inserted, err := store.AdmitIntegratedHandoff(synthetic, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("admission never reached the insert boundary")
	}
	if inserted {
		t.Fatal("the integrated handoff was admitted despite its input being superseded between the check and the insert")
	}
	// Exactly the one REAL handoff the ordinary flow admitted - the
	// synthetic, superseded attempt never landed.
	if handoffs, err := fixture.store.RunHandoffs(synthetic.RunID); err != nil || len(handoffs) != 1 {
		t.Fatalf("run %s has %d handoffs after a refused synthetic admission, want exactly 1 (%v)", synthetic.RunID, len(handoffs), err)
	}
}

// TestComposeIntegrationPostMergeFailureIsContainedAndRecovers is Scenario
// B4: once IntegrateInputs returns StatusIntegrated, Git HEAD has already
// advanced. A failure in one of commitIntegratedCandidate's own LATER steps
// (here, integratedChangedPaths, forced by a corrupted candidate revision
// that does not exist) must never be a bare, unowned error: it is routed
// through the existing restoreCandidate/FailureWorkspaceIntegrity
// containment, which both fails the operation and restores the workspace -
// never a false success, never a silently stranded advanced HEAD. A
// subsequent attempt against the now-restored workspace then composes and
// commits cleanly, proving recovery, not merely containment.
//
// This calls commitIntegratedCandidate directly rather than through the full
// scheduler: the window it tests - after a real Git merge, before this run's
// own journal write - has no provider invocation to pause at, exactly as
// R4's own review response documents (see the Scenario F discussion), so a
// full-stack tick-boundary reproduction is not possible; the function under
// test is exercised with the exact same real Git workspace, contract and
// resolver composeIntegration itself would use.
func TestComposeIntegrationPostMergeFailureIsContainedAndRecovers(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	const issueA, issueB = fleetFirstIssue, fleetFirstIssue + 1
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	// An ordinary two-unit graph - no integrator unit at all. This test needs
	// only REAL, current admitted inputs to build a real integration.Contract
	// against; it exercises commitIntegratedCandidate directly, never through
	// WorkGraph activation for a third unit.
	view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		{ID: "b", Purpose: "land b", Role: domain.RoleImplementer, Issue: issueB},
	})
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	a, b := settled.unit(t, "a"), settled.unit(t, "b")
	inputs := orchestration.WorkUnitInputs{
		{UnitID: "a", UnitOutput: orchestration.UnitOutput{HandoffID: a.Output.HandoffID, RunID: a.RunID,
			CandidateRevision: a.Output.CandidateRevision, CandidateTree: a.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
		{UnitID: "b", UnitOutput: orchestration.UnitOutput{HandoffID: b.Output.HandoffID, RunID: b.RunID,
			CandidateRevision: b.Output.CandidateRevision, CandidateTree: b.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
	}
	contract, err := integration.NewContract(view.GraphID, "fault-injection-unit", fixture.base, inputs)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := supervisor.engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	const faultRunID = "run-fault-injection-test"
	created, err := CreateCandidateClone(fixture.stateDir, faultRunID, fixture.deps.Remote.URL, fixture.base, fixture.deps.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ws := &created
	sources, readErr := engine.integrationSources(view.GraphID)
	result, err := IntegrateInputs(ws, contract, sources)
	if *readErr != nil {
		t.Fatal(*readErr)
	}
	if err != nil {
		t.Fatalf("the real merge itself failed, before this test's injected fault: %v", err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("the real merge did not integrate cleanly: %+v", result)
	}
	realCandidate := *result.Candidate
	headAfterRealMerge, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterRealMerge) != realCandidate.Revision {
		t.Fatalf("workspace HEAD %s does not match the real merge commit %s", headAfterRealMerge, realCandidate.Revision)
	}

	// Inject the fault: a candidate revision integratedChangedPaths cannot
	// possibly read, forcing commitIntegratedCandidate's OWN later step to
	// fail - AFTER the real Git merge above already advanced HEAD, exactly
	// B4's window.
	corrupted := realCandidate
	corrupted.Revision = strings.Repeat("f", 40)
	eff := engine.commitIntegratedCandidate(&runState{}, RunOperation{}, ws, contract, corrupted)
	if eff.state != OperationFailed {
		t.Fatalf("a post-merge preparation failure settled as %s, not failed", eff.state)
	}
	if len(eff.events) != 0 {
		t.Fatalf("a contained post-merge failure journalled %d events; want none (no false commit)", len(eff.events))
	}
	// The canonical containment route actually restored the workspace - the
	// real merge commit's advanced HEAD is gone, not silently stranded.
	headAfterContainment, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterContainment) != fixture.base {
		t.Fatalf("workspace HEAD after containment is %s, want restored to base %s", headAfterContainment, fixture.base)
	}

	// Recovery, not merely containment: a fresh attempt against the
	// now-restored workspace composes cleanly again (the restored workspace
	// is genuinely usable, not left in some intermediate state), and the
	// EXACT step this test injected the fault into - integratedChangedPaths -
	// succeeds on the real, uncorrupted result.
	sources2, readErr2 := engine.integrationSources(view.GraphID)
	result2, err := IntegrateInputs(ws, contract, sources2)
	if *readErr2 != nil {
		t.Fatal(*readErr2)
	}
	if err != nil || result2.Status != integration.StatusIntegrated {
		t.Fatalf("the recovery attempt did not integrate cleanly: status=%v err=%v", result2.Status, err)
	}
	if _, err := integratedChangedPaths(ws.Dir, contract.BaseRevision, result2.Candidate.Revision); err != nil {
		t.Fatalf("the exact step this test faulted still fails after recovery: %v", err)
	}
}
