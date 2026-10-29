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
	uncached, err := FleetStatus(store, dir, 0, 0, 0, at.Add(time.Hour))
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

// TestReadStoreFleetCacheDoesNotHideJournalCorruption is the #428 review
// finding: a cache keyed on the journal head served a healthy summary for a
// journal edited in place, because an UPDATE moves no head. Keyed on the bytes
// replay reads, every in-place edit misses the cache, is replayed, and is
// refused exactly as an uncached read refuses it.
func TestReadStoreFleetCacheDoesNotHideJournalCorruption(t *testing.T) {
	for _, tamper := range []struct{ name, statement string }{
		{"chain column", `UPDATE events SET previous_event_hash = '` + strings.Repeat("0", 64) + `' WHERE run_id = 'r' AND sequence = 3`},
		{"event hash column", `UPDATE events SET event_hash = '` + strings.Repeat("0", 64) + `' WHERE run_id = 'r' AND sequence = 2`},
		{"document only", `UPDATE events SET document = replace(document, 'external_review', 'external_reviez') WHERE run_id = 'r' AND sequence = 3`},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			at := time.Unix(100, 0).UTC()
			if err := store.PutRun(EngineeringRun{SchemaVersion: SchemaVersion, ID: "r", Repository: "example/repo", Phase: Execute, Disposition: Active, ControllerSHA256: strings.Repeat("4", 64), CreatedAt: at, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
			for _, e := range []EngineeringEvent{
				{ID: "created", Type: EventRunCreated},
				{ID: "waiting", Type: EventRunWaiting, Payload: []byte(`{"reason":"external_review"}`)},
				{ID: "waiting-again", Type: EventRunWaiting, Payload: []byte(`{"reason":"external_review"}`)},
			} {
				e.SchemaVersion, e.RunID, e.OccurredAt = SchemaVersion, "r", at
				if _, err := store.AppendEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := OpenReadStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			warm, err := reader.Fleet(at)
			if err != nil {
				t.Fatal(err)
			}
			if warm.Runs[0].Error != "" {
				t.Fatalf("fixture journal must replay cleanly first: %s", warm.Runs[0].Error)
			}

			db := rawJournalDB(t, dir)
			result, err := db.Exec(tamper.statement)
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := result.RowsAffected(); n != 1 {
				t.Fatalf("tamper touched %d rows; the test proves nothing", n)
			}

			after, err := reader.Fleet(at)
			if err != nil {
				t.Fatal(err)
			}
			if after.Runs[0].Error == "" {
				t.Fatal("a journal edited in place was served from cache as healthy")
			}
			uncached, err := FleetStatus(store, dir, 0, 0, 0, at)
			if err != nil {
				t.Fatal(err)
			}
			if uncached.Runs[0].Error == "" {
				t.Fatal("fixture: an uncached replay must refuse this tamper too")
			}
		})
	}
}
