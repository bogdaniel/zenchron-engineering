package runtime

import (
	"encoding/json"
	"fmt"
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
