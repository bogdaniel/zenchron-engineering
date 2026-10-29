package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DURABLE PER-RUN PAUSE (#86). The frozen design is docs/spec/runtime-v0.1.md
// "Operator pause"; this is its whole runtime half.
//
// A pause is a fact in the run's journal and nothing else: run.paused and
// run.unpaused, the latest of the two wins. No run-row mirror, no column, no
// in-memory flag. It is ORTHOGONAL to the run's Disposition and Reason, which
// it never changes and never becomes.
//
// It is enforced in exactly two places, both reading that fact:
//
//   - AcquireOperation, inside the one statement that grants a lease
//     (runPausedSQL). That is the gate: a paused run gets no lease, so no
//     work and no observation, whatever a driver read beforehand.
//   - Reconcile, which returns a paused run unchanged before planning, so a
//     pass over it journals nothing. That is an optimization, never the gate.
//
// An operation leased before the pause is not interrupted: it finishes under
// its own bounds and is journalled, and the next acquisition is refused.

// The two pause events. Neither changes Disposition or Reason.
const (
	EventRunPaused   = "run.paused"
	EventRunUnpaused = "run.unpaused"
)

// RunPause is the pause in force on a run.
type RunPause struct {
	Since    time.Time `json:"since"`
	Reason   string    `json:"reason,omitempty"`
	Operator string    `json:"operator"`
}

// RunPausePayload is the payload of run.paused (reason and operator) and of
// run.unpaused (operator only). The operator is provenance, never authority.
type RunPausePayload struct {
	Reason   string `json:"reason,omitempty"`
	Operator string `json:"operator"`
}

// PauseView is what `autonomy pause` and `autonomy unpause` report. Settling
// names the operation leased before the pause that is still finishing.
type PauseView struct {
	RunID  string `json:"run_id"`
	Paused bool   `json:"paused"`
	*RunPause
	Settling *SettlingOperation `json:"settling,omitempty"`
}

// SettlingOperation is an operation that holds a lease on a paused run.
type SettlingOperation struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
}

// foldPause is the one replay rule: the latest of run.paused and run.unpaused
// wins, a repeated pause keeps the original, and nothing else moves it.
func foldPause(current *RunPause, e EngineeringEvent) *RunPause {
	switch {
	case e.Type == EventRunUnpaused:
		return nil
	case e.Type == EventRunPaused && current == nil:
		var p RunPausePayload
		_ = json.Unmarshal(e.Payload, &p) // validated at append
		return &RunPause{Since: e.OccurredAt, Reason: p.Reason, Operator: p.Operator}
	}
	return current
}

// JournalPause is the pause in force after events, or nil.
func JournalPause(events []EngineeringEvent) *RunPause {
	var pause *RunPause
	for _, e := range events {
		pause = foldPause(pause, e)
	}
	return pause
}

// runPausedSQL is true when the latest pause event of the run named by the
// SQL expression runID is run.paused. It is the ONE statement of "is this run
// paused" in SQL. The type literals are inline so the events_run_pause partial
// index applies.
func runPausedSQL(runID string) string {
	return `COALESCE((SELECT type FROM events
		WHERE stream_kind = 'run' AND run_id = ` + runID + ` AND type IN ('run.paused', 'run.unpaused')
		ORDER BY sequence DESC LIMIT 1), '') = 'run.paused'`
}

// RunPaused reads the pause fact durably, through the same predicate the
// acquisition statement uses.
func (s *SQLiteOperationStore) RunPaused(runID string) (bool, error) {
	var paused bool
	err := s.db.QueryRow(`SELECT `+runPausedSQL("?"), runID).Scan(&paused)
	return paused, err
}

// errPauseUnchanged is how the append reports that the run is already in the
// requested state, so nothing is appended. It never leaves this file.
var errPauseUnchanged = errors.New("pause unchanged")

// pauseTransition decides, inside the append transaction and against the
// events the append is ordered after, whether a pause or unpause may be
// journalled. A terminal run is refused; a run already in the requested
// state appends nothing, which is what makes both commands idempotent even
// when two of them race.
func pauseTransition(run EngineeringRun, existing []EngineeringEvent, eventType string) error {
	snapshot, err := Reduce(run, existing)
	if err != nil {
		return err
	}
	if terminalDisposition(snapshot.Disposition) {
		return &RunTerminalError{RunID: run.ID, Disposition: snapshot.Disposition, Reason: snapshot.Reason, Verb: strings.TrimPrefix(eventType, "run.")}
	}
	if (snapshot.Paused != nil) == (eventType == EventRunPaused) {
		return errPauseUnchanged
	}
	return nil
}

// PauseRun journals run.paused. Pausing a paused run appends nothing and
// reports the original pause; a terminal run is refused with a
// RunTerminalError. Nothing else is written: no disposition, no operation, no
// cancellation request.
func PauseRun(store *SQLiteOperationStore, now time.Time, runID, reason, operator string) (PauseView, error) {
	return changePause(store, now, runID, EventRunPaused, RunPausePayload{Reason: boundedField(reason), Operator: operator})
}

// UnpauseRun journals run.unpaused. Unpausing a run that is not paused
// appends nothing.
func UnpauseRun(store *SQLiteOperationStore, now time.Time, runID, operator string) (PauseView, error) {
	return changePause(store, now, runID, EventRunUnpaused, RunPausePayload{Operator: operator})
}

func changePause(store *SQLiteOperationStore, now time.Time, runID, eventType string, payload RunPausePayload) (PauseView, error) {
	if strings.TrimSpace(payload.Operator) == "" {
		return PauseView{}, fmt.Errorf("a %s records the operator who asked for it", eventType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return PauseView{}, err
	}
	// The id is the run, the verb and the instant - never the operator's text.
	if _, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            fmt.Sprintf("%s-%s-%d", runID, strings.TrimPrefix(eventType, "run."), now.UnixNano()),
		RunID:         runID, Type: eventType, OccurredAt: now, Payload: raw,
	}); err != nil && !errors.Is(err, errPauseUnchanged) {
		return PauseView{}, err
	}
	return PauseStatus(store, runID)
}

// PauseStatus replays the run's journal for its pause and reads the operation
// still settling under one. An unreadable journal or operation set is an
// error, never "not paused" and never "nothing settling".
func PauseStatus(store *SQLiteOperationStore, runID string) (PauseView, error) {
	run, found, err := store.Run(runID)
	if err != nil {
		return PauseView{}, err
	}
	if !found {
		return PauseView{}, fmt.Errorf("unknown run %q", runID)
	}
	events, err := store.Events(runID)
	if err != nil {
		return PauseView{}, err
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		return PauseView{}, err
	}
	view := PauseView{RunID: runID, Paused: snapshot.Paused != nil, RunPause: snapshot.Paused}
	if !view.Paused {
		return view, nil
	}
	operations, err := store.Operations(runID)
	if err != nil {
		return PauseView{}, err
	}
	for _, op := range operations {
		if op.Lease != nil && (op.State == Leased || op.State == Running) {
			view.Settling = &SettlingOperation{OperationID: op.ID, Kind: op.Kind}
		}
	}
	return view, nil
}
