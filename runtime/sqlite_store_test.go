package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openPair returns two independent store handles on one database file. Every
// concurrency claim below is proved across handles: no shared Go mutex can be
// what makes these tests pass.
func openPair(t *testing.T) (string, *SQLiteOperationStore, *SQLiteOperationStore) {
	t.Helper()
	dir := t.TempDir()
	first, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return dir, first, second
}

func alwaysAlive() OwnerLiveness { return OwnerLivenessFunc(func(string) bool { return true }) }
func neverAlive() OwnerLiveness  { return OwnerLivenessFunc(func(string) bool { return false }) }

func TestSQLiteOnlyOneSchedulerAcquiresAnOperation(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	if _, created, err := one.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a"}); err != nil || !created {
		t.Fatal(err, created)
	}
	var mu sync.Mutex
	leases := []*RunOperation{}
	var wg sync.WaitGroup
	for _, s := range []Scheduler{one, two} {
		wg.Add(1)
		go func(s Scheduler) {
			defer wg.Done()
			got, err := s.Next("r")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			if got != nil {
				leases = append(leases, got)
			}
		}(s)
	}
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("expected exactly one acquisition, got %d", len(leases))
	}
	op, _, ok, err := storeB.Operation(leases[0].ID)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if op.State != Leased || op.Lease == nil || op.Lease.Owner != leases[0].Lease.Owner {
		t.Fatalf("durable lease does not match the winner: %+v", op)
	}
}

func TestSQLitePlanIsIdempotentAcrossHandles(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	planned := RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a"}
	var mu sync.Mutex
	createdCount := 0
	ids := map[string]bool{}
	var wg sync.WaitGroup
	for _, store := range []OperationStore{storeA, storeB} {
		wg.Add(1)
		go func(store OperationStore) {
			defer wg.Done()
			s := Scheduler{Store: store, Clock: c, Owner: "o", Liveness: alwaysAlive()}
			op, created, err := s.Plan(planned)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				createdCount++
			}
			ids[op.ID] = true
		}(store)
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatalf("expected exactly one creation, got %d", createdCount)
	}
	if len(ids) != 1 {
		t.Fatalf("expected one logical operation, got %d distinct ids", len(ids))
	}
	all, err := storeA.AllOperations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected one durable row, got %d", len(all))
	}
}

func TestSQLitePlanRejectsForeignKindForKey(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	if _, _, err := (Scheduler{Store: storeA, Clock: c, Owner: "one"}).Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := (Scheduler{Store: storeB, Clock: c, Owner: "two"}).Plan(RunOperation{RunID: "r", Kind: "other", IdempotencyKey: "a"})
	if err == nil {
		t.Fatal("expected a kind conflict for a reused idempotency key")
	}
}

func TestSQLiteHeartbeatUpdatesAreNotLost(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	s := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	op, _, err := s.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next("r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(op.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c.now = c.now.Add(10 * time.Second)
		if _, err := s.Heartbeat(op.ID, "progress"); err != nil {
			t.Fatal(err)
		}
		durable, _, ok, err := storeB.Operation(op.ID)
		if err != nil || !ok {
			t.Fatal(err, ok)
		}
		if !durable.Lease.HeartbeatAt.Equal(c.now) || !durable.Lease.ExpiresAt.Equal(c.now.Add(time.Minute)) {
			t.Fatalf("heartbeat %d was lost: %+v", i, durable.Lease)
		}
	}
}

func TestSQLiteStaleWriteCannotOverwriteLeaseState(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	s := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	op, _, err := s.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next("r"); err != nil {
		t.Fatal(err)
	}
	stale, revision, ok, err := storeB.Operation(op.ID)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	c.now = c.now.Add(30 * time.Second)
	if _, err := s.Heartbeat(op.ID, "progress"); err != nil {
		t.Fatal(err)
	}
	stale.State = OperationFailed
	stale.Lease = nil
	if _, written, err := storeB.PutOperation(stale, revision); err != nil || written {
		t.Fatalf("stale revision %d overwrote newer state: written=%v err=%v", revision, written, err)
	}
	durable, _, _, err := storeA.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.State != Leased || durable.Lease == nil || !durable.Lease.HeartbeatAt.Equal(c.now) {
		t.Fatalf("stale write damaged durable state: %+v", durable)
	}
}

func TestSQLiteWallBudgetTerminalClearsLease(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	s := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	op, _, err := s.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: "a", MaxAttempts: 2, WallBudget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next("r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(op.ID); err != nil {
		t.Fatal(err)
	}
	// The owner crashed: its lease expired and liveness can prove it is gone.
	c.now = c.now.Add(5 * time.Minute)
	successor := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: neverAlive()}
	if got, err := successor.Next("r"); err != nil || got != nil {
		t.Fatalf("a budget-exceeded operation must not be leased: %v %v", got, err)
	}
	durable, _, _, err := storeA.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.State != OperationFailed {
		t.Fatalf("expected terminal failed state, got %q", durable.State)
	}
	if durable.Lease != nil {
		t.Fatalf("terminal budget transition must clear the lease, got %+v", durable.Lease)
	}
}

func TestSQLiteStateAndOrderingSurviveReopen(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := Scheduler{Store: store, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive()}
	planned := []RunOperation{}
	for _, key := range []string{"c", "a", "b"} {
		c.now = c.now.Add(time.Second)
		op, _, err := s.Plan(RunOperation{RunID: "r", Kind: "k", IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		planned = append(planned, op)
	}
	leased, err := s.Next("r")
	if err != nil || leased == nil {
		t.Fatal(err, leased)
	}
	if _, err := s.Start(leased.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.Operations("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(planned) {
		t.Fatalf("expected %d operations after reopen, got %d", len(planned), len(after))
	}
	for i, op := range after {
		if op.ID != planned[i].ID {
			t.Fatalf("ordering changed at %d: got %q want %q", i, op.ID, planned[i].ID)
		}
	}
	// The memory store is the ordering reference; both must agree.
	memory := NewMemoryOperationStore()
	for _, op := range planned {
		if _, _, err := memory.PutOperation(op, 0); err != nil {
			t.Fatal(err)
		}
	}
	reference, err := memory.Operations("r")
	if err != nil {
		t.Fatal(err)
	}
	for i, op := range reference {
		if op.ID != after[i].ID {
			t.Fatalf("store ordering diverges at %d: sqlite %q memory %q", i, after[i].ID, op.ID)
		}
	}
	restored, _, ok, err := reopened.Operation(leased.ID)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if restored.State != Running || restored.Lease == nil || restored.Lease.Owner != "one" || restored.Attempt != 1 {
		t.Fatalf("crash recovery lost operation state: %+v", restored)
	}
}

func TestSQLiteRefusesNewerSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dir+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteOperationStore(dir)
	var unsupported UnsupportedSchemaError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected UnsupportedSchemaError, got %v", err)
	}
	if unsupported.Found != 99 || unsupported.Supported != sqliteSchemaVersion {
		t.Fatalf("unexpected schema error detail: %+v", unsupported)
	}
}

func TestProcessOwnerLivenessIsConservative(t *testing.T) {
	liveness := NewProcessOwnerLiveness()
	if !liveness.Alive(NewRuntimeOwner()) {
		t.Fatal("this process must be reported alive")
	}
	for _, owner := range []string{"", "nonsense", "other-host/1/token", "host/notapid/token", ownerHost() + "/0/token"} {
		if !liveness.Alive(owner) {
			t.Fatalf("unprovable owner %q must be treated as alive", owner)
		}
	}
}

// The journal shares this database, its connection pool, and its migration
// list, so operation and journal writes are proved to interleave across two
// independent handles rather than being serialized by a process-local lock.
func TestSQLiteOperationAndJournalWritesShareOneDatabase(t *testing.T) {
	_, storeA, storeB := openPair(t)
	if err := storeA.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	const each = 8
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			if _, written, err := storeA.PutOperation(RunOperation{SchemaVersion: SchemaVersion, ID: fmt.Sprintf("op-%d", i), RunID: "r", Kind: "k", IdempotencyKey: fmt.Sprintf("key-%d", i)}, 0); err != nil || !written {
				t.Errorf("operation %d was not written: %v %v", i, written, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < each; i++ {
			if _, err := storeB.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: fmt.Sprintf("e-%d", i), RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(200, 0).UTC()}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	operations, err := storeB.AllOperations()
	if err != nil || len(operations) != each {
		t.Fatalf("expected %d durable operations, got %d (%v)", each, len(operations), err)
	}
	snapshot, err := storeA.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor.LastSequence != each {
		t.Fatalf("expected the journal at sequence %d, got %+v", each, snapshot.Cursor)
	}
}

func TestSQLiteMigratesAJournalFreeDatabaseForward(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(sqliteMigrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO run_operations (` + sqliteOperationColumns + `) VALUES ('op', 'r', 'k', 'a', 1, 1, '{"id":"op","run_id":"r","kind":"k"}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != sqliteSchemaVersion {
		t.Fatalf("expected schema version %d after migration, got %d", sqliteSchemaVersion, version)
	}
	operations, err := store.AllOperations()
	if err != nil || len(operations) != 1 || operations[0].ID != "op" {
		t.Fatalf("migration lost pre-existing operations: %+v %v", operations, err)
	}
	if err := store.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r", Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStateFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r", Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("state directory is %v, want 0700", info.Mode().Perm())
	}
	// The -wal and -shm sidecars carry journal content too.
	for _, name := range []string{"runtime.db", "runtime.db-wal", "runtime.db-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("%s is %v, want no group or other access", name, info.Mode().Perm())
		}
	}
}

// ---------------------------------------------------------------------------
// Durable global run concurrency
// ---------------------------------------------------------------------------

// planFor plans one operation for a run through a scheduler and returns it.
func planFor(t *testing.T, s Scheduler, runID string, maxAttempts int) RunOperation {
	t.Helper()
	op, created, err := s.Plan(RunOperation{RunID: runID, Kind: "k", IdempotencyKey: runID, MaxAttempts: maxAttempts})
	if err != nil || !created {
		t.Fatalf("plan %s: %v %v", runID, created, err)
	}
	return op
}

func leasedAt(op RunOperation, owner string, now time.Time) RunOperation {
	op.State = Leased
	op.Lease = &Lease{Owner: owner, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute)}
	return op
}

// TestSQLiteGlobalRunCeilingIsAtomicAcrossHandles is the max=1 proof, and it is
// deliberately deterministic rather than goroutine-timed: BOTH handles read
// their operation first, so both hold a revision taken while the ceiling was
// genuinely free. Only the database can break that tie, and it must - a
// count-then-write implementation lets both of them through.
func TestSQLiteGlobalRunCeilingIsAtomicAcrossHandles(t *testing.T) {
	now := time.Unix(100, 0)
	c := &fakeClock{now: now}
	_, storeA, storeB := openPair(t)
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	planFor(t, one, "run-a", 1)
	planFor(t, two, "run-b", 1)

	opA, revA, okA, err := storeA.Operation(StableOperationKey("run-a", "k", "run-a"))
	if err != nil || !okA {
		t.Fatal(err, okA)
	}
	opB, revB, okB, err := storeB.Operation(StableOperationKey("run-b", "k", "run-b"))
	if err != nil || !okB {
		t.Fatal(err, okB)
	}
	_, gotA, err := storeA.AcquireOperation(leasedAt(opA, "one", now), revA, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, gotB, err := storeB.AcquireOperation(leasedAt(opB, "two", now), revB, 1)
	if err != nil {
		t.Fatal(err)
	}
	if gotA == gotB {
		t.Fatalf("max=1 was not durable: run-a acquired=%v, run-b acquired=%v", gotA, gotB)
	}
	if _, _, err := storeA.AcquireOperation(opA, 0, 1); err == nil {
		t.Fatal("an acquisition without an observed revision was accepted")
	}

	// Second: the count and the write are ONE statement. Two independent
	// handles are released from a barrier together on a fresh database each
	// time, both trying to drive a DIFFERENT run. An implementation that
	// counts first and writes second eventually lets both through, and the
	// repetition is what turns "eventually" into a reliable failure.
	for attempt := 0; attempt < 32; attempt++ {
		if got := raceTwoRunsForTheSingleSlot(t); got != 1 {
			t.Fatalf("attempt %d drove %d runs at once under max=1", attempt, got)
		}
	}
}

// raceTwoRunsForTheSingleSlot returns how many of two concurrent schedulers,
// on two independent database handles and two different runs, ended up driving.
func raceTwoRunsForTheSingleSlot(t *testing.T) int {
	t.Helper()
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	planFor(t, one, "run-a", 1)
	planFor(t, two, "run-b", 1)

	var driving atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, driver := range []struct {
		scheduler Scheduler
		run       string
	}{{one, "run-a"}, {two, "run-b"}} {
		wg.Add(1)
		go func(s Scheduler, run string) {
			defer wg.Done()
			<-start
			got, err := s.Next(run)
			if err != nil {
				t.Error(err)
				return
			}
			if got != nil {
				driving.Add(1)
			}
		}(driver.scheduler, driver.run)
	}
	close(start)
	wg.Wait()
	return int(driving.Load())
}

// TestSQLiteRunSlotIsYieldedWhenTheRunStopsBeingDriven proves the slot is held
// by ACTIVE DRIVING, not by the existence of a durable run. run-a's operation
// finishing is exactly what "waiting on CI, authority, auth, or opt-in removal"
// looks like to the scheduler: the run row is still there, nothing is leased,
// and the second process may drive its own run. Nothing calls a release.
func TestSQLiteRunSlotIsYieldedWhenTheRunStopsBeingDriven(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	planFor(t, one, "run-a", 1)
	planFor(t, two, "run-b", 1)

	leased, err := one.Next("run-a")
	if err != nil || leased == nil {
		t.Fatal(err, leased)
	}
	if got, err := two.Next("run-b"); err != nil || got != nil {
		t.Fatalf("a second run was driven while run-a held the only slot: %v %v", got, err)
	}
	if _, err := one.Finish(leased.ID, Succeeded); err != nil {
		t.Fatal(err)
	}
	got, err := two.Next("run-b")
	if err != nil || got == nil {
		t.Fatalf("a parked run kept its slot: %v %v", got, err)
	}
}

// TestSQLiteRunSlotIsNotStolenByExpiryAlone keeps the frozen CanAcquire rule
// honest at the slot level too: an expired lease held by a live owner blocks
// both the operation and the global slot, and only proven owner death plus
// expiry releases either.
func TestSQLiteRunSlotIsNotStolenByExpiryAlone(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	_, storeA, storeB := openPair(t)
	one := Scheduler{Store: storeA, Clock: c, Owner: "one", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	two := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: alwaysAlive(), MaxConcurrentRuns: 1}
	planFor(t, one, "run-a", 2)
	planFor(t, two, "run-b", 2)

	leased, err := one.Next("run-a")
	if err != nil || leased == nil {
		t.Fatal(err, leased)
	}
	if _, err := one.Start(leased.ID); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(2 * time.Minute)
	if got, err := two.Next("run-a"); err != nil || got != nil {
		t.Fatalf("expiry alone stole a live owner's operation: %v %v", got, err)
	}
	if got, err := two.Next("run-b"); err != nil || got != nil {
		t.Fatalf("expiry alone stole a live owner's run slot: %v %v", got, err)
	}
	dead := Scheduler{Store: storeB, Clock: c, Owner: "two", LeaseDuration: time.Minute, Liveness: neverAlive(), MaxConcurrentRuns: 1}
	if got, err := dead.Next("run-a"); err != nil || got == nil {
		t.Fatalf("owner death plus expiry did not permit reclamation: %v %v", got, err)
	}
}
