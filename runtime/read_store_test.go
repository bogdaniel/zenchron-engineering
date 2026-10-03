package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadStoreAndBoundedEventsPage(t *testing.T) {
	dir, store := openJournal(t)
	const total = 1200
	for _, id := range []string{"r", "other"} {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n <= total; n++ {
		// Interleave another stream so global_sequence cannot masquerade as sequence.
		for _, id := range []string{"r", "other"} {
			if _, err := store.AppendEvent(changesSinceEvent(id, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runtime.db")
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.store.db.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("read handle permitted a mutation")
	}
	var cursor int64
	for {
		events, more, err := reader.EventsPage("r", cursor, 37)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 37 {
			t.Fatal("unbounded page")
		}
		for _, e := range events {
			if e.Sequence != cursor+1 {
				t.Fatalf("gap/duplicate at %d: %d", cursor, e.Sequence)
			}
			cursor = e.Sequence
		}
		if !more {
			break
		}
	}
	if cursor != total {
		t.Fatal(cursor)
	}
	if _, err := reader.Fleet(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Status("r", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("database changed")
	}
	for _, limit := range []int{0, -1, 501} {
		if _, _, err := reader.EventsPage("r", 0, limit); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
	if _, _, err := reader.EventsPage("r", -1, 1); err == nil {
		t.Fatal("accepted negative cursor")
	}
}

func TestEventsPageDoesNotDecodeUnboundedTail(t *testing.T) {
	_, store := openJournal(t)
	for _, e := range journalFixture(t, "r") {
		if _, err := store.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE events SET document='{}' WHERE sequence=4`); err != nil {
		t.Fatal(err)
	}
	events, more, err := store.EventsPage("r", 0, 2)
	if err != nil || len(events) != 2 || !more {
		t.Fatalf("%v %v %v", events, more, err)
	}
	if _, _, err := store.EventsPage("r", 2, 2); err == nil {
		t.Fatal("accepted corrupt document")
	}
}

// TestBoundedEventReadCostAtAJournalScale measures - and records in the test
// log - what the per-run SSE boundary's own read pattern costs against a
// large, real-shaped journal: LatestSequence for the initial snapshot cursor,
// then the same bounded, LIMIT-paged EventsPage loop drain() uses to replay
// backlog. The journal is built by inserting rows directly rather than
// through AppendEvent, because AppendEvent re-replays the whole run on every
// call (see its own "ponytail" note) and paying that O(n^2) cost here would
// measure fixture setup, not the read path this test exists to measure.
func TestBoundedEventReadCostAtAJournalScale(t *testing.T) {
	const total = 20_000
	dir, store := openJournal(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawJournalDB(t, dir)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO events (` + sqliteEventInsertColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	for n := int64(1); n <= total; n++ {
		e := EngineeringEvent{SchemaVersion: SchemaVersion, ID: fmt.Sprintf("e%d", n), RunID: "r", Sequence: n, Type: "synthetic.marker", OccurredAt: at.Add(time.Duration(n) * time.Second)}
		document, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stmt.Exec(e.ID, e.RunID, e.Sequence, e.Type, e.OperationID, e.PreviousEventID, e.PreviousEventHash, e.StateBefore, e.StateAfter, e.EventHash, string(document), n); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	start := time.Now()
	latest, err := reader.LatestSequence("r")
	latestElapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if latest != total {
		t.Fatalf("latest sequence: %d, want %d", latest, total)
	}

	const page = 100
	start = time.Now()
	var cursor int64
	pages := 0
	for {
		events, more, err := reader.EventsPage("r", cursor, page)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range events {
			cursor = e.Sequence
		}
		if !more {
			break
		}
	}
	drainElapsed := time.Since(start)
	if cursor != total {
		t.Fatalf("drained to %d, want %d", cursor, total)
	}
	t.Logf("read cost at %d run events: LatestSequence=%s; bounded drain across %d pages of %d=%s", total, latestElapsed, pages, page, drainElapsed)
	if drainElapsed > 5*time.Second {
		t.Fatalf("bounded drain of a %d-event journal took %s; a LIMIT-paged, indexed read should not scale like this", total, drainElapsed)
	}
}

func TestReadStoreRefusesMissingAndOldDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadStore(dir); err == nil {
		t.Fatal("created missing database")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("read open created files", entries, err)
	}
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenReadStore(dir); err == nil {
		t.Fatal("accepted old schema")
	}
}
