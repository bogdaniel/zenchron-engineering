package runtime

// How a WorkGraph unit's child execution is BOUND (#472): to the unit, to the
// exact admitted inputs it consumes, and to nothing else - and what that
// execution is given. An earlier run of the same issue is a different execution
// and can never satisfy a unit; a dependency delivers its handoff rather than
// only gating time; and a readiness hold outside the graph keeps a unit out of
// the frontier until its owner lifts it.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestAnEarlierRunOfTheSameIssueCannotSatisfyAWorkUnit is the #472 binding
// regression.
//
// A unit's child execution is bound to the UNIT and the exact inputs it was
// activated against. An ordinary orchestration of the same issue, or another
// graph's unit on it, is a different execution: it never executed against these
// inputs, so it can never satisfy this unit - however finished it is.
func TestAnEarlierRunOfTheSameIssueCannotSatisfyAWorkUnit(t *testing.T) {
	t.Run("a direct batch's finished run", func(t *testing.T) {
		fixture := newFleetFixture(t, 4)
		supervisor := fixture.supervisor()
		// A DIRECT #470 batch on issue 101 alone, driven until its worker has
		// finished and its handoff is admitted.
		direct := fixture.orchestrate(supervisor, "claude", []int{graphUnitA})
		settled := fixture.drive(supervisor, direct.BatchID)
		if settled.Counts.Completed != 1 {
			t.Fatalf("the direct batch did not finish: %+v", settled.Counts)
		}
		directRun := settled.Items[0].RunID
		assertUnitDoesNotInherit(t, fixture, supervisor, directRun, direct.BatchID)
	})

	t.Run("another graph's finished unit", func(t *testing.T) {
		fixture := newFleetFixture(t, 4)
		supervisor := fixture.supervisor()
		// One graph of a SINGLE unit on issue 101, driven to completion.
		first, err := supervisor.AdoptWorkGraph(context.Background(), ControlRequest{
			Repository: "acme/repo", Agent: "claude", Operator: "operator@example",
			WorkGraph: &orchestration.WorkGraphProposal{Name: "other-graph", Revision: 1,
				Units: graphDiamond()[:1]},
		})
		if err != nil {
			t.Fatal(err)
		}
		settled := fixture.driveGraph(supervisor, first.GraphID, nil)
		if settled.Counts.Activated.Completed != 1 {
			t.Fatalf("the first graph's unit did not complete: %+v", settled.Counts)
		}
		owner := settled.unit(t, "a")
		assertUnitDoesNotInherit(t, fixture, supervisor, owner.RunID, batchOf(t, fixture, first.GraphID, "a"))
	})
}

// assertUnitDoesNotInherit adopts the diamond over an issue some OTHER execution
// already owns, and proves the diamond's unit neither takes that run nor reads
// as satisfied by it.
func assertUnitDoesNotInherit(t *testing.T, fixture *fleetFixture, supervisor *Supervisor, ownedRun, ownedBatch string) {
	t.Helper()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	for range 3 {
		report, err := supervisor.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		current := fixture.graph(view.GraphID)
		a := current.unit(t, "a")
		if a.RunID == ownedRun {
			t.Fatalf("unit a took over run %s, which belongs to %s", ownedRun, ownedBatch)
		}
		if a.State == orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit a reads as completed without a child execution of its own: run=%q", a.RunID)
		}
		if b := current.unit(t, "b"); b.RunID != "" || b.State != orchestration.UnitBlocked {
			t.Fatalf("b is %s with run %q behind a unit that has produced nothing", b.State, b.RunID)
		}
		// The issue is already owned by live work this graph did not create, so
		// activation is REFUSED and says so, rather than adopting it.
		if a.RunID == "" && len(report.WorkGraphs) == 0 {
			t.Fatalf("unit a was neither activated nor explained: %+v", report.WorkGraphs)
		}
	}
	activations, err := fixture.store.WorkUnitActivations(view.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	if activation, found := activations["a"]; found {
		if activation.RunID == ownedRun || activation.BatchID == ownedBatch {
			t.Fatalf("unit a's activation points at the other execution: %+v", activation)
		}
	}
}

// TestAUnitBatchIsBoundToItsUnitAndInputs proves the durable binding on the
// happy path: the batch a unit activates names the graph, the unit and the exact
// admitted inputs, and is not the batch a direct orchestration of that issue
// would use.
func TestAUnitBatchIsBoundToItsUnitAndInputs(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 4 {
		t.Fatalf("the graph did not complete: %+v", settled.Counts)
	}
	a := settled.unit(t, "a")
	for _, unitID := range []string{"a", "b", "c", "d"} {
		unit := settled.unit(t, unitID)
		batch, found, err := fixture.store.OrchestrationBatch(batchOf(t, fixture, view.GraphID, unitID))
		if err != nil || !found {
			t.Fatalf("unit %s has no batch: found=%t err=%v", unitID, found, err)
		}
		if batch.Origin == nil || batch.Origin.GraphID != view.GraphID || batch.Origin.UnitID != unitID {
			t.Fatalf("unit %s's batch does not name it: %+v", unitID, batch.Origin)
		}
		direct, err := orchestration.BatchID("acme/repo", "claude", []int{unit.Issue})
		if err != nil {
			t.Fatal(err)
		}
		if batch.ID == direct {
			t.Fatalf("unit %s's batch is the issue's direct batch", unitID)
		}
		// The recorded input set is the one the activation digested, and the
		// digest the activation stored is the digest of that set.
		digest, err := batch.Origin.Inputs.Digest()
		if err != nil {
			t.Fatal(err)
		}
		activation := fixture.activation(t, view.GraphID, unitID)
		if digest != activation.InputsDigest {
			t.Fatalf("unit %s recorded digest %s over an input set digesting %s", unitID, activation.InputsDigest, digest)
		}
		if err := batch.Origin.Inputs.Validate(); err != nil {
			t.Fatalf("unit %s recorded an incomplete input set: %v", unitID, err)
		}
	}
	// B consumes exactly A's admitted output.
	inputs := fixture.batchOrigin(t, view.GraphID, "b").Inputs
	if len(inputs) != 1 || inputs[0].UnitID != "a" || inputs[0].RunID != a.RunID ||
		inputs[0].HandoffID != a.Output.HandoffID || inputs[0].CandidateRevision != a.Output.CandidateRevision {
		t.Fatalf("b's recorded inputs are not a's admitted output: %+v vs %+v", inputs, a.Output)
	}
	// D consumes both.
	if dependencies := fixture.batchOrigin(t, view.GraphID, "d").Inputs; len(dependencies) != 2 {
		t.Fatalf("d recorded %d inputs, want two: %+v", len(dependencies), dependencies)
	}
}

func (f *fleetFixture) activation(t *testing.T, graphID, unitID string) WorkUnitActivation {
	t.Helper()
	activations, err := f.store.WorkUnitActivations(graphID)
	if err != nil {
		t.Fatal(err)
	}
	activation, found := activations[unitID]
	if !found {
		t.Fatalf("unit %s has no activation", unitID)
	}
	return activation
}

func (f *fleetFixture) batchOrigin(t *testing.T, graphID, unitID string) orchestration.BatchOrigin {
	t.Helper()
	batch, found, err := f.store.OrchestrationBatch(batchOf(t, f, graphID, unitID))
	if err != nil || !found || batch.Origin == nil {
		t.Fatalf("unit %s has no origin-bearing batch: found=%t err=%v", unitID, found, err)
	}
	return *batch.Origin
}

// TestADependentUnitsExecutionReceivesTheExactUpstreamHandoff is the #472
// delivery regression: a dependency does not only gate time, it DELIVERS. The
// invocation of a dependent unit's child run is given the exact admitted
// upstream outputs its activation was bound to - commit, tree, the handoff
// identity and the producer's own report - so the recorded input set is a fact
// about the execution rather than bookkeeping beside it.
func TestADependentUnitsExecutionReceivesTheExactUpstreamHandoff(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 4 {
		t.Fatalf("the graph did not complete: %+v", settled.Counts)
	}
	a, b, d := settled.unit(t, "a"), settled.unit(t, "b"), settled.unit(t, "d")

	// THE ROOT receives nothing: it consumes no upstream output.
	if upstream := fixture.worker.request(a.RunID).Upstream; len(upstream) != 0 {
		t.Fatalf("the root unit's invocation was given %d upstream outputs: %+v", len(upstream), upstream)
	}
	// B receives exactly A's.
	upstream := fixture.worker.request(b.RunID).Upstream
	if len(upstream) != 1 {
		t.Fatalf("b's invocation was given %d upstream outputs, want one: %+v", len(upstream), upstream)
	}
	got := upstream[0]
	if got.StageID != "a" || got.RunID != a.RunID ||
		got.Commit != a.Output.CandidateRevision || got.Tree != a.Output.CandidateTree {
		t.Fatalf("b's invocation was given %+v, want a's exact subject %+v", got, a.Output)
	}
	if got.Handoff == nil || got.Handoff.ID != a.Output.HandoffID ||
		got.Handoff.Outcome != orchestration.OutcomeCompleted || got.Handoff.Summary == "" {
		t.Fatalf("b's invocation was not given a's admitted handoff report: %+v", got.Handoff)
	}
	if strings.TrimSpace(got.Diff) == "" {
		t.Fatalf("b's invocation was given no upstream diff for run %s", a.RunID)
	}
	// D receives BOTH its dependencies', and nothing else.
	both := fixture.worker.request(d.RunID).Upstream
	if len(both) != 2 {
		t.Fatalf("d's invocation was given %d upstream outputs, want two: %+v", len(both), both)
	}
	producers := []string{both[0].StageID, both[1].StageID}
	sort.Strings(producers)
	if producers[0] != "b" || producers[1] != "c" {
		t.Fatalf("d's invocation was given outputs from %v", producers)
	}
	for _, input := range both {
		if input.Handoff == nil || input.Handoff.ID == "" {
			t.Fatalf("d was given an upstream with no admitted handoff: %+v", input)
		}
	}

	// AN ORDINARY #470 CHILD receives none: it has no unit origin.
	direct := fixture.orchestrate(supervisor, "claude", []int{graphUnitE})
	ordinary := fixture.drive(supervisor, direct.BatchID)
	if upstream := fixture.worker.request(ordinary.Items[0].RunID).Upstream; len(upstream) != 0 {
		t.Fatalf("an ordinary orchestrated child was given %d upstream outputs", len(upstream))
	}
}

// TestAHeldUnitIsNotActivatedUntilItsDecisionResolves is the #508 integration
// seam end to end: #472 represents an unresolved decision and resolves nothing.
//
// A held unit is not in the frontier, so the supervisor does not activate it; the
// moment its owner stops reporting the hold the ordinary frontier computation
// includes it and it runs. Nothing a worker does can lift a hold.
func TestAHeldUnitIsNotActivatedUntilItsDecisionResolves(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	// The ROOT is held before anything is adopted, so the graph cannot start.
	fixture.hold("a", "decision-7", "the operator must choose the schema")
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	if held := view.unit(t, "a"); held.State != orchestration.UnitAwaitingDecision || held.AwaitingDecision == nil {
		t.Fatalf("the held root is %s: %+v", held.State, held.AwaitingDecision)
	}
	for range 3 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		current := fixture.graph(view.GraphID)
		if a := current.unit(t, "a"); a.RunID != "" || a.State != orchestration.UnitAwaitingDecision {
			t.Fatalf("a held unit was activated: %s run=%q", a.State, a.RunID)
		}
		if len(current.Frontier) != 0 {
			t.Fatalf("a held unit is in the frontier: %v", current.Frontier)
		}
	}
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a held graph created %d runs", len(runs))
	}
	// AN UNANSWERABLE OWNER holds everything: the pass refuses rather than
	// activating past a hold that may exist.
	fixture.holdError = errors.New("the decision store is unreachable")
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(61 * time.Second)
	if len(report.WorkGraphs) == 0 {
		t.Fatal("an unanswerable hold source was not reported")
	}
	if after, err := fixture.store.Runs(); err != nil || len(after) != 0 {
		t.Fatalf("a pass with an unanswerable hold source created %d runs (%v)", len(after), err)
	}
	fixture.holdError = nil

	// RESOLVED: the ordinary frontier includes it again and the graph runs.
	fixture.release("a")
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	if settled.Counts.Activated.Completed != 4 {
		t.Fatalf("the graph did not complete after its decision resolved: %+v", settled.Counts)
	}
}
