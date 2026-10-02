package runtime

import (
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
