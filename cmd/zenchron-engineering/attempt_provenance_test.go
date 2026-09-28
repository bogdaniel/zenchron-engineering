package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// #327: `autonomy status` shows how the latest execution attempt ran and ended
// - in text and JSON - and says so explicitly when that attempt recorded none.
func TestStatusSurfacesTheLatestAttemptProvenance(t *testing.T) {
	engine := &scriptedRuntime{
		runID: "run-1",
		report: runtime.StatusReport{
			SchemaVersion: runtime.SchemaVersion, RunID: "run-1", Repository: "zenchron/seeded",
			Phase: runtime.Execute, Disposition: runtime.Failed, Reason: "run_wall_budget_exhausted",
			Attempts: map[string]int{runtime.OpExecutionInvoke: 1},
			ExecutionAttemptProvenance: &runtime.ExecutionAttemptProvenance{
				OperationID: "op-invoke", AttemptIdentity: 1,
				Invocation: runtime.InvocationProvenance{AgentID: "claude", InvocationObservation: domain.InvocationObservation{
					Executable: "claude", PermissionMode: "acceptEdits",
					TerminationCause: "deadline_reached", ProgressMode: "structured_claude_events",
					InactivityLimit: 10 * time.Minute, StructuredEvents: 202, OpenToolsAtExit: 1,
					PermissionDenials: 6, PermissionDeniedTools: []string{"Bash", "WebFetch"},
				}},
			},
		},
	}
	var text bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"termination=deadline_reached", "progress_mode=structured_claude_events", "inactivity_limit=10m0s",
		"structured_events=202", "open_tools_at_exit=1", "permission_denials=6", "Bash,WebFetch", "permission=acceptEdits",
	} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("the text status does not surface %q:\n%s", want, text.String())
		}
	}
	var out bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1"}, autonomyOverrides{Runtime: engine}, &out); err != nil {
		t.Fatal(err)
	}
	var view struct {
		Provenance *runtime.ExecutionAttemptProvenance `json:"execution_attempt_provenance"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil || view.Provenance == nil ||
		view.Provenance.Invocation.TerminationCause != "deadline_reached" {
		t.Fatalf("the JSON status does not carry the provenance (%v):\n%s", err, out.String())
	}

	// ABSENCE IS STATED, NOT ZERO-FILLED.
	engine.report.ExecutionAttemptProvenance = nil
	var none bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &none); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), "no invocation provenance recorded") || strings.Contains(none.String(), "structured_events=0") {
		t.Fatalf("an attempt without provenance was not stated as such:\n%s", none.String())
	}
}
