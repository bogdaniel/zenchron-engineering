package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAdmission is the serving side's gate, standing in for a supervisor. It is
// the real gate type underneath, so the sequencing this test exercises is the
// sequencing production gets.
type fakeAdmission struct{ gate *workAdmissionGate }

func newFakeAdmission() *fakeAdmission { return &fakeAdmission{gate: newWorkAdmissionGate(false)} }

func (f *fakeAdmission) EnableWorkAdmission(admission WorkAdmission) error {
	if !admission.Permitted {
		return &WorkAdmissionRefusedError{Reason: admission.Reason}
	}
	return f.gate.open()
}
func (f *fakeAdmission) AdmittingWork() bool { return f.gate.permitted() }
func (f *fakeAdmission) Drain()              { f.gate.close("drained for handoff") }

// serviceFixture is a successor process: a store holding a transition at
// successor_acquired, two real generation directories, and this process's own
// identity as the successor.
type serviceFixture struct {
	state     string
	root      string
	store     *SQLiteOperationStore
	record    ControllerHandoff
	self      ControllerSelfRecord
	admission *fakeAdmission
	service   *ControllerService
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	inner := newChoreography(t)
	// Walk the predecessor's half. The record is then at ownership_released,
	// which is where a successor takes over: the PHASE that records the
	// successor holding the role is written by the transition itself, not by
	// whoever set the fixture up.
	released, err := BeginHandoff(inner.predecessorPorts(), inner.record)
	if err != nil {
		t.Fatal(err)
	}
	return &serviceFixture{
		state: inner.state, root: inner.root, store: inner.store,
		record: released, self: inner.self, admission: newFakeAdmission(),
	}
}

func (f *serviceFixture) start(t *testing.T) *ControllerService {
	t.Helper()
	service, err := StartControllerService(f.state, f.root, f.store, f.self, f.admission)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.lease.Release() })
	service.now = func() time.Time { return time.Unix(1700000002, 0).UTC() }
	return service
}

func (f *serviceFixture) expectation(t *testing.T) ActivationExpectation {
	t.Helper()
	expect, err := Expect(f.record)
	if err != nil {
		t.Fatal(err)
	}
	return expect
}

// ACQUIRING THE ROLE OPENS NOTHING, and neither does proving identity.
func TestRoleAndIdentityDoNotOpenService(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)

	if service.AdmittingWork() {
		t.Fatal("taking the controller role opened work admission")
	}
	if err := fixture.self.ProvesGeneration(fixture.record.Successor.Binding); err != nil {
		t.Fatal(err)
	}
	if service.AdmittingWork() {
		t.Fatal("proving the generation opened work admission")
	}
	// And service cannot be opened before the activation exists.
	if err := service.EnableWorkAdmission(fixture.record.ID); err == nil {
		t.Fatal("work admission opened before the durable activation")
	}
}

// ACTIVATION IS AN EXACT-TRANSITION COMMIT.
func TestActivationRequiresTheExactTransitionItWasAuthorizedFor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActivationExpectation)
		want   string
	}{
		{"a different transition", func(e *ActivationExpectation) { e.HandoffID = "handoff-somewhere-else" }, "no transition"},
		{"a different phase", func(e *ActivationExpectation) { e.Phase = HandoffSuccessorAcquired }, "has moved"},
		{"a different predecessor", func(e *ActivationExpectation) { e.Predecessor = "0000" }, "has moved"},
		{"a different successor", func(e *ActivationExpectation) { e.Successor = "0000" }, "has moved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			service := fixture.start(t)
			expect := fixture.expectation(t)
			test.mutate(&expect)
			if _, err := service.ActivateSuccessor(expect, nil); err == nil {
				t.Fatal("activation accepted a transition it was not authorized for")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
			stored, _, err := fixture.store.ControllerHandoff(fixture.record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Phase == HandoffActivated {
				t.Fatal("a refused activation still activated the transition")
			}
		})
	}
}

// A role holder of the WRONG generation cannot activate the successor, however
// legitimately it holds the role.
func TestWrongGenerationHoldingTheRoleCannotActivate(t *testing.T) {
	fixture := newServiceFixture(t)
	other := attestedBuild(ControllerAdopted, strangerRevision, "tree-x", strings.Repeat("ef", 32))
	fixture.self = ControllerSelfRecord{Build: other, Measured: other.BinarySHA256}
	service := fixture.start(t)

	if _, err := service.ActivateSuccessor(fixture.expectation(t), nil); err == nil {
		t.Fatal("a controller of another generation activated this transition")
	} else if !strings.Contains(err.Error(), "not the successor") {
		t.Fatalf("error = %v, want one naming the generation mismatch", err)
	}
}

// THE WHOLE SEQUENCE, and the state after each step.
func TestActivationEstablishesTruthAndNotService(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)

	activated, err := service.ActivateSuccessor(fixture.expectation(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if activated.Phase != HandoffActivated {
		t.Fatalf("phase = %q, want activated", activated.Phase)
	}
	// ACTIVATION IS NOT SERVICE. The gate is still shut on the way out.
	if service.AdmittingWork() {
		t.Fatal("activation opened work admission")
	}
	// The projection followed, because it could.
	if _, err := os.Readlink(filepath.Join(fixture.root, StableEntrypointName)); err != nil {
		t.Fatalf("the stable entrypoint was not repaired: %v", err)
	}
	// Service opens only now, and only on the conjunction.
	if err := service.EnableWorkAdmission(fixture.record.ID); err != nil {
		t.Fatal(err)
	}
	if !service.AdmittingWork() {
		t.Fatal("work admission did not open after activation")
	}
	// Repeating the exact transition converges rather than double-activating.
	if _, err := service.ActivateSuccessor(fixture.expectation(t), nil); err == nil {
		t.Fatal("the same transition activated twice from the same expectation")
	}
}

// A CRASH BEFORE ACTIVATION leaves no serving successor and invents nothing.
func TestCrashBeforeActivationLeavesNothingActivated(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)

	// The endpoint proof fails, which is the last step before the commit.
	if _, err := service.ActivateSuccessor(fixture.expectation(t), func() error {
		return errTestEndpointSilent
	}); err == nil {
		t.Fatal("activation survived an endpoint that did not answer")
	}
	stored, _, err := fixture.store.ControllerHandoff(fixture.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase == HandoffActivated {
		t.Fatal("a failed proof still activated the transition")
	}
	if service.AdmittingWork() {
		t.Fatal("a failed activation opened service")
	}
	if err := service.EnableWorkAdmission(fixture.record.ID); err == nil {
		t.Fatal("service opened on a transition that never activated")
	}
}

var errTestEndpointSilent = &ControlEndpointError{Path: "/test", Detail: "did not answer"}

// A CRASH IMMEDIATELY AFTER ACTIVATION leaves durable successor truth with
// nobody serving, and a replacement process of that generation resumes without
// inventing a new transition.
func TestActivatedButNotServingIsResumable(t *testing.T) {
	fixture := newServiceFixture(t)
	dying := fixture.start(t)
	if _, err := dying.ActivateSuccessor(fixture.expectation(t), nil); err != nil {
		t.Fatal(err)
	}
	// The process dies: its role goes, its gate goes, the record stays.
	if err := dying.lease.Release(); err != nil {
		t.Fatal(err)
	}

	stored, _, err := fixture.store.ControllerHandoff(fixture.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != HandoffActivated {
		t.Fatalf("phase = %q: the activation must survive the process", stored.Phase)
	}

	// A replacement of the SAME generation starts, takes the free role, and
	// recovers from durable truth. Nothing is re-decided.
	replacement := &serviceFixture{
		state: fixture.state, root: fixture.root, store: fixture.store,
		record: stored, self: fixture.self, admission: newFakeAdmission(),
	}
	resumed := replacement.start(t)
	if resumed.AdmittingWork() {
		t.Fatal("a replacement started already serving")
	}
	recovered, err := resumed.RecoverActivated(fixture.record.ID)
	if err != nil {
		t.Fatalf("a replacement of the activated generation could not resume: %v", err)
	}
	if recovered.Phase != HandoffActivated {
		t.Fatal("recovery changed the phase")
	}
	if err := resumed.EnableWorkAdmission(fixture.record.ID); err != nil {
		t.Fatal(err)
	}
	if !resumed.AdmittingWork() {
		t.Fatal("the resumed controller is not serving")
	}
}

// Recovery is not a way to activate something that never activated.
func TestRecoveryRefusesATransitionThatNeverActivated(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)
	if _, err := service.RecoverActivated(fixture.record.ID); err == nil {
		t.Fatal("recovery resumed an activation that never happened")
	}
	stored, _, err := fixture.store.ControllerHandoff(fixture.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase == HandoffActivated {
		t.Fatal("recovery activated a transition")
	}
}

// A PROJECTION THAT CANNOT BE REPAIRED does not revoke activation and does not
// by itself prohibit service.
func TestProjectionFailureDoesNotRevokeActivationOrService(t *testing.T) {
	fixture := newServiceFixture(t)
	// A directory at the pointer path is the one occupant activation refuses
	// to clear, so the repair fails for a real reason.
	if err := os.MkdirAll(filepath.Join(fixture.root, StableEntrypointName, "operator-files"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := fixture.start(t)

	activated, err := service.ActivateSuccessor(fixture.expectation(t), nil)
	var repair *ProjectionRepairFailedError
	if err == nil {
		t.Fatal("the projection failure was not reported")
	} else if !asProjectionFailure(err, &repair) {
		t.Fatalf("error = %v, want a typed projection failure", err)
	}
	if activated.Phase != HandoffActivated {
		t.Fatalf("phase = %q: a projection failure must not undo the activation", activated.Phase)
	}
	if err := service.EnableWorkAdmission(fixture.record.ID); err != nil {
		t.Fatalf("a stale stable entrypoint blocked service: %v", err)
	}
	if !service.AdmittingWork() {
		t.Fatal("the active controller is not serving because a symlink is wrong")
	}
}

func asProjectionFailure(err error, target **ProjectionRepairFailedError) bool {
	failure, ok := err.(*ProjectionRepairFailedError)
	if ok {
		*target = failure
	}
	return ok
}

// THE ROLE IS NOT RELEASED WHILE SERVING. Draining happens first, through one
// operation, so the ordering is not something a caller has to remember.
func TestRoleIsReleasedOnlyAfterServiceIsDrained(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)
	if _, err := service.ActivateSuccessor(fixture.expectation(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := service.EnableWorkAdmission(fixture.record.ID); err != nil {
		t.Fatal(err)
	}
	if !service.AdmittingWork() {
		t.Fatal("the controller is not serving, so the case is not the one being tested")
	}

	if err := service.DrainAndReleaseRole(); err != nil {
		t.Fatal(err)
	}
	if service.AdmittingWork() {
		t.Fatal("the controller is still admitting work after releasing the role")
	}
	// The role really went, so a successor can take it.
	next, err := AcquireControllerRole(fixture.state)
	if err != nil {
		t.Fatalf("the role was not released: %v", err)
	}
	t.Cleanup(func() { _ = next.Release() })
	// And this service can no longer do anything privileged with it.
	if err := service.EnableWorkAdmission(fixture.record.ID); err == nil {
		t.Fatal("a drained controller reopened service without the role")
	}
}

// Service requires the live role, not merely a past one.
func TestServiceCannotOpenWithoutTheRole(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.start(t)
	if _, err := service.ActivateSuccessor(fixture.expectation(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := service.lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnableWorkAdmission(fixture.record.ID); err == nil {
		t.Fatal("service opened without the controller role")
	}
	if service.AdmittingWork() {
		t.Fatal("service opened without the controller role")
	}
}

// A FAIL-BACK MUST NOT DEADLOCK THE PROCESS THAT PERFORMS IT.
//
// CompleteHandoff runs inside the lease's authority section, which holds its
// read side; giving ownership back takes the write side. Releasing from inside
// therefore blocked on itself, Go reported "all goroutines are asleep -
// deadlock!", and the process died - after the point of no return, with the
// predecessor already gone and the stable entrypoint still naming it.
//
// It survived every existing test of this path because they supply a FAKE
// ReleaseOwnership that clears a string. This one drives a REAL
// ControllerRoleLease through ControllerService, which is the only arrangement
// where the two sides of that lock meet.
func TestAFailedRevalidationReleasesTheRealRoleWithoutDeadlocking(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	// The durable head moves after the preflight decided about it, which is
	// what revalidation exists to catch - and it catches it AFTER ownership
	// has transferred, which is the case that used to be fatal.
	runs, err := c.store.Runs()
	if err != nil || len(runs) == 0 {
		t.Fatalf("HARNESS PRECONDITION: the fixture has no live run: %v", err)
	}
	before, err := c.store.Events(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.fixture.runtime.Reconcile(context.Background(), runs[0].ID); err != nil {
		t.Fatalf("HARNESS PRECONDITION: %v", err)
	}
	after, err := c.store.Events(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) == len(before) {
		t.Fatal("HARNESS PRECONDITION: the durable head did not move, so revalidation has nothing to refuse")
	}

	lease, err := AcquireControllerRole(c.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	service := BindControllerService(c.state, c.root, c.store, c.self, lease, newFakeAdmission())
	stored := c.stored()
	expect, err := Expect(stored)
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, activateErr := service.ActivateSuccessor(expect, nil)
		done <- outcome{activateErr}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a successor whose revalidation failed reported the transition activated")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the fail-back deadlocked: releasing the role from inside the authority section blocks on itself")
	}

	// AND THE ROLE IS ACTUALLY GONE. The protocol's failure law is that a
	// successor which cannot prove what it was handed gives ownership back, and
	// moving where that happens must not quietly stop it happening.
	if err := lease.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("the failed successor is still holding the controller role")
	}
	if settled := c.stored(); settled.Phase != HandoffFailed {
		t.Fatalf("phase = %q, want the transition settled back to the predecessor", settled.Phase)
	}
}
