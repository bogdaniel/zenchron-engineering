package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestPauseCommandLocalPath drives `autonomy pause/unpause` with no
// supervisor: written locally, idempotent, resume refused with the unpause
// hint, and stop still wins (#86).
func TestPauseCommandLocalPath(t *testing.T) {
	dir, configPath := watchWorkspace(t)
	runID := "run-pausable"
	stateDir := activeRun(t, configPath, dir, runID)
	pause := func(args ...string) (int, string, error) {
		var out bytes.Buffer
		code, err := autonomy(append(args, "--config", configPath), offlineOverrides(), &out)
		return code, out.String(), err
	}

	code, out, err := pause("pause", runID, "--reason", "investigating dependency")
	if err != nil || code != runtime.ExitCompleted {
		t.Fatalf("pause: code=%d err=%v", code, err)
	}
	var view runtime.PauseView
	if err := json.Unmarshal([]byte(out), &view); err != nil || !view.Paused || view.Reason != "investigating dependency" || view.Operator == "" {
		t.Fatalf("pause reported %q (%v)", out, err)
	}
	events := journalOf(t, stateDir, runID)
	if events[len(events)-1].Type != runtime.EventRunPaused {
		t.Fatalf("pause did not journal run.paused: %+v", events[len(events)-1])
	}
	if _, _, err := pause("pause", runID, "--reason", "again"); err != nil || len(journalOf(t, stateDir, runID)) != len(events) {
		t.Fatalf("a repeated pause appended (%v)", err)
	}
	if code, _, err := pause("resume", runID); err == nil || code != runtime.ExitWaiting || !strings.Contains(err.Error(), "autonomy unpause "+runID) {
		t.Fatalf("resume walked over a pause: code=%d err=%v", code, err)
	}
	if run := runDocument(t, stateDir, runID); run.Disposition != runtime.Active {
		t.Fatalf("pause moved the disposition to %q", run.Disposition)
	}

	if code, out, err := pause("unpause", runID); err != nil || code != runtime.ExitCompleted || !strings.Contains(out, `"paused": false`) {
		t.Fatalf("unpause: code=%d out=%q err=%v", code, out, err)
	}
	if code, _, err := pause("stop", runID); err != nil || code != runtime.ExitCancelled {
		t.Fatalf("stop: code=%d err=%v", code, err)
	}
	if code, _, err := pause("pause", runID); err == nil || code != runtime.ExitInvalid {
		t.Fatalf("a cancelled run was paused: code=%d err=%v", code, err)
	}
	if code, _, err := pause("pause", "run-absent"); err == nil || code != exitRunNotFound {
		t.Fatalf("an unknown run: code=%d err=%v", code, err)
	}
}
