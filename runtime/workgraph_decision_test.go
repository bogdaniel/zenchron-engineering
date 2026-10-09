package runtime

// Durable, authorized DecisionResolution (#508), proved end to end against the
// REAL #472 WorkGraph fleet fixture: a real hold, placed and resolved through
// the Supervisor's own governed actions and read back through the REAL
// store-backed SupervisorDependencies.WorkUnitHolds - never the fixture's
// fake hold map every other #472 test uses, because what is under test here
// is exactly the wiring those tests deliberately leave unexercised.

import (
	"context"
	"strings"
	"sync"
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
//
// SCOPE: this proves the one thing reachable from inside this process - that
// Supervisor.ResolveDecision itself never accepts an unauthenticated caller,
// and never builds an authority of any kind but "operator" for one it does
// accept. It does NOT exercise a rejected non-owner CALLER: that proof is the
// OS file-permission boundary of the control endpoint itself
// (runtime/control_endpoint_ownership_test.go), which this method is reached
// through and does not re-implement.
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

// TestPlaceWorkUnitHoldNeverRacesAGraphRevision is #508 review P3: a
// WorkGraph revision that removes a not-yet-activated unit (ValidateMutation
// permits this - nothing has consumed it yet) racing a hold placement on
// that SAME unit must decide the hold against ONE consistent revision, never
// a membership check read before the shared lock and an insert committed
// after a mutation moved underneath it. Both PlaceWorkUnitHold and
// AdoptWorkGraph's mutation hold s.orchestrationMu for their whole check, so
// exactly one of two outcomes is possible, and each is checked on its own
// terms - never against a state read AFTER both goroutines have finished,
// which cannot distinguish "the hold was invalid when checked" from "a LATER
// mutation moved on after a perfectly valid hold already committed":
//
//   - the hold is refused, naming no such unit - the mutation's critical
//     section ran first, and the hold's own check correctly saw "b" already
//     gone;
//   - the hold succeeds and is durably stored - its check ran first, while
//     "b" still named a unit of the current revision, and that revision -
//     revision 1, immutable once adopted - still proves it today.
func TestPlaceWorkUnitHoldNeverRacesAGraphRevision(t *testing.T) {
	for i := 0; i < 20; i++ {
		fixture := newFleetFixture(t, 4)
		supervisor := fixture.supervisorWithRealHolds()
		view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
			{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: fleetFirstIssue},
			{ID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: fleetFirstIssue + 1},
		})

		var wg sync.WaitGroup
		start := make(chan struct{})
		var holdErr, mutateErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, holdErr = supervisor.PlaceWorkUnitHold(ControlRequest{
				GraphID: view.GraphID, UnitID: "b", Operator: decisionOperator(), Note: "racing a graph mutation",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			// Revision 2 keeps only "a": a legitimate mutation, since
			// nothing has activated "b" yet.
			_, err := supervisor.AdoptWorkGraph(context.Background(), ControlRequest{
				Repository: "acme/repo", Agent: "claude", Operator: "operator@example",
				WorkGraph: &orchestration.WorkGraphProposal{Name: "m2-o1", Revision: 2, Units: []orchestration.WorkUnit{
					{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: fleetFirstIssue},
				}},
			})
			mutateErr = err
		}()
		close(start)
		wg.Wait()
		if mutateErr != nil {
			t.Fatalf("iteration %d: revision 2 was refused: %v", i, mutateErr)
		}

		if holdErr == nil {
			// The hold's OWN check ran first and saw "b": revision 1, never
			// rewritten, still proves that was true.
			revision1, found, err := fixture.store.WorkGraphRevision(view.GraphID, 1)
			if err != nil || !found {
				t.Fatalf("iteration %d: revision 1 unreadable: found=%t err=%v", i, found, err)
			}
			named := false
			for _, unit := range revision1.Units {
				named = named || unit.ID == "b"
			}
			if !named {
				t.Fatalf("iteration %d: a hold succeeded for unit b, which not even revision 1 named", i)
			}
			if stored, found, err := fixture.store.WorkUnitHoldByID(mustWorkUnitHoldID(t, view.GraphID, "b")); err != nil || !found || stored.UnitID != "b" {
				t.Fatalf("iteration %d: PlaceWorkUnitHold reported success but no durable hold exists: found=%t err=%v stored=%+v", i, found, err, stored)
			}
			continue
		}
		// The mutation's critical section ran first: the hold's own check
		// correctly saw a revision that no longer names "b", and refused
		// before writing anything.
		if !strings.Contains(holdErr.Error(), "names no unit") {
			t.Fatalf("iteration %d: hold refused for an unexpected reason: %v", i, holdErr)
		}
		if _, found, err := fixture.store.WorkUnitHoldByID(mustWorkUnitHoldID(t, view.GraphID, "b")); err != nil || found {
			t.Fatalf("iteration %d: a refused hold left a durable row anyway: found=%t err=%v", i, found, err)
		}
	}
}

func mustWorkUnitHoldID(t *testing.T, graphID, unitID string) string {
	t.Helper()
	id, err := orchestration.WorkUnitHoldID(graphID, unitID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPlaceWorkUnitHoldNeverRacesActivation is #508 review B3: placing a hold
// and activating the frontier must be ONE atomic decision, not two reads that
// can interleave. supervisor.PlaceWorkUnitHold and activateGraphFrontier now
// share s.orchestrationMu, so exactly one of two things is true after a Tick
// and a PlaceWorkUnitHold call race on the SAME ready unit: the hold won,
// and the unit is NEVER activated afterwards either; or activation won, and
// the hold is refused with a clear reason - never both succeeding, which
// would mean a hold attached to a unit that is already running, or an
// already-held unit got activated anyway.
//
// Run with -race to also catch any unsynchronized access the lock was
// supposed to prevent.
func TestPlaceWorkUnitHoldNeverRacesActivation(t *testing.T) {
	for i := 0; i < 20; i++ {
		fixture := newFleetFixture(t, 4)
		supervisor := fixture.supervisorWithRealHolds()
		view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
			{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: fleetFirstIssue},
		})

		var wg sync.WaitGroup
		start := make(chan struct{})
		var holdErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, holdErr = supervisor.PlaceWorkUnitHold(ControlRequest{
				GraphID: view.GraphID, UnitID: "a", Operator: decisionOperator(), Note: "racing the activation pass",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = supervisor.Tick(context.Background())
		}()
		close(start)
		wg.Wait()

		activations, err := fixture.store.WorkUnitActivations(view.GraphID)
		if err != nil {
			t.Fatal(err)
		}
		_, activated := activations["a"]
		holdPlaced := holdErr == nil

		if holdPlaced && activated {
			t.Fatalf("iteration %d: BOTH the hold and activation succeeded - a hold attached to an already-activated unit", i)
		}
		if holdPlaced {
			// The hold won: it must bar activation on every LATER pass too,
			// not just the racing one.
			if _, err := supervisor.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			fixture.clock.advance(61 * time.Second)
			if activations, err = fixture.store.WorkUnitActivations(view.GraphID); err != nil {
				t.Fatal(err)
			}
			if _, activated := activations["a"]; activated {
				t.Fatalf("iteration %d: a successfully placed hold did not bar a later activation", i)
			}
			continue
		}
		// Activation won: the hold must be refused for the right reason, not
		// silently dropped or accepted as if it had been placed in time.
		if holdErr == nil || !strings.Contains(holdErr.Error(), "already activated") {
			t.Fatalf("iteration %d: activation won the race but the hold was not refused as already-activated: %v", i, holdErr)
		}
	}
}
