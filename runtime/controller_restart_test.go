package runtime

// A CRASH BETWEEN A PREPARED TRANSITION AND ITS ACTIVATION MUST CONVERGE.
//
// The two cut points are the ones a predecessor can restart from: it persisted
// the transition and crashed while draining, and it released ownership and
// crashed before the successor took over. Both leave a durable record in flight
// naming the predecessor as the controller permitted to recover.
//
// What is proven here is the whole convergence, not the settle: the record is
// closed, the controller serves, AND THE NEXT ATTEMPT AT THE SAME TRANSITION
// SUCCEEDS. Settling alone would look correct and leave automatic upgrades
// refusing forever, which is the defect (#288) rather than its fix.

import (
	"testing"
	"time"
)

func restartAt(t *testing.T, phase HandoffPhase) *choreography {
	t.Helper()
	c := newChoreography(t)
	switch phase {
	case HandoffDraining:
		// Persisted and drained; the crash lands before ownership is released.
		c.crashAt = "release"
		if _, err := BeginHandoff(c.predecessorPorts(), c.record); err == nil {
			t.Fatal("HARNESS PRECONDITION: the interrupted handoff reported success")
		}
	case HandoffOwnershipReleased:
		// The predecessor's half completed; the successor never took over.
		if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
			t.Fatalf("HARNESS PRECONDITION: %v", err)
		}
	default:
		t.Fatalf("HARNESS PRECONDITION: %q is not a restart cut point", phase)
	}
	if stored := c.stored(); stored.Phase != phase {
		t.Fatalf("HARNESS PRECONDITION: the transition is at %q and the cut point is %q", stored.Phase, phase)
	}
	return c
}

// THE PREDECESSOR RESTARTS AND THE TRANSITION CONVERGES.
func TestAPredecessorRestartSettlesAnInterruptedTransition(t *testing.T) {
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased} {
		t.Run(string(phase), func(t *testing.T) {
			c := restartAt(t, phase)
			predecessor := c.digest(c.record.Predecessor)

			resolved, err := ResolveHandoffAtStartup(c.store, c.predecesor, predecessor, time.Unix(1700000100, 0).UTC())
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.Settled {
				t.Fatalf("the interrupted transition was not settled: %+v", resolved.Resolution)
			}
			settled := c.stored()
			if settled.Phase != HandoffFailed {
				t.Fatalf("phase = %q, want the transition closed", settled.Phase)
			}
			// AND THE PREDECESSOR IS THE ONE PERMITTED TO CARRY ON. Nothing
			// chose that here; the record said it before the crash.
			if !settled.MayRecover(predecessor) {
				t.Fatal("the settled transition does not permit the predecessor to resume")
			}
			if admission := WorkAdmissionFor(&settled, c.predecesor, predecessor); !admission.Permitted {
				t.Fatalf("the restarted predecessor may not serve: %s", admission.Reason)
			}
		})
	}
}

// AND THE NEXT AUTOMATIC UPGRADE PROCEEDS. This is the half that makes the
// settle worth anything: the transition id is deterministic in the two
// controllers, so a settled record that could not be re-addressed would refuse
// every later attempt between the same generations for the rest of its life.
func TestASettledTransitionIsRetriedRatherThanRefusedForever(t *testing.T) {
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased} {
		t.Run(string(phase), func(t *testing.T) {
			c := restartAt(t, phase)
			predecessor := c.digest(c.record.Predecessor)
			if _, err := ResolveHandoffAtStartup(c.store, c.predecesor, predecessor, time.Unix(1700000100, 0).UTC()); err != nil {
				t.Fatal(err)
			}

			// The same two controllers, the same transition, a later attempt -
			// on a process that is not crashing this time.
			c.owner, c.drained, c.crashAt = "predecessor", false, ""
			retried, err := BeginHandoff(c.predecessorPorts(), c.record)
			if err != nil {
				t.Fatalf("a later attempt at the settled transition was refused: %v", err)
			}
			if retried.Phase != HandoffOwnershipReleased {
				t.Fatalf("phase = %q, want the predecessor's half completed", retried.Phase)
			}
			// AND IT COMPLETES. The retry is a whole transition, not a record.
			activated, err := CompleteHandoff(c.successorPorts(), c.record.ID)
			if err != nil {
				t.Fatalf("the retried transition did not complete: %v", err)
			}
			if activated.Phase != HandoffActivated {
				t.Fatalf("phase = %q, want activated", activated.Phase)
			}
		})
	}
}

// A TRANSITION IN A LIVE PHASE IS STILL REFUSED. The retry is conditional on
// the record being settled; a transition that is actually happening is not a
// record to overwrite.
func TestALiveTransitionIsNotOverwrittenByANewAttempt(t *testing.T) {
	c := restartAt(t, HandoffOwnershipReleased)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err == nil {
		t.Fatal("a transition in flight was overwritten by a new attempt at it")
	}
}

// A RECORD THIS PROCESS DOES NOT OWN IS NOT SETTLED BY IT.
//
// Before acquisition the recovery owner is the predecessor - the record says
// so, and the successor has taken nothing - so a successor generation starting
// on this state directory is refused rather than invited to clean up. It is
// worth pinning because the successor is the OTHER party to this exact
// transition, which is the closest anything gets to being entitled to settle
// it, and it still is not.
func TestAStartingControllerDoesNotSettleAnotherGenerationsTransition(t *testing.T) {
	c := restartAt(t, HandoffOwnershipReleased)
	successor := c.digest(c.record.Successor)

	resolved, err := ResolveHandoffAtStartup(c.store, c.self, successor, time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Settled {
		t.Fatal("the successor settled a transition whose recovery owner is the predecessor")
	}
	if resolved.Resolution.Action != HandoffActionRefuse {
		t.Fatalf("action = %q, want a refusal", resolved.Resolution.Action)
	}
	if stored := c.stored(); stored.Phase != HandoffOwnershipReleased {
		t.Fatalf("the record moved to %q", stored.Phase)
	}
}

// AN ACTIVATED TRANSITION IS NOT AN INTERRUPTED ONE. Authority is established;
// there is nothing to settle and nothing here reconsiders it.
func TestAnActivatedTransitionIsNotSettledAtStartup(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteHandoff(c.successorPorts(), c.record.ID); err != nil {
		t.Fatal(err)
	}
	successor := c.digest(c.record.Successor)

	resolved, err := ResolveHandoffAtStartup(c.store, c.self, successor, time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Settled {
		t.Fatal("an activated transition was settled")
	}
	if resolved.Resolution.Action != HandoffActionRepairProjection {
		t.Fatalf("action = %q, want the projection repair that changes no authority", resolved.Resolution.Action)
	}
	if stored := c.stored(); stored.Phase != HandoffActivated {
		t.Fatalf("the activated record moved to %q", stored.Phase)
	}
}
