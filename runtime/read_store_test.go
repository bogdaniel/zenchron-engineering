package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Two interleaved streams, so global_sequence cannot masquerade as sequence.
	// The journal is seeded rather than built by 2,400 AppendEvent calls, each of
	// which replays its whole stream: that fixture cost over ten minutes under
	// -race and none of it was the read path. The seed is the AppendEvent journal
	// row for row (TestSeededJournalIsRowForRowTheAppendEventJournal).
	seedDB := rawJournalDB(t, dir)
	seedInterleavedRuns(t, seedDB, []string{"r", "other"}, total, changesSinceEvent)
	if err := seedDB.Close(); err != nil {
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
			if e.RunID != "r" {
				t.Fatalf("event %s of run %q leaked into the page of run r", e.ID, e.RunID)
			}
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
	// The other stream is complete and independent of the one just paged.
	if latest, err := reader.LatestSequence("other"); err != nil || latest != total {
		t.Fatalf("latest sequence of the interleaved stream: %d, %v; want %d", latest, err, total)
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

func TestReadStoreRunBindingExposesOnlyGovernedLineage(t *testing.T) {
	dir, store := openJournal(t)
	planRun := newJournalRun("plan-run")
	planRun.Plan = &RunPlanBinding{PlanID: "plan", StageID: "stage"}
	graphRun := newJournalRun("graph-run")
	graphRun.Orchestration = &RunOrchestrationBinding{BatchID: "graph"}
	for _, run := range []EngineeringRun{newJournalRun("plain-run"), planRun, graphRun} {
		if err := store.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, test := range []struct {
		id                   string
		plan, graph, present bool
	}{{"plain-run", false, false, true}, {"plan-run", true, false, true}, {"graph-run", false, true, true}, {"missing", false, false, false}} {
		plan, graph, present, err := reader.RunBinding(test.id)
		if err != nil || plan != test.plan || graph != test.graph || present != test.present {
			t.Errorf("RunBinding(%q) = %v, %v, %v, %v", test.id, plan, graph, present, err)
		}
	}
}

func TestReadStoreRunPausedReadsLatestDurablePauseEvent(t *testing.T) {
	dir, store := openJournal(t)
	for _, id := range []string{"never-paused", "paused", "resumed"} {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().UTC()
	if _, err := PauseRun(store, at, "paused", "operator pause", "operator-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := PauseRun(store, at, "resumed", "operator pause", "operator-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := UnpauseRun(store, at.Add(time.Second), "resumed", "operator-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, test := range []struct {
		id     string
		paused bool
	}{{"never-paused", false}, {"paused", true}, {"resumed", false}} {
		paused, err := reader.RunPaused(test.id)
		if err != nil || paused != test.paused {
			t.Errorf("RunPaused(%q) = %v, %v", test.id, paused, err)
		}
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
		// A persisted row always carries its event_hash (#462); this read
		// path does not verify the chain, only that one is present.
		if e.EventHash, err = EventDigest(e); err != nil {
			t.Fatal(err)
		}
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

// TestStreamReadQueriesAreIndexSeeks pins the per-run SSE reads to a range
// seek on the full (stream_kind, run_id, plan_id, sequence) index prefix.
// Without plan_id bound, SQLite can only seek (stream_kind, run_id) and walks
// the run's whole index range per page and per steady-state poll - O(run
// length) per poll even though the rows returned stay bounded (#396).
func TestStreamReadQueriesAreIndexSeeks(t *testing.T) {
	_, store := openJournal(t)
	for _, tc := range []struct {
		query, want string
		args        []any
	}{
		{`SELECT id FROM events WHERE ` + eventsPageWhere + ` ORDER BY sequence ASC LIMIT 501`, "plan_id=? AND sequence>?", []any{streamRun, "r", 0}},
		{latestSequenceQuery, "plan_id=?", []any{streamRun, "r"}},
	} {
		query, want := tc.query, tc.want
		var plan strings.Builder
		rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+query, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), "SEARCH") || !strings.Contains(plan.String(), want) || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Fatalf("%s\nplan:\n%s\nwant a SEARCH binding %q with no sort", query, plan.String(), want)
		}
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
