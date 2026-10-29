package runtime

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// changesSinceEvent builds a minimal, validly-typed event for a given run
// with an id unique across every run in the store - event id is a PRIMARY
// KEY on the whole events table, not scoped per run, so two runs can never
// reuse journalFixture's literal "e-1".."e-4" ids in the same database.
func changesSinceEvent(runID string, n int) EngineeringEvent {
	return EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            fmt.Sprintf("%s-e%d", runID, n),
		RunID:         runID,
		Type:          EventCandidateChanged,
		OccurredAt:    time.Unix(int64(100+n), 0).UTC(),
	}
}

// TestChangesSinceDeterministicCrossRunOrder interleaves events across two
// runs and checks that ChangesSince reports them in the order their events
// were actually committed, not run-creation order or alphabetical order.
func TestChangesSinceDeterministicCrossRunOrder(t *testing.T) {
	_, store := openJournal(t)
	if err := store.PutRun(newJournalRun("other")); err != nil {
		t.Fatal(err)
	}
	// Interleave: other, r, other, r - so a run-id ordered or creation ordered
	// read would disagree with the true commit order asserted below.
	order := []EngineeringEvent{
		changesSinceEvent("other", 1), changesSinceEvent("r", 1),
		changesSinceEvent("other", 2), changesSinceEvent("r", 2),
	}
	for _, e := range order {
		if _, err := store.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	// A single wide read collapses each run's two events into one RunChange -
	// ChangesSince reports distinct changed runs, not raw event rows - so
	// commit order is observed here by paging one underlying row at a time.
	wantRuns := []string{"other", "r", "other", "r"}
	cursor := int64(0)
	for i, want := range wantRuns {
		changes, next, err := store.ChangesSince(cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 1 {
			t.Fatalf("page %d: got %d changes, want 1: %+v", i, len(changes), changes)
		}
		if changes[0].RunID != want {
			t.Fatalf("page %d: got run %q, want %q", i, changes[0].RunID, want)
		}
		if next <= cursor {
			t.Fatalf("page %d: cursor did not advance: %d -> %d", i, cursor, next)
		}
		cursor = next
	}
	done, next, err := store.ChangesSince(cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 || next != cursor {
		t.Fatalf("expected no further changes, got %+v (next=%d)", done, next)
	}
}

// TestChangesSinceRestartPreservesCursor closes and reopens the store between
// two batches of appends, proving the cursor is durable rather than an
// in-memory counter that a restart would reset to zero.
func TestChangesSinceRestartPreservesCursor(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	fixture := journalFixture(t, "r")
	if _, err := store.AppendEvent(fixture[0]); err != nil {
		t.Fatal(err)
	}
	firstBatch, cursor, err := store.ChangesSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstBatch) != 1 {
		t.Fatalf("expected one change before restart, got %+v", firstBatch)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Reading from the pre-restart cursor must be empty: nothing changed yet.
	empty, sameCursor, err := reopened.ChangesSince(cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || sameCursor != cursor {
		t.Fatalf("restart invented a change: changes=%+v cursor=%d want=%d", empty, sameCursor, cursor)
	}
	if _, err := reopened.AppendEvent(fixture[1]); err != nil {
		t.Fatal(err)
	}
	after, next, err := reopened.ChangesSince(cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].RunID != "r" || after[0].Cursor <= cursor {
		t.Fatalf("cursor was not preserved across restart: %+v (prior cursor %d)", after, cursor)
	}
	if next != after[0].Cursor {
		t.Fatalf("nextCursor %d does not match the only change's cursor %d", next, after[0].Cursor)
	}
}

// TestChangesSinceReReadIsIdempotent calls ChangesSince twice from the same
// cursor and requires byte-for-byte identical results.
func TestChangesSinceReReadIsIdempotent(t *testing.T) {
	_, store := openJournal(t)
	for _, e := range journalFixture(t, "r") {
		if _, err := store.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	first, firstNext, err := store.ChangesSince(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, secondNext, err := store.ChangesSince(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if firstNext != secondNext {
		t.Fatalf("nextCursor differs across identical reads: %d vs %d", firstNext, secondNext)
	}
	if len(first) != len(second) {
		t.Fatalf("result length differs across identical reads: %+v vs %+v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("result %d differs across identical reads: %+v vs %+v", i, first[i], second[i])
		}
	}
}

// TestChangesSinceBoundedPaginationOmitsNoRun drains a journal spanning many
// runs through a small page size and checks that every run's most recent
// sequence was eventually observed, and that pagination terminates.
func TestChangesSinceBoundedPaginationOmitsNoRun(t *testing.T) {
	_, store := openJournal(t)
	const runCount = 25
	const eventsPerRun = 4
	want := map[string]int64{}
	for i := 0; i < runCount; i++ {
		id := fmt.Sprintf("run-%02d", i)
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= eventsPerRun; n++ {
			stored, err := store.AppendEvent(changesSinceEvent(id, n))
			if err != nil {
				t.Fatal(err)
			}
			want[id] = stored.Sequence
		}
	}
	got := map[string]int64{}
	cursor := int64(0)
	for iterations := 0; ; iterations++ {
		if iterations > runCount*eventsPerRun+1 {
			t.Fatal("pagination did not terminate")
		}
		changes, next, err := store.ChangesSince(cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == 0 {
			break
		}
		for _, c := range changes {
			if c.Sequence > got[c.RunID] {
				got[c.RunID] = c.Sequence
			}
		}
		if next <= cursor {
			t.Fatalf("cursor failed to advance: %d -> %d", cursor, next)
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("observed %d runs, want %d: got=%v want=%v", len(got), len(want), got, want)
	}
	for id, seq := range want {
		if got[id] != seq {
			t.Fatalf("run %q: observed highest sequence %d, want %d", id, got[id], seq)
		}
	}
}

// TestChangesSinceFeedsEventsAfter is the intended consumer loop: use the
// Sequence ChangesSince reports to resume a run's own durable replay cursor
// via EventsAfter, and confirm it returns exactly the events appended after
// that point and nothing the consumer already saw.
func TestChangesSinceFeedsEventsAfter(t *testing.T) {
	_, store := openJournal(t)
	fixture := journalFixture(t, "r")
	if _, err := store.AppendEvent(fixture[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(fixture[1]); err != nil {
		t.Fatal(err)
	}
	changes, cursor, err := store.ChangesSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].RunID != "r" || changes[0].Sequence != 2 {
		t.Fatalf("unexpected changes: %+v", changes)
	}
	caughtUp, err := store.EventsAfter("r", changes[0].Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(caughtUp) != 0 {
		t.Fatalf("expected no events beyond what ChangesSince already reported, got %+v", caughtUp)
	}
	if _, err := store.AppendEvent(fixture[2]); err != nil {
		t.Fatal(err)
	}
	more, nextCursor, err := store.ChangesSince(cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 1 || more[0].RunID != "r" || more[0].Sequence != 3 {
		t.Fatalf("unexpected follow-up changes: %+v", more)
	}
	resumed, err := store.EventsAfter("r", changes[0].Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].ID != fixture[2].ID {
		t.Fatalf("EventsAfter did not resume where ChangesSince left off: %+v", resumed)
	}
	if nextCursor != more[0].Cursor {
		t.Fatalf("nextCursor %d does not match the only change's cursor %d", nextCursor, more[0].Cursor)
	}
}

func TestChangesSinceRejectsNonPositiveLimit(t *testing.T) {
	_, store := openJournal(t)
	if _, _, err := store.ChangesSince(0, 0); err == nil {
		t.Fatal("accepted a zero limit")
	}
	if _, _, err := store.ChangesSince(0, -1); err == nil {
		t.Fatal("accepted a negative limit")
	}
}

// TestChangesSinceStaysBoundedOnALargeJournal measures the actual concern
// #96 raised: a fleet-wide read must not cost more as the journal grows. It
// builds a journal sized like current dogfood (hundreds of runs, thousands of
// events) and checks two things a correct-but-unbounded implementation could
// still pass: the query plan touches the global_sequence index rather than
// scanning every row, and a page read from deep in the journal is not
// measurably slower than one from the start.
func TestChangesSinceStaysBoundedOnALargeJournal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-thousand-event journal")
	}
	_, store := openJournal(t)
	const runs = 300
	const eventsPerRun = 10
	for i := 0; i < runs; i++ {
		id := fmt.Sprintf("dogfood-%04d", i)
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= eventsPerRun; n++ {
			if _, err := store.AppendEvent(changesSinceEvent(id, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	total := int64(runs * eventsPerRun)

	var plan strings.Builder
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN SELECT run_id, sequence, global_sequence FROM events
		WHERE stream_kind = ? AND global_sequence > ? ORDER BY global_sequence ASC LIMIT ?`, streamRun, total-5, 3)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		dest := make([]any, len(cols))
		raw := make([]sql.RawBytes, len(cols))
		for i := range dest {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for _, c := range raw {
			plan.Write(c)
			plan.WriteByte(' ')
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := plan.String(); !strings.Contains(got, "events_stream_global_sequence") {
		t.Fatalf("ChangesSince's query does not use the global_sequence index, so its cost grows with the whole journal: %s", got)
	}

	early := timeChangesSince(t, store, 0, 3)
	late := timeChangesSince(t, store, total-3, 3)
	if late > early*20+time.Millisecond {
		t.Fatalf("reading near the end of a %d-event journal (%v) was not bounded relative to reading its start (%v)", total, late, early)
	}
}

func timeChangesSince(t *testing.T, store *SQLiteOperationStore, cursor int64, limit int) time.Duration {
	t.Helper()
	start := time.Now()
	if _, _, err := store.ChangesSince(cursor, limit); err != nil {
		t.Fatal(err)
	}
	return time.Since(start)
}
