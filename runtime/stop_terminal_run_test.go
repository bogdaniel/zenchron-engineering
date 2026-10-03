package runtime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// seedTerminal journals a run that ended with eventType. When settleRow is
// false the run row still reads active: the window between recordDisposition's
// append and its PutRun, where only the journal knows the run is over.
func seedTerminal(t *testing.T, store *SQLiteOperationStore, id, eventType string, disposition Disposition, settleRow bool) {
	t.Helper()
	run := newJournalRun(id)
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"reason": "outcome-" + id})
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id + "-end", RunID: id, Type: eventType, OccurredAt: run.CreatedAt, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if settleRow {
		run.Disposition, run.Reason = disposition, "outcome-"+id
		if err := store.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
}

func assertUntouched(t *testing.T, store *SQLiteOperationStore, id string, want Disposition) {
	t.Helper()
	run, _, err := store.Run(id)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	if run.Disposition == Cancelled || snapshot.Disposition != want || snapshot.Reason != "outcome-"+id || countType(events, EventRunCancelled) != 0 {
		t.Fatalf("%s: row=%q replay=%q/%q events=%v, want %q kept and no run.cancelled", id, run.Disposition, snapshot.Disposition, snapshot.Reason, journalTypes(events), want)
	}
}

// TestStopNeverRewritesATerminalOutcome is #439: stopping a completed or failed
// run is refused and writes nothing, including when only the journal - not yet
// the row - knows the run is over.
func TestStopNeverRewritesATerminalOutcome(t *testing.T) {
	_, store := openJournal(t)
	c := &fakeClock{now: time.Unix(200, 0)}
	scheduler := Scheduler{Store: store, Clock: c, Owner: "op"}
	for _, tc := range []struct {
		id        string
		event     string
		want      Disposition
		settleRow bool
	}{
		{"done", EventRunCompleted, Completed, true},
		{"broken", EventRunFailed, Failed, true},
		{"done-lagging", EventRunCompleted, Completed, false},
		{"broken-lagging", EventRunFailed, Failed, false},
	} {
		seedTerminal(t, store, tc.id, tc.event, tc.want, tc.settleRow)
		_, err := CancelRun(store, scheduler, c.Now(), tc.id, "operator/stop")
		var refused *RunTerminalError
		if !errors.As(err, &refused) || refused.Disposition != tc.want {
			t.Fatalf("%s: err=%v, want a RunTerminalError naming %q", tc.id, err, tc.want)
		}
		assertUntouched(t, store, tc.id, tc.want)
	}
}

// TestStopAllOverAMixedFleetCancelsOnlyTheActive keeps the bulk stop honest:
// the active run is cancelled, the finished ones keep their outcomes.
func TestStopAllOverAMixedFleetCancelsOnlyTheActive(t *testing.T) {
	_, store := openJournal(t) // seeds active run "r"
	seedTerminal(t, store, "done", EventRunCompleted, Completed, true)
	seedTerminal(t, store, "broken", EventRunFailed, Failed, true)
	c := &fakeClock{now: time.Unix(200, 0)}
	supervisor := &Supervisor{deps: SupervisorDependencies{Store: store, Clock: c, Owner: "op"}}
	outcomes, err := supervisor.StopAll("operator_stop_all")
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].RunID != "r" || outcomes[0].Disposition != Cancelled {
		t.Fatalf("stop-all outcomes = %+v, want only r cancelled", outcomes)
	}
	if run, _, _ := store.Run("r"); run.Disposition != Cancelled {
		t.Fatalf("the active run was not cancelled: %q", run.Disposition)
	}
	assertUntouched(t, store, "done", Completed)
	assertUntouched(t, store, "broken", Failed)
}

// TestStopAllSkipsARunThatFinishesAfterTheListing: "b" has journalled
// run.completed but its row still reads active, so StopAll lists it and its
// CancelRun is refused. That refusal must not abort the bulk stop: "a" and "c"
// are still cancelled, b keeps its outcome, and StopAll reports no error.
func TestStopAllSkipsARunThatFinishesAfterTheListing(t *testing.T) {
	_, store := openJournal(t)
	for _, id := range []string{"a", "c"} {
		if err := store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	seedTerminal(t, store, "b", EventRunCompleted, Completed, false)
	c := &fakeClock{now: time.Unix(200, 0)}
	supervisor := &Supervisor{deps: SupervisorDependencies{Store: store, Clock: c, Owner: "op"}}
	if _, err := supervisor.StopAll("operator_stop_all"); err != nil {
		t.Fatalf("a run finishing after the listing aborted stop-all: %v", err)
	}
	for _, id := range []string{"a", "c"} {
		if run, _, _ := store.Run(id); run.Disposition != Cancelled {
			t.Fatalf("%s was not cancelled: %q", id, run.Disposition)
		}
	}
	assertUntouched(t, store, "b", Completed)
}
