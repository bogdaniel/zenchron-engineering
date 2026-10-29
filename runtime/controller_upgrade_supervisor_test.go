package runtime

// THE UPGRADE AGAINST A REAL SUPERVISOR AND A REAL ADMISSION GATE.
//
// The launcher's own tests drive fake ports, which is right for proving the
// order and the point of no return, and structurally incapable of catching what
// this file exists for: a transition suspends and then CLOSES intake, and the
// gate those operations take is the same gate a scheduling pass holds open
// while it works. Nothing about that is visible from a fake Begin.
//
// Both regressions here are about the SAME gate object the supervisor admits
// work through. Neither substitutes it.

import (
	"context"
	"testing"
	"time"
)

// upgradeSupervisor is a supervisor with nothing to schedule, so a pass is
// exactly the controller-maintenance part of one.
func upgradeSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return supervisorFixture(t, newPhase8Fixture(t), 1)
}

// holdOneRunDriver occupies the supervisor's driving group the way a run being
// driven does, and returns the way to let it finish.
//
// It uses the very WaitGroup the dispatcher adds to, because that is the thing
// quiescence waits on. Driving a real run to a provider would take longer and
// prove less: the property is about the wait, not about what the work does.
func holdOneRunDriver(supervisor *Supervisor) (release func()) {
	finished := make(chan struct{})
	supervisor.driving.Add(1)
	go func() {
		<-finished
		supervisor.driving.Done()
	}()
	return func() { close(finished) }
}

// A LAUNCH RUNS OUTSIDE THE PASS'S INTAKE SECTION.
//
// pass() holds the admission gate open for reading across discovery and plan
// reconciliation. A transition takes the same gate exclusively - to suspend
// intake, and again to close it. Performing one inside the other is a
// self-deadlock: the pass cannot release the read side until it returns, and it
// cannot return until the write side it is waiting for is granted. It would
// have appeared on the first real upgrade of a live controller.
func TestAnUpgradeDoesNotDeadlockAgainstTheIntakeSection(t *testing.T) {
	supervisor := upgradeSupervisor(t)
	drained := make(chan struct{})
	upgrade := NewControllerUpgrade(alwaysReadyUpdater(t), func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch {
		// EXACTLY WHAT BeginSuccession DOES, against the real gate: suspend
		// intake, then close it.
		resume, err := supervisor.QuiesceWorkForTransition(context.Background(), time.Minute)
		if err != nil {
			return SuccessionLaunch{Quiesce: stepRefused("%v", err)}
		}
		defer resume()
		supervisor.Drain()
		close(drained)
		return SuccessionLaunch{Committed: true, Served: true}
	})
	if err := supervisor.BindControllerUpgrade(upgrade); err != nil {
		t.Fatal(err)
	}

	passed := make(chan SupervisorReport, 1)
	go func() {
		report, err := supervisor.pass(context.Background())
		if err != nil {
			t.Errorf("the pass failed: %v", err)
		}
		passed <- report
	}()

	select {
	case report := <-passed:
		if report.Upgrade == nil || !report.Upgrade.Superseded() {
			t.Fatalf("the pass did not report the controller superseded: %+v", report.Upgrade)
		}
		// AND IT STOPPED THERE. A controller that gave up the role must not go
		// on to enumerate and dispatch runs in the same pass.
		if !report.Draining {
			t.Fatal("a superseded pass did not report itself draining")
		}
		// Active is the count a pass fills in when it ENUMERATES runs, which a
		// superseded pass returns before reaching.
		if report.Active != 0 || report.Plans != nil || report.Discovery != nil {
			t.Fatalf("a superseded pass went on with the rest of the pass: active=%d plans=%v discovery=%v",
				report.Active, report.Plans, report.Discovery)
		}
	case <-time.After(20 * time.Second):
		select {
		case <-drained:
			t.Fatal("the pass never returned after the transition closed intake")
		default:
			t.Fatal("the transition deadlocked against the pass's intake section")
		}
	}
}

// NOTHING IS GIVEN UP WHILE THIS CONTROLLER'S OWN WORK CAN STILL APPEND.
//
// Closing intake stops runs being CREATED and says nothing about the goroutines
// already driving one. A predecessor that released the role with those running
// would have its successor activate and serve while it was still writing to the
// same store.
func TestTheRoleIsNotGivenUpWhilePredecessorWorkIsStillRunning(t *testing.T) {
	supervisor := upgradeSupervisor(t)
	release := holdOneRunDriver(supervisor)

	quiesced := make(chan struct{})
	go func() {
		resume, err := supervisor.QuiesceWorkForTransition(context.Background(), 30*time.Second)
		if err != nil {
			t.Errorf("quiescence failed: %v", err)
			return
		}
		defer resume()
		close(quiesced)
	}()

	// INTAKE IS ALREADY SUSPENDED, so nothing new can be created while the
	// existing driver finishes - but the transition has NOT proceeded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if supervisor.AdmittingWork() {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		break
	}
	if supervisor.AdmittingWork() {
		t.Fatal("intake was never suspended")
	}
	select {
	case <-quiesced:
		t.Fatal("the transition treated a controller with a live run driver as quiescent")
	case <-time.After(250 * time.Millisecond):
	}

	release()
	select {
	case <-quiesced:
	case <-time.After(10 * time.Second):
		t.Fatal("the transition did not proceed after the run driver finished")
	}
}

// A SUSPENSION THAT DOES NOT SETTLE GIVES INTAKE BACK. The update is lost, the
// controller is not.
func TestAnUnsettledFleetCostsTheUpdateAndNotTheController(t *testing.T) {
	supervisor := upgradeSupervisor(t)
	release := holdOneRunDriver(supervisor)
	defer release()

	if _, err := supervisor.QuiesceWorkForTransition(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("a fleet that never settled was reported quiescent")
	}
	if !supervisor.AdmittingWork() {
		t.Fatal("a transition that was abandoned left this controller unable to admit work")
	}
}

// AND A SUSPENDED GATE IS NOT OPENED FROM OUTSIDE THE TRANSITION.
//
// The reconciler asks for exactly this when it sees a controller that is
// durably active and not admitting work, which is what a held gate looks like
// from outside. Honouring it would put the predecessor back to admitting work
// in the window its successor is evaluating.
func TestAHeldGateIsNotReopenedByTheReconciler(t *testing.T) {
	supervisor := upgradeSupervisor(t)
	resume, err := supervisor.QuiesceWorkForTransition(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = supervisor.EnableWorkAdmission(WorkAdmission{Permitted: true})
	if err == nil {
		t.Fatal("a held gate was opened by something outside the transition")
	}
	if supervisor.AdmittingWork() {
		t.Fatal("a held gate is admitting work")
	}
	resume()
	if !supervisor.AdmittingWork() {
		t.Fatal("the transition's own release did not put intake back")
	}
}

// alwaysReadyUpdater is an updater whose subject is always a moved trusted
// main, so a pass reaches the launch.
func alwaysReadyUpdater(t *testing.T) *ControllerUpdater {
	t.Helper()
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)}}
	updater := newUpdater(t, harness)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if update := updater.Attempt(context.Background(), time.Now().UTC()); update.State == UpdateReady {
			return updater
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("HARNESS PRECONDITION: the updater never reached a ready successor")
	return nil
}
