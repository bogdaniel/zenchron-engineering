package runtime

import (
	"context"
	"testing"
)

// These tests fail a REAL store write at each point a driver can be cut off
// after the scheduler granted it a lease (#485). The fault is a SQLite trigger:
// nothing in production code knows a test is injecting it, and the error the
// driver sees is the one any failed write would give it.

// liveDriverFixture is the governed-run fixture with the controller process
// ALIVE - as it is under `serve` - and the process's record of the leases its
// drivers hold. The base fixture reports every owner dead, under which the
// dead-owner rule would recover the lease and hide the defect.
func liveDriverFixture(t *testing.T) *phase8Fixture {
	t.Helper()
	fixture := newPhase8Fixture(t)
	fixture.deps.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner == "owner-1" })
	fixture.deps.DriverLeases = NewDriverLeases()
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture
}

func injectFault(t *testing.T, store *SQLiteOperationStore, name, ddl string) func() {
	t.Helper()
	if _, err := store.db.Exec(`CREATE TRIGGER ` + name + ` ` + ddl); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := store.db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	// The scheduler's start write: Leased -> Running, after the lease.
	faultStart = `BEFORE UPDATE ON run_operations
		WHEN json_extract(OLD.document, '$.state') = 'leased' AND json_extract(NEW.document, '$.state') = 'running'
		BEGIN SELECT RAISE(ABORT, 'injected store fault'); END`
	// The journal write that follows StartWithin and precedes any effect.
	faultBefore = `BEFORE INSERT ON events WHEN NEW.type = 'operation.before'
		BEGIN SELECT RAISE(ABORT, 'injected store fault'); END`
	// The write that would end the driver's lease. With this in place the
	// driver CANNOT clean up after itself, which is the case only a later
	// pass can recover.
	faultRelinquish = `BEFORE UPDATE ON run_operations
		WHEN json_extract(NEW.document, '$.lease') IS NULL AND json_extract(NEW.document, '$.state') IN ('leased', 'running')
		BEGIN SELECT RAISE(ABORT, 'injected store fault'); END`
	// The scheduler's finish of a provider execution, after operation.after
	// is journalled.
	faultExecutionFinish = `BEFORE UPDATE ON run_operations
		WHEN json_extract(NEW.document, '$.kind') = 'execution.invoke' AND json_extract(NEW.document, '$.state') = 'succeeded'
		BEGIN SELECT RAISE(ABORT, 'injected store fault'); END`
)

// leased returns every operation of a run still carrying a durable lease.
func leasedOperations(t *testing.T, fixture *phase8Fixture, runID string) []RunOperation {
	t.Helper()
	operations, err := fixture.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []RunOperation
	for _, op := range operations {
		if op.Lease != nil {
			out = append(out, op)
		}
	}
	return out
}

// failPass runs one Reconcile that must fail on the injected fault.
func failPass(t *testing.T, fixture *phase8Fixture, runID string) {
	t.Helper()
	if _, err := fixture.runtime.Reconcile(context.Background(), runID); err == nil {
		t.Fatal("the injected store fault did not fail the pass")
	}
}

// TestAFailedStartReturnsItsLeaseAtOnce: the start write fails right after the
// lease is taken. The driver returns its lease on the way out, so capacity is
// free immediately and nothing about the attempt is invented.
func TestAFailedStartReturnsItsLeaseAtOnce(t *testing.T) {
	fixture := liveDriverFixture(t)
	runID := fixture.start()
	clear := injectFault(t, fixture.store, "fault_start", faultStart)
	failPass(t, fixture, runID)
	if held := leasedOperations(t, fixture, runID); len(held) != 0 {
		t.Fatalf("the failed driver left %d lease(s) behind: %+v", len(held), held[0])
	}
	operations, err := fixture.store.Operations(runID)
	if err != nil || len(operations) != 1 {
		t.Fatalf("operations = %d (%v)", len(operations), err)
	}
	if op := operations[0]; op.State != Leased || op.Attempt != 0 || op.AttemptIdentity != 0 {
		t.Fatalf("the failed start invented an attempt or a state: %+v", op)
	}
	clear()
	if outcome := fixture.reconcile(runID); outcome.Disposition == Failed {
		t.Fatalf("the run did not recover: %+v", outcome)
	}
	if len(fixture.provider.requests) != 1 {
		t.Fatalf("provider invoked %d times, want once", len(fixture.provider.requests))
	}
}

// TestAStrandedLeaseIsRecoveredByTheSameLiveProcess: the journal write after
// StartWithin fails AND the driver's own cleanup write fails, so the lease is
// left on the row with a live owner. The next pass of the SAME runtime - no
// restart - recovers it, runs the work exactly once, and the stranded
// attempt's accounting is left as the journal and scheduler recorded it.
func TestAStrandedLeaseIsRecoveredByTheSameLiveProcess(t *testing.T) {
	fixture := liveDriverFixture(t)
	runID := fixture.start()
	clearBefore := injectFault(t, fixture.store, "fault_before", faultBefore)
	clearRelinquish := injectFault(t, fixture.store, "fault_relinquish", faultRelinquish)
	failPass(t, fixture, runID)
	held := leasedOperations(t, fixture, runID)
	if len(held) != 1 || held[0].Lease.Owner != "owner-1" || held[0].State != Running {
		t.Fatalf("the fault did not strand a live lease as intended: %+v", held)
	}
	stranded := held[0]
	if stranded.Attempt != 1 || stranded.ActiveSince == nil || len(fixture.provider.requests) != 0 {
		t.Fatalf("stranded attempt = %+v, provider calls %d", stranded, len(fixture.provider.requests))
	}
	clearBefore()
	clearRelinquish()
	outcome := fixture.reconcile(runID)
	if outcome.Reason == "operation_unavailable" || outcome.Disposition == Failed {
		t.Fatalf("the same live process could not recover its own stranded lease: %+v", outcome)
	}
	if held := leasedOperations(t, fixture, runID); len(held) != 0 {
		t.Fatalf("leases still held after the pass returned: %+v", held)
	}
	if len(fixture.provider.requests) != 1 {
		t.Fatalf("provider invoked %d times, want once", len(fixture.provider.requests))
	}
	recovered, _, _, err := fixture.store.Operation(stranded.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The stranded try is still counted - it started - and the recovered try
	// is the next physical identity, never a reuse of the stranded one.
	if recovered.AttemptIdentity <= stranded.AttemptIdentity || recovered.Attempt < stranded.Attempt {
		t.Fatalf("attempt accounting went backwards: stranded %d/%d, recovered %d/%d",
			stranded.Attempt, stranded.AttemptIdentity, recovered.Attempt, recovered.AttemptIdentity)
	}
}

// TestAStrandedLeaseGivesItsCapacityBack: at a ceiling of one, a lease this
// process stranded on one run must not keep a different run from working.
func TestAStrandedLeaseGivesItsCapacityBack(t *testing.T) {
	fixture := liveDriverFixture(t)
	first := fixture.start()
	fixture.issue = phase8Issue + 1
	fixture.forge.Issues[fixture.issue] = GitHubIssue{
		Number: fixture.issue, URL: "https://github.com/acme/repo/issues/42",
		Title: "second", Body: "second body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	second := fixture.start()
	clearBefore := injectFault(t, fixture.store, "fault_before", faultBefore)
	clearRelinquish := injectFault(t, fixture.store, "fault_relinquish", faultRelinquish)
	failPass(t, fixture, first)
	if len(leasedOperations(t, fixture, first)) != 1 {
		t.Fatal("the fault did not strand the first run's lease")
	}
	clearBefore()
	clearRelinquish()
	outcome := fixture.reconcile(second)
	if outcome.Reason == "operation_unavailable" {
		t.Fatalf("the stranded lease kept the only slot from another run: %+v", outcome)
	}
	if len(leasedOperations(t, fixture, first)) != 0 {
		t.Fatal("the stranded lease still occupies the slot")
	}
}

// TestAJournalledEffectIsNotRepeatedAfterAFailedFinish: the provider ran and
// operation.after is journalled, then the scheduler's finish write fails. The
// lease is returned without inventing a state, and the next pass finishes the
// row FROM THE JOURNAL (reconcileStoreLag) instead of running the provider
// again.
func TestAJournalledEffectIsNotRepeatedAfterAFailedFinish(t *testing.T) {
	fixture := liveDriverFixture(t)
	runID := fixture.start()
	clear := injectFault(t, fixture.store, "fault_finish", faultExecutionFinish)
	failPass(t, fixture, runID)
	if len(fixture.provider.requests) != 1 {
		t.Fatalf("provider invoked %d times before the finish failed", len(fixture.provider.requests))
	}
	if held := leasedOperations(t, fixture, runID); len(held) != 0 {
		t.Fatalf("the failed finish left a lease behind: %+v", held)
	}
	state := fixture.state(runID)
	var execution RunOperation
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke {
			execution = op
		}
	}
	if execution.State != Succeeded {
		t.Fatalf("the journal does not record the execution as succeeded: %s", execution.State)
	}
	stored, _, _, err := fixture.store.Operation(execution.ID)
	if err != nil || stored.State != Running {
		t.Fatalf("the scheduler row = %s (%v); releasing capacity must not have invented a state", stored.State, err)
	}
	clear()
	fixture.reconcile(runID)
	if len(fixture.provider.requests) != 1 {
		t.Fatalf("provider invoked %d times: the journalled effect was repeated", len(fixture.provider.requests))
	}
	if finished, _, _, err := fixture.store.Operation(execution.ID); err != nil || finished.State != Succeeded {
		t.Fatalf("the row was not finished from the journal: %s (%v)", finished.State, err)
	}
}

// TestTenContendedDriversNeverStrandALease is the #485 stress reproduction:
// ten drivers of one live process writing at once. Under -race the store fails
// some drivers' writes mid-pass. Each such failure may cost that pass - and,
// truthfully, an attempt it had started - but never a lease: every item still
// settles in this process, nothing is left waiting on operation_unavailable,
// and no durable lease outlives the drivers that took them.
func TestTenContendedDriversNeverStrandALease(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(10))
	settled := fixture.drive(supervisor, view.BatchID)
	for _, item := range settled.Items {
		if item.Reason == "operation_unavailable" {
			t.Fatalf("issue %d is stuck behind a lease: %+v", item.Issue, item)
		}
		if held := leasedOperations(t, fixture.phase8Fixture, item.RunID); len(held) != 0 {
			t.Fatalf("issue %d still carries a lease after every driver returned: %+v", item.Issue, held[0])
		}
	}
}
