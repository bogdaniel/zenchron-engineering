package runtime

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentDiscoveryClaimsOneRunAcrossHandles is the cross-process source
// claim proof. Two EngineeringRuntimes hold two INDEPENDENT SQLite handles on
// one database - one standing for `autonomy run issue N`, one for watch
// discovery - and race on the same source. No Go mutex is shared between them,
// so anything this proves is proved by the database.
//
// Three things must hold: exactly one EngineeringRun exists, both callers
// resolve it, and run.created appears exactly once. The last assertion is the
// sharp one: it checks that the surviving run document is still the one the
// genesis event hashed itself against, which is what an upsert-based
// query-then-insert silently breaks.
func TestConcurrentDiscoveryClaimsOneRunAcrossHandles(t *testing.T) {
	fixture := newPhase8Fixture(t)
	second, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	watcher := fixture.deps
	watcher.Store = second
	watcher.Owner = "owner-2"

	runtimes := []*EngineeringRuntime{fixture.runtime, fixture.newRuntime(watcher)}
	ids := make([]string, len(runtimes))
	errs := make([]error, len(runtimes))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, rt := range runtimes {
		wg.Add(1)
		go func(i int, rt *EngineeringRuntime) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = rt.StartOrResumeIssueRun(context.Background(), fixture.issue)
		}(i, rt)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("discovery %d failed instead of resolving the claimed run: %v", i, err)
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("callers resolved different runs: %q and %q", ids[0], ids[1])
	}

	// Read everything back through the handle that did not necessarily win.
	var runs int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("one source produced %d durable runs", runs)
	}
	events, err := second.Events(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := countType(events, EventRunCreated); got != 1 {
		t.Fatalf("run.created recorded %d times: %v", got, journalTypes(events))
	}
	run, ok, err := second.Run(ids[0])
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	genesis, err := Reduce(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if genesis.StateSHA256 != events[0].StateBefore {
		t.Fatalf("the stored run was overwritten after run.created hashed it: %s != %s",
			genesis.StateSHA256, events[0].StateBefore)
	}
}

// TestClaimRunReportsTheLoserWithoutOverwriting pins the claim primitive on its
// own: a second claim of the same identity changes nothing at all.
func TestClaimRunReportsTheLoserWithoutOverwriting(t *testing.T) {
	_, storeA, storeB := openPair(t)
	run := newJournalRun("r")
	claimed, err := storeA.ClaimRun(run)
	if err != nil || !claimed {
		t.Fatalf("first claim: %v %v", claimed, err)
	}
	loser := run
	loser.Goal = "a different goal"
	loser.Repository = "someone/else"
	if claimed, err := storeB.ClaimRun(loser); err != nil || claimed {
		t.Fatalf("second claim on another handle reported %v (%v)", claimed, err)
	}
	stored, ok, err := storeB.Run("r")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if stored.Goal != run.Goal || stored.Repository != run.Repository {
		t.Fatalf("the losing claim overwrote the winner's run: %+v", stored)
	}
	if _, err := storeA.ClaimRun(EngineeringRun{}); err == nil {
		t.Fatal("a run without an id was claimed")
	}
}
