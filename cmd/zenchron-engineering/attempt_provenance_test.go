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
	devNull := "/dev/null"
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
					FinalResultObserved: true, PermissionDenials: 6, PermissionDeniedTools: []string{"Bash", "WebFetch"},
					ProviderEnvironment: []domain.EnvironmentEntry{
						{Name: "GOENV", Value: &devNull}, {Name: "GOFLAGS", Value: new(string)}, {Name: "GOCACHE"},
						{Name: "HOME", SHA256: strings.Repeat("ab", 32)},
					},
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
		// #391: set, set-to-empty and absent stay three distinct facts.
		`GOENV="/dev/null"`, `GOFLAGS=""`, "GOCACHE (absent)", "HOME=sha256:abababababab",
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

	// NO FINAL RESULT: the denial count is UNKNOWN, never 0 (#324's shape).
	deadline := engine.report.ExecutionAttemptProvenance.Invocation.InvocationObservation
	deadline.FinalResultObserved, deadline.PermissionDenials, deadline.PermissionDeniedTools = false, 0, nil
	engine.report.ExecutionAttemptProvenance.Invocation.InvocationObservation = deadline
	var unknown bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &unknown); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unknown.String(), "permission_denials=unknown (no final result)") || strings.Contains(unknown.String(), "permission_denials=0") {
		t.Fatalf("an unknown denial count was not rendered as unknown:\n%s", unknown.String())
	}

	// A provider without the structured stream reports no denials at all: the
	// count is NOT APPLICABLE, decided from the recorded progress mode.
	byteOutput := engine.report.ExecutionAttemptProvenance.Invocation.InvocationObservation
	byteOutput.ProgressMode = "byte_output"
	engine.report.ExecutionAttemptProvenance.Invocation.InvocationObservation = byteOutput
	var notApplicable bytes.Buffer
	if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &notApplicable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notApplicable.String(), "permission_denials=n/a (provider reports none)") {
		t.Fatalf("a byte_output provider's denials were not rendered as not applicable:\n%s", notApplicable.String())
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
