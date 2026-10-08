package runtime

// Durable, authorized DecisionResolution (#508), proved end to end against the
// REAL #472 WorkGraph fleet fixture: a real hold, placed and resolved through
// the Supervisor's own governed actions and read back through the REAL
// store-backed SupervisorDependencies.WorkUnitHolds - never the fixture's
// fake hold map every other #472 test uses, because what is under test here
// is exactly the wiring those tests deliberately leave unexercised.

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// supervisorWithRealHolds builds a supervisor exactly as fleetFixture.supervisor
// does, except WorkUnitHolds reads the fixture's REAL durable store instead of
// the in-memory fake every other #472 test plays. This is the one thing these
// tests exist to prove is wired correctly.
func (f *fleetFixture) supervisorWithRealHolds() *Supervisor {
	f.t.Helper()
	registry := supervisorRegistry(f.t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		f.t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: f.store, Clock: f.clock, Owner: "owner-1", StateDir: f.stateDir,
		Liveness: f.deps.Liveness, Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: f.capacity,
		MaxConcurrentObservations: 10, PollInterval: time.Minute, Agents: registry,
		WorkUnitHolds: f.store.WorkGraphHolds,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			deps := f.deps
			deps.Store, deps.Agent, deps.Agents = f.store, agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return supervisor
}

func decisionOperator() string { return "operator@example" }

// realGraph reads the graph through the SAME real, store-backed holds the
// supervisor built with supervisorWithRealHolds decides activation from -
// never fixture.graph's in-memory fake, which these tests never populate.
func (f *fleetFixture) realGraph(graphID string) WorkGraphView {
	f.t.Helper()
	holds, err := f.store.WorkGraphHolds(graphID)
	if err != nil {
		f.t.Fatal(err)
	}
	view, err := WorkGraphStatus(f.store, f.stateDir, graphID, f.clock.Now(), holds)
	if err != nil {
		f.t.Fatal(err)
	}
	return view
}

// TestAWorkUnitHoldConsumesNoCapacityUntilAuthorizedResolution is #508
// acceptance 2, 9, 14-17: a unit held before its first activation gets no
// child run - zero provider process, zero scheduler slot - while its
// dependencies, and an unrelated branch, proceed and consume the scheduler
// normally; resolving the hold through the governed action is what makes the
// unit's FIRST activation happen, with an ordinary fresh run.
func TestAWorkUnitHoldConsumesNoCapacityUntilAuthorizedResolution(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisorWithRealHolds()
	units := append(graphDiamond(), orchestration.WorkUnit{
		ID: "e", Purpose: "land the unrelated thing", Role: domain.RoleImplementer, Issue: graphUnitE,
	})
	view := fixture.adoptGraph(supervisor, "claude", 1, units)

	hold, err := supervisor.PlaceWorkUnitHold(ControlRequest{
		GraphID: view.GraphID, UnitID: "d", Operator: decisionOperator(), Note: "human sign-off before d",
	})
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}

	// Drive manually - NOT fixture.driveGraph, which waits for every unit to
	// settle and a held unit never does - until a, b, c and e are all done.
	// d must stay held throughout and never get a child run: that is the
	// "zero capacity while waiting" claim, not merely an eventual one.
	settledWithoutD := func(view WorkGraphView) bool {
		for _, unit := range view.Units {
			if unit.UnitID == "d" {
				continue
			}
			if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
				return false
			}
		}
		return true
	}
	var observed WorkGraphView
	for range 160 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		observed = fixture.realGraph(view.GraphID)
		if d := observed.unit(t, "d"); d.RunID != "" {
			t.Fatalf("held unit d got a child run before its hold was resolved: %s", d.RunID)
		}
		if settledWithoutD(observed) {
			break
		}
	}
	if !settledWithoutD(observed) {
		t.Fatalf("a, b, c and e never settled behind d's hold: %+v", observed.Counts)
	}
	d := observed.unit(t, "d")
	if d.State != orchestration.UnitAwaitingDecision {
		t.Fatalf("d is %s with every dependency satisfied, want awaiting_decision: %s", d.State, d.Reason)
	}
	if d.AwaitingDecision == nil || d.AwaitingDecision.Reference != hold.ID {
		t.Fatalf("d's hold reference is %+v, want the placed hold %s", d.AwaitingDecision, hold.ID)
	}
	// ZERO CAPACITY: no activation record exists for d at all.
	activations, err := fixture.store.WorkUnitActivations(view.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	if _, activated := activations["d"]; activated {
		t.Fatal("d was activated while its hold stood")
	}

	// Resolve: the governed action, never a worker and never the graph
	// itself.
	resolved, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: hold.ID, DecisionOutcomeKind: orchestration.DecisionAllowDeny,
		DecisionOutcomeValue: orchestration.DecisionAllow, Operator: decisionOperator(), Note: "approved",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Resolution.RequestID != hold.ID {
		t.Fatalf("resolution names request %s, want %s", resolved.Resolution.RequestID, hold.ID)
	}

	final := fixture.driveGraph(supervisor, view.GraphID, nil)
	d = final.unit(t, "d")
	if d.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("d is %s after its hold was resolved, want completed: %s", d.State, d.Reason)
	}
	if d.RunID == "" {
		t.Fatal("d completed with no child run")
	}
	// FRESH SESSION: d's run was invoked exactly once, ever - there is no
	// prior session for it to have continued, because this is its first and
	// only activation.
	invocations, _ := fixture.worker.counts(d.RunID)
	if invocations != 1 {
		t.Fatalf("d's single, first-ever activation reports %d provider invocations, want exactly 1", invocations)
	}
}

// TestAnEarlyResolutionDoesNotBypassAnUnsatisfiedDependency is #508 acceptance
// 15: resolving a hold the moment it is placed - before the unit's OTHER
// dependencies are anywhere near satisfied - must not let it jump ahead of
// them. The hold is #472's seam; it adds a wait, and lifting it early can only
// remove that one wait, never the graph's own dependency gating.
func TestAnEarlyResolutionDoesNotBypassAnUnsatisfiedDependency(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisorWithRealHolds()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())

	hold, err := supervisor.PlaceWorkUnitHold(ControlRequest{
		GraphID: view.GraphID, UnitID: "d", Operator: decisionOperator(), Note: "sign-off",
	})
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: hold.ID, DecisionOutcomeKind: orchestration.DecisionAllowDeny,
		DecisionOutcomeValue: orchestration.DecisionAllow, Operator: decisionOperator(),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var bAndCCompleteWhenDActivated bool
	var dActivated bool
	final := fixture.driveGraph(supervisor, view.GraphID, func(v WorkGraphView) {
		if dActivated {
			return
		}
		d := v.unit(t, "d")
		if d.RunID == "" {
			return
		}
		dActivated = true
		b, c := v.unit(t, "b"), v.unit(t, "c")
		bAndCCompleteWhenDActivated = b.State == orchestration.UnitState(orchestration.ItemCompleted) && c.State == orchestration.UnitState(orchestration.ItemCompleted)
	})
	if !dActivated {
		t.Fatal("d never activated at all")
	}
	if !bAndCCompleteWhenDActivated {
		t.Fatal("d activated before b and c were both complete: an already-resolved hold bypassed an unsatisfied dependency")
	}
	if final.unit(t, "d").State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("d ended at %s, not completed", final.unit(t, "d").State)
	}
}

// TestSupervisorResolveDecisionEnforcesAuthorityAndIdempotency is #508
// acceptance 5, 8, 9 and the self-resolution/unauthorized-operator laws, at
// the governed action itself rather than at the pure domain function: an
// empty operator (no authority established at all - the shape a worker's own
// report could never even carry, since it has no such member) is refused, an
// unknown request is refused, a normal resolution succeeds, an identical
// retry is idempotent, and a conflicting second answer is refused.
func TestSupervisorResolveDecisionEnforcesAuthorityAndIdempotency(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisorWithRealHolds()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	hold, err := supervisor.PlaceWorkUnitHold(ControlRequest{GraphID: view.GraphID, UnitID: "d", Operator: decisionOperator(), Note: "sign-off"})
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}
	allow := ControlRequest{DecisionID: hold.ID, DecisionOutcomeKind: orchestration.DecisionAllowDeny, DecisionOutcomeValue: orchestration.DecisionAllow}

	if _, err := supervisor.ResolveDecision(allow); err == nil {
		t.Fatal("expected a resolution naming no operator to be refused")
	}
	unknown := allow
	unknown.DecisionID, unknown.Operator = "decision-resolution-does-not-exist", decisionOperator()
	if _, err := supervisor.ResolveDecision(unknown); err == nil {
		t.Fatal("expected resolving an unknown request to be refused")
	}

	authorized := allow
	authorized.Operator = decisionOperator()
	first, err := supervisor.ResolveDecision(authorized)
	if err != nil {
		t.Fatalf("expected an authorized resolution to succeed, got %v", err)
	}

	retry, err := supervisor.ResolveDecision(authorized)
	if err != nil || retry.Resolution != first.Resolution {
		t.Fatalf("expected an identical retry to be idempotent: err=%v first=%+v retry=%+v", err, first.Resolution, retry.Resolution)
	}

	conflicting := authorized
	conflicting.DecisionOutcomeValue = orchestration.DecisionDeny
	if _, err := supervisor.ResolveDecision(conflicting); err == nil {
		t.Fatal("expected a conflicting second answer to be refused")
	}
}
