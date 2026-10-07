package runtime

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func nestedParent(t *testing.T, store *SQLiteOperationStore, s Scheduler, runID string) RunOperation {
	t.Helper()
	if err := store.PutRun(newJournalRun(runID)); err != nil {
		t.Fatal(err)
	}
	planKind(t, s, runID, OpExecutionInvoke)
	leased := mustNext(t, s, runID)
	if leased == nil {
		t.Fatal("parent work capacity unavailable")
	}
	parent, err := s.StartWithin(leased.ID, &AttemptLimit{Within: 10 * time.Minute, Bound: BoundAttemptWall})
	if err != nil {
		t.Fatal(err)
	}
	return parent
}

func TestNestedVerificationRestartKeepsHeldCapacityAndExactAttempt(t *testing.T) {
	dir, store := openJournal(t)
	clock := &fakeClock{now: time.Now().UTC()}
	s := verificationScheduler(store, "controller", 10, 1)
	s.Clock = clock
	parent := nestedParent(t, store, s, "producer")
	ref := ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}
	p, err := s.RequestVerification(ref, "physical-tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(p); !ok || err != nil {
		t.Fatalf("grant: %v / %v", ok, err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	if _, err := s.RequestVerification(ref, "physical-tool", "tool-owner"); err == nil {
		t.Fatal("restart duplicated a physical tool")
	}
	if ok, err := s.AcquireVerification(p); err == nil || ok {
		t.Fatal("restart re-executed a granted tool")
	}
	clock.now = clock.now.Add(2 * s.LeaseDuration)
	if err := s.reclaimVerificationPermits(); err != nil {
		t.Fatal(err)
	}
	if saturated, err := s.VerificationSaturated("other"); !saturated || err != nil {
		t.Fatal("expiry released a live or unverified owner")
	}
	s.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner != "tool-owner" })
	if err := s.reclaimVerificationPermits(); err != nil {
		t.Fatal(err)
	}
	if saturated, err := s.VerificationSaturated("other"); saturated || err != nil {
		t.Fatal("proven death plus expiry did not release capacity")
	}
	stale := ref
	stale.Attempt++
	if _, err := s.RequestVerification(stale, "new-tool", "tool-owner"); err == nil {
		t.Fatal("accepted wrong physical parent attempt")
	}
}

func TestNestedVerificationAndAssuranceShareOneCeiling(t *testing.T) {
	_, store := openJournal(t)
	s := verificationScheduler(store, "controller", 10, 2)
	parent := nestedParent(t, store, s, "producer")
	for _, id := range []string{"assurance", "next-assurance"} {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	planKind(t, s, "assurance", OpAssuranceGo)
	if mustNext(t, s, "assurance") == nil {
		t.Fatal("assurance slot was not granted")
	}
	p, err := s.RequestVerification(ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, "tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(p); !ok || err != nil {
		t.Fatalf("nested grant: %v/%v", ok, err)
	}
	other := nestedParent(t, store, s, "other-producer")
	q, err := s.RequestVerification(ExecutionAttemptRef{RunID: other.RunID, OperationID: other.ID, Attempt: other.AttemptIdentity}, "tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(q); ok || err != nil {
		t.Fatalf("third verifier admitted: %v/%v", ok, err)
	}
	planKind(t, s, "next-assurance", OpAssuranceGo)
	if mustNext(t, s, "next-assurance") != nil {
		t.Fatal("assurance ignored nested occupancy")
	}
	fleet, err := FleetStatus(store, t.TempDir(), 10, 2, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Working != 3 || fleet.Verifying != 2 || fleet.AwaitingVerification != 2 {
		t.Fatal(fmt.Sprintf("untruthful fleet: working=%d verifying=%d awaiting=%d", fleet.Working, fleet.Verifying, fleet.AwaitingVerification))
	}
}

func TestNestedVerificationRestartCannotResumeWorkBeforeToolCleanup(t *testing.T) {
	dir, store := openJournal(t)
	s := verificationScheduler(store, "old-controller", 10, 2)
	parent := nestedParent(t, store, s, "producer")
	p, err := s.RequestVerification(ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, "physical-tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(p); !ok || err != nil {
		t.Fatal("tool never acquired capacity")
	}
	if _, err := s.Finish(parent.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	next := planKind(t, s, parent.RunID, OpContractCompile)
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	s.Owner = "new-controller"
	if leased := mustNext(t, s, parent.RunID); leased != nil {
		t.Fatal("restart leased work while its old tool was still running")
	}
	if waiting, err := s.VerificationCleanupPending(parent.RunID); !waiting || err != nil {
		t.Fatal("cleanup wait is not typed and durable")
	}
	// The atomic store must refuse even a caller whose view predates the tool.
	stale := next
	now := s.Clock.Now()
	stale.State = Leased
	stale.Lease = &Lease{Owner: s.Owner, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute)}
	_, acquired, err := reopened.AcquireOperation(stale, 1, 10, 2, 2)
	if err != nil || acquired {
		t.Fatalf("stale acquisition bypassed tool cleanup: %v/%v", acquired, err)
	}
	fleet, err := FleetStatus(reopened, t.TempDir(), 10, 2, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Waiting != 2 || fleet.Verifying != 1 || fleet.Runnable != 0 {
		t.Fatalf("cleanup was reported runnable: %+v", fleet)
	}
	s.Clock = &fakeClock{now: now.Add(2 * time.Minute)}
	s.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner != "tool-owner" })
	if leased := mustNext(t, s, parent.RunID); leased == nil || leased.ID != next.ID {
		t.Fatal("proven tool cleanup did not restore work eligibility")
	}
}

func TestNestedVerificationUncertainCleanupIsLocal(t *testing.T) {
	dir, store := openJournal(t)
	s := verificationScheduler(store, "controller", 10, 1)
	parent := nestedParent(t, store, s, "affected")
	binding := &VerificationSandbox{StateDir: t.TempDir(), Image: "verifier"}
	p, err := s.requestVerification(ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, "docker", "dead-tool", binding, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(p); !ok || err != nil {
		t.Fatalf("grant: %v/%v", ok, err)
	}
	sandbox := binding.sandbox(p.ID)
	record, err := sandbox.operationRecordPath()
	if err != nil {
		t.Fatal(err)
	}
	// An unreadable record cannot prove that no container remains.
	if err := os.MkdirAll(record, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(parent.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	next := planKind(t, s, parent.RunID, OpContractCompile)
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	store, s.Store = reopened, reopened
	s.Clock = &fakeClock{now: s.Clock.Now().Add(2 * time.Minute)}
	s.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner != "dead-tool" })
	if leased := mustNext(t, s, parent.RunID); leased != nil {
		t.Fatal("uncertain cleanup resumed affected run")
	}
	if full, err := s.VerificationSaturated("other"); !full || err != nil {
		t.Fatalf("uncertain cleanup lost capacity: %v/%v", full, err)
	}
	for _, kind := range []string{OpExecutionInvoke, OpSourceObserve} {
		id := kind
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
		planKind(t, s, id, kind)
		leased := mustNext(t, s, id)
		if leased == nil {
			t.Fatalf("unrelated %s blocked", kind)
		}
		if _, err := s.Start(leased.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finish(leased.ID, Succeeded); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if leased := mustNext(t, s, parent.RunID); leased == nil || leased.ID != next.ID {
		t.Fatal("proven cleanup did not resume affected run")
	}
	current, _, _, err := store.VerificationPermit(p.ID)
	if err != nil || current.State != VerificationReleased {
		t.Fatalf("release not durable: %+v/%v", current, err)
	}
}
