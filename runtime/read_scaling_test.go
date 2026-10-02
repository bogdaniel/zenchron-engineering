package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEventsAfterCursorAndValidation(t *testing.T) {
	_, store := openJournal(t)
	for _, event := range journalFixture(t, "r") {
		if _, err := store.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, cursor := range []int64{0, 2, 4, 100} {
		events, err := store.EventsAfter("r", cursor)
		if err != nil {
			t.Fatal(err)
		}
		want := 4 - int(cursor)
		if want < 0 {
			want = 0
		}
		if len(events) != want {
			t.Fatalf("cursor %d: got %d rows, want %d", cursor, len(events), want)
		}
		for i, event := range events {
			if event.Sequence != cursor+int64(i)+1 {
				t.Fatalf("unexpected sequence %d", event.Sequence)
			}
		}
	}
	// Old rows are not decoded, but every returned row is still validated.
	if _, err := store.db.Exec(`UPDATE events SET document = '{}' WHERE sequence = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventsAfter("r", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventsAfter("r", 0); err == nil {
		t.Fatal("accepted inconsistent row")
	}
}

// recordingEventQuerier wraps a real *sql.DB and remembers the last query
// text and arguments it was asked to run, so a test can inspect what was
// actually sent to SQLite rather than only the Go-level result.
type recordingEventQuerier struct {
	db        *sql.DB
	lastQuery string
	lastArgs  []any
}

func (r *recordingEventQuerier) Query(query string, args ...any) (*sql.Rows, error) {
	r.lastQuery = query
	r.lastArgs = args
	return r.db.Query(query, args...)
}

// TestEventsPageCarriesTheBoundIntoSQL proves the bound is a property of the
// query SQLite executes, not a slice applied to an already-fetched result:
// the statement itself carries "ORDER BY sequence ASC LIMIT ?", and the limit
// argument is one more than the page size (fetching one extra row is how the
// caller learns whether the page was truncated without a second query).
func TestEventsPageCarriesTheBoundIntoSQL(t *testing.T) {
	_, store := openJournal(t)
	for _, event := range journalFixture(t, "r") {
		if _, err := store.AppendEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &recordingEventQuerier{db: store.db}
	events, err := queryStreamEventsLimit(recorder, 3, `stream_kind = ? AND run_id = ? AND sequence > ?`, streamRun, "r", int64(0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.lastQuery, "ORDER BY sequence ASC LIMIT ?") {
		t.Fatalf("query did not carry a SQL-level LIMIT: %q", recorder.lastQuery)
	}
	if len(recorder.lastArgs) == 0 || recorder.lastArgs[len(recorder.lastArgs)-1] != 3 {
		t.Fatalf("LIMIT argument = %v, want 3 as the final bound parameter", recorder.lastArgs)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want the 3-row bound honored", len(events))
	}
}

// TestEventsPageOnALargeTailOnlyMaterializesTheBoundedPage seeds a run with a
// tail far larger than any one page and proves two things: a single call
// never returns more than limit+1 rows regardless of how much history exists
// behind the cursor, and walking the whole tail page by page via NextAfter
// visits every sequence exactly once - no gaps, no duplicates.
func TestEventsPageOnALargeTailOnlyMaterializesTheBoundedPage(t *testing.T) {
	_, store := openJournal(t)
	const total = 237
	for i := 0; i < total; i++ {
		if _, err := store.AppendEvent(EngineeringEvent{
			SchemaVersion: SchemaVersion,
			ID:            fmt.Sprintf("tail-%d", i),
			RunID:         "r",
			Type:          EventRunCreated,
		}); err != nil {
			t.Fatal(err)
		}
	}

	const limit = 10
	var after int64
	seen := map[int64]bool{}
	pages := 0
	for {
		page, err := store.EventsPage("r", after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > limit+1 {
			t.Fatalf("page returned %d rows, want at most limit+1=%d", len(page), limit+1)
		}
		truncated := len(page) > limit
		if truncated {
			page = page[:limit]
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if event.Sequence <= after {
				t.Fatalf("page returned sequence %d at or before cursor %d", event.Sequence, after)
			}
			if seen[event.Sequence] {
				t.Fatalf("sequence %d returned twice across pages", event.Sequence)
			}
			seen[event.Sequence] = true
		}
		after = page[len(page)-1].Sequence
		pages++
		if !truncated {
			break
		}
		if pages > total {
			t.Fatal("cursor chaining did not converge")
		}
	}
	if len(seen) != total {
		t.Fatalf("walked %d distinct sequences, want %d", len(seen), total)
	}
	for seq := int64(1); seq <= int64(total); seq++ {
		if !seen[seq] {
			t.Fatalf("sequence %d was never visited: gap in cursor chaining", seq)
		}
	}
}

func TestActiveOperationsFiltersRunAndState(t *testing.T) {
	_, store := openJournal(t)
	for i, state := range []OperationState{Pending, Leased, Running, Succeeded, OperationFailed, OperationCancelled} {
		for _, run := range []string{"r", "other"} {
			id := fmt.Sprintf("%s-%d", run, i)
			_, ok, err := store.PutOperation(RunOperation{ID: id, RunID: run, IdempotencyKey: id, State: state}, 0)
			if err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
	}
	for _, run := range []string{"", "r", "missing"} {
		ops, err := store.ActiveOperations(run)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"": 4, "r": 2, "missing": 0}[run]
		if len(ops) != want {
			t.Fatalf("run %q: got %d, want %d", run, len(ops), want)
		}
		for _, op := range ops {
			if op.State != Leased && op.State != Running {
				t.Fatal(op.State)
			}
			if run != "" && op.RunID != run {
				t.Fatal(op.RunID)
			}
		}
	}
}

func TestFeedbackFoldCacheTracksAppend(t *testing.T) {
	s := &runState{}
	if len(s.feedbackState().Consumed) != 0 {
		t.Fatal("unexpected consumed feedback")
	}
	payload, err := json.Marshal(FeedbackConsumedPayload{Keys: []string{"key"}})
	if err != nil {
		t.Fatal(err)
	}
	s.events = append(s.events, EngineeringEvent{Type: EventFeedbackConsumed, Payload: payload})
	if !s.feedbackState().Consumed["key"] {
		t.Fatal("cache did not observe appended event")
	}
	cached := s.feedbackCached
	s.feedbackState()
	if s.feedbackCached != cached {
		t.Fatal("unchanged journal was refolded")
	}
}

func TestActiveRunsFiltersDispositionUpdates(t *testing.T) {
	_, store := openJournal(t)
	for i, disposition := range []Disposition{Active, Waiting, Completed, Failed, Cancelled} {
		run := newJournalRun(fmt.Sprintf("run-%d", i))
		run.Disposition = disposition
		if err := store.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.ActiveRuns()
	if err != nil || len(runs) != 3 {
		t.Fatalf("active runs: %v, %v", runs, err)
	}
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			t.Fatal(run.Disposition)
		}
		run.Disposition = Completed
		if err := store.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
	runs, err = store.ActiveRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("terminal runs retained: %v, %v", runs, err)
	}
}

func TestReconcileStoreLagRepairsOnlyJournalledTerminalOperations(t *testing.T) {
	_, store := openJournal(t)
	snapshot := RunSnapshot{Operations: map[string]RunOperation{}}
	for _, id := range []string{"done", "ongoing", "unrecorded"} {
		op := RunOperation{ID: id, RunID: "r", IdempotencyKey: id, State: Running}
		if _, ok, err := store.PutOperation(op, 0); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if id == "done" {
			op.State = Succeeded
		}
		if id != "unrecorded" {
			snapshot.Operations[id] = op
		}
	}
	engine := EngineeringRuntime{deps: Dependencies{Store: store}, scheduler: Scheduler{Store: store, Clock: RealClock{}}}
	state := &runState{run: newJournalRun("r"), snapshot: snapshot}
	for i := 0; i < 2; i++ {
		if err := engine.reconcileStoreLag(state); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"done", "ongoing", "unrecorded"} {
		op, _, _, err := store.Operation(id)
		if err != nil {
			t.Fatal(err)
		}
		want := Running
		if id == "done" {
			want = Succeeded
		}
		if op.State != want {
			t.Fatalf("%s: got %s, want %s", id, op.State, want)
		}
	}
}
