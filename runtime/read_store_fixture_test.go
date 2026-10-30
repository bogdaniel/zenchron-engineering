package runtime

import (
	"database/sql"
	"reflect"
	"testing"
)

// seedInterleavedRuns writes `total` events for each run into a closed journal,
// in one transaction, interleaved run by run exactly as a loop of AppendEvent
// calls would write them: sequence n of every run before sequence n+1 of any.
//
// It exists because AppendEvent replays its whole stream on every call (see its
// own "ponytail" note), so building a long journal through it is quadratic -
// 55 s plain and over ten minutes under -race for 2 x 1200 events - and almost
// all of that is fixture, not the read path under test.
//
// The rows it writes are the rows AppendEvent writes, not an approximation of
// them: chain links, event hashes, state digests, canonical documents and the
// cross-stream cursor. TestSeededJournalIsRowForRowTheAppendEventJournal holds
// that equality against the real write path, and after writing, the seeder
// replays each stream through the production reducer, which refuses a bad
// sequence, chain or hash and whose final state digest must be the one recorded.
//
// It handles events whose reduction leaves the state digest unchanged (the
// state digest excludes the cursor, so a payload-free candidate.changed keeps
// the empty run's digest). The final replay refuses anything else.
func seedInterleavedRuns(t *testing.T, db *sql.DB, runIDs []string, total int, event func(runID string, n int) EngineeringEvent) {
	t.Helper()
	runs := map[string]EngineeringRun{}
	digest := map[string]string{}
	last := map[string]EngineeringEvent{}
	for _, id := range runIDs {
		run := newJournalRun(id)
		snapshot, err := Reduce(run, nil)
		if err != nil {
			t.Fatal(err)
		}
		runs[id], digest[id] = run, snapshot.StateSHA256
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO events (` + sqliteEventInsertColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	var global int64
	for n := 1; n <= total; n++ {
		for _, id := range runIDs {
			e := event(id, n)
			e.Sequence = int64(n)
			if previous, ok := last[id]; ok {
				e.PreviousEventID, e.PreviousEventHash = previous.ID, previous.EventHash
			}
			e.StateBefore = digest[id]
			// AppendEvent hashes once with state_after still empty and again with it.
			if e.EventHash, err = EventDigest(e); err != nil {
				t.Fatal(err)
			}
			e.StateAfter = digest[id]
			if e.EventHash, err = EventDigest(e); err != nil {
				t.Fatal(err)
			}
			canonical, err := CanonicalJSON(e)
			if err != nil {
				t.Fatal(err)
			}
			global++
			if _, err := stmt.Exec(e.ID, e.RunID, e.Sequence, e.Type, e.OperationID, e.PreviousEventID,
				e.PreviousEventHash, e.StateBefore, e.StateAfter, e.EventHash, string(canonical), global); err != nil {
				t.Fatal(err)
			}
			last[id] = e
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range runIDs {
		events, err := queryEvents(db, id)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := Reduce(runs[id], events)
		if err != nil {
			t.Fatalf("the seeded journal of %q does not replay: %v", id, err)
		}
		if len(events) != total || snapshot.StateSHA256 != last[id].StateAfter {
			t.Fatalf("seeded journal of %q: %d events, state %s, recorded %s", id, len(events), snapshot.StateSHA256, last[id].StateAfter)
		}
	}
}

type journalRow struct {
	ID, RunID, Type, OperationID, PreviousEventID, PreviousEventHash, StateBefore, StateAfter, EventHash, Document string
	Sequence, GlobalSequence                                                                                       int64
}

func journalRows(t *testing.T, db *sql.DB) []journalRow {
	t.Helper()
	rows, err := db.Query(`SELECT ` + sqliteEventInsertColumns + ` FROM events ORDER BY global_sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []journalRow
	for rows.Next() {
		var r journalRow
		if err := rows.Scan(&r.ID, &r.RunID, &r.Sequence, &r.Type, &r.OperationID, &r.PreviousEventID, &r.PreviousEventHash,
			&r.StateBefore, &r.StateAfter, &r.EventHash, &r.Document, &r.GlobalSequence); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// seededJournal builds the two-run journal both ways: through the production
// write path and through the seeder. The runs are the read-store test's.
func seededJournal(t *testing.T, total int, event func(runID string, n int) EngineeringEvent) (appended, seeded []journalRow) {
	t.Helper()
	runIDs := []string{"r", "other"}
	dir, store := openJournal(t)
	for _, id := range runIDs {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n <= total; n++ {
		for _, id := range runIDs {
			if _, err := store.AppendEvent(event(id, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	appended = journalRows(t, rawJournalDB(t, dir))

	dir, store = openJournal(t)
	for _, id := range runIDs {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawJournalDB(t, dir)
	seedInterleavedRuns(t, db, runIDs, total, event)
	return appended, journalRows(t, db)
}

// The seeder is only a faster way to write the same journal. Compared against
// the real AppendEvent, every persisted column of every event is identical.
func TestSeededJournalIsRowForRowTheAppendEventJournal(t *testing.T) {
	appended, seeded := seededJournal(t, 60, changesSinceEvent)
	if len(appended) != 120 {
		t.Fatalf("the AppendEvent journal has %d events, want 2 x 60", len(appended))
	}
	if !reflect.DeepEqual(appended, seeded) {
		for i := range appended {
			if i >= len(seeded) || !reflect.DeepEqual(appended[i], seeded[i]) {
				t.Fatalf("event %d differs:\nAppendEvent %+v\nseeded      %+v", i, appended[i], seeded[min(i, len(seeded)-1)])
			}
		}
		t.Fatalf("the journals differ in length: %d against %d", len(appended), len(seeded))
	}
	// Interleaving and per-stream numbering are what the read-store test depends on.
	for i, row := range seeded {
		wantRun, wantSequence := []string{"r", "other"}[i%2], int64(i/2+1)
		if row.RunID != wantRun || row.Sequence != wantSequence || row.GlobalSequence != int64(i+1) {
			t.Fatalf("event %d is %s #%d at global %d, want %s #%d at global %d", i, row.RunID, row.Sequence, row.GlobalSequence, wantRun, wantSequence, i+1)
		}
	}
}

// Deliberate breaks: a seeder that loses any of what a journal is made of must
// be caught by the comparison, not pass it.
func TestTheJournalComparisonRefusesAnIncorrectSeed(t *testing.T) {
	appended, seeded := seededJournal(t, 12, changesSinceEvent)
	for name, damage := range map[string]func([]journalRow){
		"lost event":              func(rows []journalRow) { rows[len(rows)-1] = journalRow{} },
		"sequence gap":            func(rows []journalRow) { rows[4].Sequence += 7 },
		"streams not interleaved": func(rows []journalRow) { rows[2], rows[3] = rows[3], rows[2] },
		"global cursor reused":    func(rows []journalRow) { rows[5].GlobalSequence = rows[4].GlobalSequence },
		"chain link lost":         func(rows []journalRow) { rows[6].PreviousEventHash = "" },
		"event hash lost":         func(rows []journalRow) { rows[7].EventHash = "" },
		"state digest lost":       func(rows []journalRow) { rows[8].StateAfter = "" },
		"document altered":        func(rows []journalRow) { rows[9].Document += " " },
	} {
		broken := append([]journalRow(nil), seeded...)
		damage(broken)
		if reflect.DeepEqual(appended, broken) {
			t.Errorf("%s was not detected", name)
		}
	}
	if !reflect.DeepEqual(appended, seeded) {
		t.Fatal("the undamaged seed does not match, so the breaks above prove nothing")
	}
}
