package runtime

import (
	"strings"
	"testing"
	"time"
)

// TestReadStoreFleetCacheFollowsTheJournal pins the control plane's fleet
// cache: a run whose journal has not moved is served from cache with its
// clock-dependent fields recomputed, and one new event invalidates it.
func TestReadStoreFleetCacheFollowsTheJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Unix(100, 0).UTC()
	run := EngineeringRun{SchemaVersion: SchemaVersion, ID: "r", Repository: "example/repo", Phase: Execute, Disposition: Active, ControllerSHA256: strings.Repeat("4", 64), CreatedAt: at, UpdatedAt: at}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "created", RunID: "r", Type: EventRunCreated, OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	first, err := reader.Fleet(at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.Fleet(at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Runs[0].Elapsed != time.Hour || first.Runs[0].Elapsed != time.Minute {
		t.Fatalf("a cached summary must still recompute elapsed from now: %v then %v", first.Runs[0].Elapsed, second.Runs[0].Elapsed)
	}

	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "waiting", RunID: "r", Type: EventRunWaiting, OccurredAt: at, Payload: []byte(`{"reason":"external_review"}`)}); err != nil {
		t.Fatal(err)
	}
	third, err := reader.Fleet(at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	uncached, err := FleetStatus(store, dir, 0, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, want := third.Runs[0], uncached.Runs[0]
	if got.Disposition != want.Disposition || got.Reason != want.Reason {
		t.Fatalf("a new journal event must invalidate the cached summary: cached %s/%q, replayed %s/%q", got.Disposition, got.Reason, want.Disposition, want.Reason)
	}
	if got.Disposition == second.Runs[0].Disposition && got.Reason == second.Runs[0].Reason {
		t.Fatalf("fixture did not change the summary (%s/%q); the test proves nothing", got.Disposition, got.Reason)
	}
}
