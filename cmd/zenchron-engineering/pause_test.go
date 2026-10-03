package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestServeAppliesPauseUnderTheControllerRole is the delegated path's server
// half: the endpoint handler serve installs. Without the controller role it
// applies nothing; with it, pause and unpause are journalled; a terminal run
// is refused.
func TestServeAppliesPauseUnderTheControllerRole(t *testing.T) {
	dir, configPath := watchWorkspace(t)
	runID := "run-delegated"
	stateDir := activeRun(t, configPath, dir, runID)
	built, err := newComposition(autonomyFlags{Config: configPath}, offlineOverrides())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	control := func(command string) runtime.ControlResponse {
		return built.handleControl(context.Background(), nil, func() {}, runtime.ControlRequest{
			Command: command, RunID: runID, Reason: "delegated", Operator: "requester"})
	}
	before := len(journalOf(t, stateDir, runID))

	if response := control(runtime.ControlPause); response.OK || !strings.Contains(response.Error, "controller role") {
		t.Fatalf("a process without the controller role applied a pause: %+v", response)
	}
	if len(journalOf(t, stateDir, runID)) != before {
		t.Fatal("a refused pause was journalled")
	}

	role, err := runtime.AcquireControllerRole(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = role.Release() })
	built.role = role
	response := control(runtime.ControlPause)
	var view runtime.PauseView
	if !response.OK || json.Unmarshal(response.Payload, &view) != nil || !view.Paused || view.Operator != "requester" || view.Reason != "delegated" {
		t.Fatalf("the delegated pause: %+v", response)
	}
	if events := journalOf(t, stateDir, runID); events[len(events)-1].Type != runtime.EventRunPaused {
		t.Fatal("the delegated pause did not journal run.paused")
	}
	if response := control(runtime.ControlUnpause); !response.OK || !strings.Contains(string(response.Payload), `"paused":false`) {
		t.Fatalf("the delegated unpause: %+v", response)
	}

	if _, err := cancelRun(built, runID, stopReason); err != nil {
		t.Fatal(err)
	}
	stopped := len(journalOf(t, stateDir, runID))
	if response := control(runtime.ControlPause); response.OK || !strings.Contains(response.Error, "nothing to pause") {
		t.Fatalf("a cancelled run was paused through the endpoint: %+v", response)
	}
	if len(journalOf(t, stateDir, runID)) != stopped {
		t.Fatal("a refused pause on a cancelled run was journalled")
	}
}

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
