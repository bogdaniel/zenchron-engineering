package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #472 acceptance fixture reuses the #470 fleet: one governed repository,
// controlled workers of two provider kinds, the real scheduler and its real
// ceilings. Only the graph is new.
//
//	A (101)
//	├── B (102)
//	└── C (103)
//	     \ /
//	      D (104)
const (
	graphUnitA = fleetFirstIssue
	graphUnitB = fleetFirstIssue + 1
	graphUnitC = fleetFirstIssue + 2
	graphUnitD = fleetFirstIssue + 3
	// A second, UNRELATED branch, so "one dead branch does not stop the
	// others" has something to be true of.
	graphUnitE = fleetFirstIssue + 4
)

func graphDiamond() []orchestration.WorkUnit {
	return []orchestration.WorkUnit{
		{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: graphUnitA},
		{ID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: graphUnitB, DependsOn: []string{"a"}},
		{ID: "c", Purpose: "land the writer", Role: domain.RoleImplementer, Issue: graphUnitC, DependsOn: []string{"a"}},
		{ID: "d", Purpose: "land the surface", Role: domain.RoleImplementer, Issue: graphUnitD, DependsOn: []string{"b", "c"}},
	}
}

func (f *fleetFixture) adoptGraph(supervisor *Supervisor, agent string, revision int, units []orchestration.WorkUnit) WorkGraphView {
	f.t.Helper()
	view, err := supervisor.AdoptWorkGraph(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: agent, Operator: "operator@example",
		WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: revision, Units: units},
	})
	if err != nil {
		f.t.Fatalf("adopt revision %d: %v", revision, err)
	}
	return view
}

func (f *fleetFixture) graph(graphID string) WorkGraphView {
	f.t.Helper()
	view, err := WorkGraphStatus(f.store, f.stateDir, graphID, f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return view
}

func (v WorkGraphView) unit(t *testing.T, unitID string) WorkGraphUnitView {
	t.Helper()
	for _, unit := range v.Units {
		if unit.UnitID == unitID {
			return unit
		}
	}
	t.Fatalf("graph %s has no unit %q", v.GraphID, unitID)
	return WorkGraphUnitView{}
}

func (v WorkGraphView) states() map[string]orchestration.UnitState {
	states := map[string]orchestration.UnitState{}
	for _, unit := range v.Units {
		states[unit.UnitID] = unit.State
	}
	return states
}

// unitSettled is whether this pass could still move the unit on its own.
func unitSettled(unit WorkGraphUnitView) bool {
	switch unit.State {
	case orchestration.UnitBlocked, orchestration.UnitInvalidated, orchestration.UnitUnknown:
		return true
	case orchestration.UnitReady:
		return false
	}
	switch orchestration.ItemState(unit.State) {
	case orchestration.ItemCompleted, orchestration.ItemPartial, orchestration.ItemFailed, orchestration.ItemStopped:
		return true
	case orchestration.ItemHandoffPending:
		// Admission is a later pass; wait for its durable decision.
		return unit.Child == nil || unit.Child.Handoff != orchestration.HandoffReported
	}
	return false
}

// driveGraph ticks until no unit can move, calling observe after every pass so a
// test can assert an invariant that only holds mid-flight.
func (f *fleetFixture) driveGraph(supervisor *Supervisor, graphID string, observe func(WorkGraphView)) WorkGraphView {
	f.t.Helper()
	for range 160 {
		report, err := supervisor.Tick(context.Background())
		if err != nil {
			f.t.Fatal(err)
		}
		for _, problem := range report.WorkGraphs {
			f.t.Log("work graph: " + problem)
		}
		for _, problem := range report.Orchestration {
			f.t.Log("orchestration: " + problem)
		}
		f.clock.advance(61 * time.Second)
		view := f.graph(graphID)
		if observe != nil {
			observe(view)
		}
		settled := 0
		for _, unit := range view.Units {
			if unitSettled(unit) {
				settled++
			}
		}
		if settled == len(view.Units) && len(view.Frontier) == 0 {
			return view
		}
	}
	view := f.graph(graphID)
	for _, unit := range view.Units {
		f.t.Logf("unit %s: %s run=%s reason=%s", unit.UnitID, unit.State, unit.RunID, unit.Reason)
	}
	f.t.Fatalf("the graph never settled: %+v frontier=%v", view.Counts, view.Frontier)
	return view
}

// TestTheDiamondAdvancesOnAdmittedHandoffsAlone is #472 acceptance 1, 2, 3 and 8.
//
// Only A is initially runnable; B and C get no child run until A's handoff is
// ADMITTED, not when its worker exits; B and C then become runnable together; D
// waits for both. The whole thing holds identically for two provider kinds,
// because provider kind appears in none of it.
func TestTheDiamondAdvancesOnAdmittedHandoffsAlone(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			fixture := newFleetFixture(t, 4)
			supervisor := fixture.supervisor()
			view := fixture.adoptGraph(supervisor, agent, 1, graphDiamond())
			// ADOPTION STARTS NOTHING, and only the root is runnable.
			if got := view.states(); got["a"] != orchestration.UnitReady ||
				got["b"] != orchestration.UnitBlocked || got["c"] != orchestration.UnitBlocked || got["d"] != orchestration.UnitBlocked {
				t.Fatalf("states after adoption = %v", got)
			}
			runs, err := fixture.store.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("adopting a graph created %d runs", len(runs))
			}

			// The invariant checked after EVERY pass: a unit's dependents stay
			// runless until that unit has an admitted output, whatever its
			// worker has already done.
			reportedButUnadmitted := 0
			settled := fixture.driveGraph(supervisor, view.GraphID, func(current WorkGraphView) {
				a := current.unit(t, "a")
				if a.Child != nil && a.Child.Handoff == orchestration.HandoffReported && a.Output == nil {
					reportedButUnadmitted++
					for _, dependent := range []string{"b", "c"} {
						unit := current.unit(t, dependent)
						if unit.RunID != "" || unit.State != orchestration.UnitBlocked {
							t.Fatalf("%s is %s with run %q while a's handoff is only REPORTED", dependent, unit.State, unit.RunID)
						}
					}
				}
				for _, pair := range [][2]string{{"b", "a"}, {"c", "a"}, {"d", "b"}, {"d", "c"}} {
					if current.unit(t, pair[0]).RunID != "" && current.unit(t, pair[1]).Output == nil {
						t.Fatalf("%s has a child run while its dependency %s has no admitted output", pair[0], pair[1])
					}
				}
			})
			if reportedButUnadmitted == 0 {
				t.Fatal("a's handoff was never observed reported-but-unadmitted, so the law under test was never exercised")
			}
			for _, unit := range settled.Units {
				if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
					t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
				}
				if unit.Output == nil || unit.Output.CandidateRevision == "" {
					t.Fatalf("unit %s completed with no admitted output subject", unit.UnitID)
				}
			}
			if settled.Counts.Total != 4 || settled.Counts.Activated.Completed != 4 {
				t.Fatalf("counts = %+v", settled.Counts)
			}
			// Every unit is one ORDINARY run of its own issue, created through
			// the one-issue batch path, and bound to the named agent.
			for _, unit := range settled.Units {
				run, ok := storedRun(t, fixture.phase8Fixture, unit.RunID)
				if !ok || run.Goal != issueGoal("acme/repo", unit.Issue) || run.AgentID != agent ||
					run.Orchestration == nil || run.Plan != nil {
					t.Fatalf("unit %s is not an ordinary orchestrated run of issue %d: %+v", unit.UnitID, unit.Issue, run)
				}
			}
		})
	}
}

// TestTheSchedulerStillBoundsGraphConcurrency is #472 acceptance 2's second half:
// B and C are runnable together, and the EXISTING ceiling alone decides whether
// they execute together. The graph has no pool to compete with it.
func TestTheSchedulerStillBoundsGraphConcurrency(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			fixture := newFleetFixture(t, capacity)
			fixture.worker.hold = 150 * time.Millisecond
			supervisor := fixture.supervisor()
			view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
			bothRunnable := false
			settled := fixture.driveGraph(supervisor, view.GraphID, func(current WorkGraphView) {
				b, c := current.unit(t, "b"), current.unit(t, "c")
				if b.RunID != "" && c.RunID != "" {
					bothRunnable = true
				}
			})
			if !bothRunnable {
				t.Fatal("b and c never both had a child run, so concurrency was never offered")
			}
			if settled.Counts.Activated.Completed != 4 {
				t.Fatalf("counts under a ceiling of %d = %+v", capacity, settled.Counts)
			}
			fixture.worker.mu.Lock()
			peak := fixture.worker.peak
			fixture.worker.mu.Unlock()
			if peak > capacity {
				t.Fatalf("%d workers ran at once under a ceiling of %d", peak, capacity)
			}
		})
	}
}

// TestAFailedUnitBlocksItsDependentsAndNothingElse is #472 acceptance 4.
func TestAFailedUnitBlocksItsDependentsAndNothingElse(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	// A fifth unit on its own, depending on nothing: the unrelated branch.
	units := append(graphDiamond(), orchestration.WorkUnit{
		ID: "e", Purpose: "land the unrelated thing", Role: domain.RoleImplementer, Issue: graphUnitE,
	})
	view := fixture.adoptGraph(supervisor, "claude", 1, units)
	// C's worker FAILS. Its child run identity is the ordinary deterministic
	// one for its issue, which is what lets the behaviour be set before C is
	// activated at all; the assertion below fails loudly if it is not.
	failing := issueRunOf(t, fixture, graphUnitC)
	fixture.worker.set(failing, fleetFails)
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	states := settled.states()
	if settled.unit(t, "c").RunID != failing {
		t.Fatalf("c's child is %s, not the deterministic identity %s whose worker was set to fail", settled.unit(t, "c").RunID, failing)
	}
	if states["c"] != orchestration.UnitState(orchestration.ItemFailed) {
		t.Fatalf("c is %s, want failed: %s", states["c"], settled.unit(t, "c").Reason)
	}
	if states["d"] != orchestration.UnitBlocked {
		t.Fatalf("d is %s behind a failed dependency", states["d"])
	}
	if !strings.Contains(settled.unit(t, "d").Reason, "will never transfer an admitted handoff") {
		t.Fatalf("d's reason does not say the branch is dead: %q", settled.unit(t, "d").Reason)
	}
	if settled.unit(t, "d").RunID != "" {
		t.Fatalf("d got a child run behind a failed dependency: %s", settled.unit(t, "d").RunID)
	}
	for _, unitID := range []string{"a", "b", "e"} {
		if states[unitID] != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unrelated unit %s is %s: %s", unitID, states[unitID], settled.unit(t, unitID).Reason)
		}
	}
}

// TestRestartReproducesTheFrontierWithoutDuplicateChildren is #472 acceptance 5,
// and the lost-reply case: the graph is re-adopted and the supervisor rebuilt
// from the store alone, repeatedly, mid-flight.
func TestRestartReproducesTheFrontierWithoutDuplicateChildren(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	first := fixture.adoptGraph(fixture.supervisor(), "claude", 1, graphDiamond())
	// A RESUBMISSION of the same proposal finds the same graph at the same
	// revision, with the same content identity.
	again := fixture.adoptGraph(fixture.supervisor(), "claude", 1, graphDiamond())
	if again.GraphID != first.GraphID || again.Revision != 1 || again.RevisionDigest != first.RevisionDigest {
		t.Fatalf("a resubmission produced %s revision %d (%s) beside %s revision %d (%s)",
			again.GraphID, again.Revision, again.RevisionDigest, first.GraphID, first.Revision, first.RevisionDigest)
	}
	graphs, err := fixture.store.WorkGraphs()
	if err != nil || len(graphs) != 1 {
		t.Fatalf("graphs = %d (%v), want exactly one", len(graphs), err)
	}

	// Every pass runs on a supervisor built fresh from the store, and the
	// frontier is read before and after each restart.
	var runs []string
	for pass := range 160 {
		supervisor := fixture.supervisor()
		before := fixture.graph(first.GraphID)
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		// A restart between the pass and the read reproduces the identical
		// projection: nothing the frontier depends on lived in that process.
		after := fixture.graph(first.GraphID)
		restarted := fixture.graph(first.GraphID)
		if projectionJSON(t, after) != projectionJSON(t, restarted) {
			t.Fatalf("pass %d: the projection changed when read again\n%s\n%s",
				pass, projectionJSON(t, after), projectionJSON(t, restarted))
		}
		if pass > 0 && len(before.Frontier) > 0 && len(after.Frontier) > 0 &&
			strings.Join(before.Frontier, ",") == strings.Join(after.Frontier, ",") &&
			before.unit(t, after.Frontier[0]).RunID != "" {
			t.Fatalf("pass %d: a unit stayed in the frontier after being activated", pass)
		}
		runs = recordRuns(t, fixture, runs)
		settled := 0
		for _, unit := range after.Units {
			if unitSettled(unit) {
				settled++
			}
		}
		if settled == len(after.Units) && len(after.Frontier) == 0 {
			if after.Counts.Activated.Completed != 4 {
				t.Fatalf("counts = %+v", after.Counts)
			}
			// EXACTLY four runs exist: one per unit, no replay duplicate.
			if len(runs) != 4 {
				t.Fatalf("%d runs exist for a four unit graph: %v", len(runs), runs)
			}
			return
		}
	}
	t.Fatal("the graph never settled across restarts")
}

// projectionJSON is the view's canonical content, so two reads are compared by
// what they SAY rather than by where their child views happen to be allocated.
func projectionJSON(t *testing.T, view WorkGraphView) string {
	t.Helper()
	document, err := CanonicalJSON(view)
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

// recordRuns accumulates every run identity ever observed, so a duplicate that
// appeared and terminated between passes is still caught.
func recordRuns(t *testing.T, fixture *fleetFixture, seen []string) []string {
	t.Helper()
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		known := false
		for _, id := range seen {
			known = known || id == run.ID
		}
		if !known {
			seen = append(seen, run.ID)
		}
	}
	sort.Strings(seen)
	return seen
}

// TestAReplacedUpstreamSubjectInvalidatesDownstreamSatisfaction is #472
// acceptance 7, through durable state: once completed is NOT always completed.
func TestAReplacedUpstreamSubjectInvalidatesDownstreamSatisfaction(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 4 {
		t.Fatalf("the graph did not complete first: %+v", settled.Counts)
	}
	before := settled.unit(t, "a")
	// A's run transfers a SECOND admitted handoff, bound to a different
	// candidate: the exact shape of an upstream output being replaced.
	replaceAdmittedOutput(t, fixture, view.GraphID, before)
	after := fixture.graph(view.GraphID)
	states := after.states()
	for _, unitID := range []string{"b", "c", "d"} {
		if states[unitID] != orchestration.UnitInvalidated {
			t.Fatalf("unit %s is %s after its upstream output was replaced, want invalidated: %s",
				unitID, states[unitID], after.unit(t, unitID).Reason)
		}
	}
	if states["a"] != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("a is %s", states["a"])
	}
	if len(after.Frontier) != 0 {
		t.Fatalf("invalidated units are in the frontier: %v", after.Frontier)
	}
	// And the next pass starts nothing: invalidated work is not silently
	// re-performed, and no second child run appears.
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.supervisor().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterTick, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTick) != len(runs) {
		t.Fatalf("a pass over an invalidated graph created %d runs", len(afterTick)-len(runs))
	}
}

// replaceAdmittedOutput journals a second reported handoff for one unit's child
// and admits it against a different candidate. It writes only through the
// durable APIs the runtime itself uses.
func replaceAdmittedOutput(t *testing.T, fixture *fleetFixture, graphID string, unit WorkGraphUnitView) {
	t.Helper()
	if unit.Child == nil || unit.Output == nil {
		t.Fatalf("unit %s has no admitted output to replace", unit.UnitID)
	}
	events, err := fixture.store.Events(unit.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var reported HandoffReportedPayload
	for _, event := range events {
		if event.Type == EventHandoffReported {
			if err := json.Unmarshal(event.Payload, &reported); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reported.OperationID == "" {
		t.Fatalf("unit %s journalled no reported handoff", unit.UnitID)
	}
	next := reported
	next.Attempt = reported.Attempt + 1
	payload, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "event-replacement-" + unit.UnitID, RunID: unit.RunID,
		Type: EventHandoffReported, OccurredAt: fixture.clock.Now(),
		OperationID: reported.OperationID, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	id, err := orchestration.HandoffID(unit.RunID, next.OperationID, next.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	stored, found, err := fixture.store.OrchestrationBatch(batchOf(t, fixture, graphID, unit.UnitID))
	if err != nil || !found {
		t.Fatalf("batch for unit %s: found=%t err=%v", unit.UnitID, found, err)
	}
	admitted, err := fixture.store.RunHandoffs(unit.RunID)
	if err != nil || len(admitted) == 0 {
		t.Fatalf("run %s has %d admitted handoffs (%v)", unit.RunID, len(admitted), err)
	}
	replacement := admitted[len(admitted)-1]
	replacement.ID, replacement.Producer.Attempt = id, next.Attempt
	replacement.BatchID = stored.ID
	replacement.Subject.CandidateRevision = strings.Repeat("9", len(replacement.Subject.CandidateRevision))
	replacement.AdmittedAt = fixture.clock.Now()
	inserted, err := fixture.store.AdmitHandoff(replacement)
	if err != nil || !inserted {
		t.Fatalf("admitting the replacement handoff: inserted=%t err=%v", inserted, err)
	}
}

// issueRunOf is the ordinary first-generation run identity of one issue: the
// same identity a direct `run issue` or a #470 batch would decide.
func issueRunOf(t *testing.T, fixture *fleetFixture, issue int) string {
	t.Helper()
	id, err := issueRunID("acme/repo", issue, fixture.deps.ConfigDigest, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func batchOf(t *testing.T, fixture *fleetFixture, graphID, unitID string) string {
	t.Helper()
	activations, err := fixture.store.WorkUnitActivations(graphID)
	if err != nil {
		t.Fatal(err)
	}
	activation, found := activations[unitID]
	if !found {
		t.Fatalf("unit %s has no activation", unitID)
	}
	return activation.BatchID
}

// TestAMutationIsAdmittedOnlyByDeterministicValidation is #472 acceptance 6 and
// the planner/model proposal seam: a proposal that adds a cycle, or rewrites
// work already activated, is refused BEFORE anything executes, and a legitimate
// addition becomes runnable through the same frontier as everything else.
func TestAMutationIsAdmittedOnlyByDeterministicValidation(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	// One pass, so "a" is activated and therefore frozen.
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(61 * time.Second)
	if fixture.graph(view.GraphID).unit(t, "a").RunID == "" {
		t.Fatal("a was not activated by the first pass")
	}
	refusals := map[string][]orchestration.WorkUnit{}
	cyclic := graphDiamond()
	cyclic[0].DependsOn = []string{"d"}
	refusals["a cycle"] = cyclic
	rewritten := graphDiamond()
	rewritten[0].Issue = graphUnitE
	refusals["an activated unit's issue"] = rewritten
	refusals["an activated unit's removal"] = graphDiamond()[1:]
	for name, units := range refusals {
		t.Run(name, func(t *testing.T) {
			_, err := supervisor.AdoptWorkGraph(context.Background(), ControlRequest{
				Repository: "acme/repo", Agent: "claude", Operator: "operator@example",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 2, Units: units},
			})
			if err == nil {
				t.Fatal("the proposal was adopted")
			}
			current := fixture.graph(view.GraphID)
			if current.Revision != 1 {
				t.Fatalf("a refused proposal moved the graph to revision %d", current.Revision)
			}
		})
	}
	// A revision is an APPEND: it moves no activation, so the budget the
	// activated unit's child run has already consumed is untouched by it.
	beforeMutation := fixture.graph(view.GraphID).unit(t, "a")
	// A legitimate addition: a unit downstream of the diamond.
	grown := append(graphDiamond(), orchestration.WorkUnit{
		ID: "e", Purpose: "land the release note", Role: domain.RoleImplementer,
		Issue: graphUnitE, DependsOn: []string{"d"},
	})
	second := fixture.adoptGraph(supervisor, "claude", 2, grown)
	if second.Revision != 2 || second.unit(t, "e").State != orchestration.UnitBlocked {
		t.Fatalf("revision 2 did not land with e blocked: %+v", second.states())
	}
	afterMutation := second.unit(t, "a")
	if afterMutation.RunID != beforeMutation.RunID || afterMutation.State != beforeMutation.State {
		t.Fatalf("revision 2 moved the activated unit from %s/%s to %s/%s",
			beforeMutation.State, beforeMutation.RunID, afterMutation.State, afterMutation.RunID)
	}
	activations, err := fixture.store.WorkUnitActivations(view.GraphID)
	if err != nil || activations["a"].InputsDigest == "" {
		t.Fatalf("activations after the mutation = %+v (%v)", activations, err)
	}
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 5 {
		t.Fatalf("the grown graph did not complete: %+v", settled.Counts)
	}
	// Both revisions are kept. A revision is an append, so the first one is
	// still readable and the activations it produced were never rewritten.
	if _, found, err := fixture.store.WorkGraphRevision(view.GraphID, 1); err != nil || !found {
		t.Fatalf("revision 1 was not kept: found=%t err=%v", found, err)
	}
}

// TestAWorkGraphRefusesWhatItCannotGovern covers the intake boundary.
func TestAWorkGraphRefusesWhatItCannotGovern(t *testing.T) {
	fixture := newFleetFixture(t, 2)
	supervisor := fixture.supervisor()
	cases := map[string]struct {
		request ControlRequest
		want    string
	}{
		"ungoverned repository": {
			request: ControlRequest{Repository: "other/repo", Agent: "claude",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: graphDiamond()}},
			want: "is not governed by this supervisor",
		},
		"no agent": {
			request: ControlRequest{Repository: "acme/repo",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: graphDiamond()}},
			want: "names its execution agent explicitly",
		},
		"unknown agent": {
			request: ControlRequest{Repository: "acme/repo", Agent: "ghost",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: graphDiamond()}},
			want: "ghost",
		},
		"no proposal": {
			request: ControlRequest{Repository: "acme/repo", Agent: "claude"},
			want:    "carries the proposed revision",
		},
		"a first revision is 1": {
			request: ControlRequest{Repository: "acme/repo", Agent: "claude",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 4, Units: graphDiamond()}},
			want: "its first revision is 1, not 4",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := supervisor.AdoptWorkGraph(context.Background(), test.request); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one containing %q", err, test.want)
			}
		})
	}
	graphs, err := fixture.store.WorkGraphs()
	if err != nil || len(graphs) != 0 {
		t.Fatalf("a refused request stored %d graphs (%v)", len(graphs), err)
	}
}
