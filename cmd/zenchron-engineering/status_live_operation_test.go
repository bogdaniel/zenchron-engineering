package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// #326: the lease line of a running operation is the live row's lease that
// status matched to this attempt, not the one the journal stamped at start.
func TestLeaseLineOfARunningOperationIsTheRowLease(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	beat := started.Add(20 * time.Minute)
	journalled, err := json.Marshal(runtime.RunOperation{ID: "op-1", Lease: &runtime.Lease{Owner: "journal-owner", HeartbeatAt: started, ExpiresAt: started}})
	if err != nil {
		t.Fatal(err)
	}
	events := []runtime.EngineeringEvent{{Type: runtime.EventOperationBefore, OperationID: "op-1", Payload: journalled}}
	report := runtime.StatusReport{Operation: &runtime.OperationStatus{
		ID: "op-1", State: runtime.Running, ProgressSource: "row",
		Lease: &runtime.Lease{Owner: "row-owner", HeartbeatAt: beat, ExpiresAt: beat.Add(time.Minute)},
	}}
	lease := leaseOf(events, report)
	if lease == nil || lease.Owner != "row-owner" || !lease.HeartbeatAt.Equal(beat) {
		t.Fatalf("lease = %#v, want the row's", lease)
	}
	// A journal fallback still reads the journal, and the text says so.
	report.Operation.ProgressSource, report.Operation.Lease = "journal", nil
	report.Operation.InactivityLimit = 10 * time.Minute
	lease = leaseOf(events, report)
	if lease == nil || lease.Owner != "journal-owner" {
		t.Fatalf("fallback lease = %#v, want the journal's", lease)
	}
	var out bytes.Buffer
	if err := renderStatusText(&out, statusView{StatusReport: report, Lease: lease}); err != nil {
		t.Fatal(err)
	}
	// No recorded progress: silence is unknown, never a reassuring 0s.
	for _, want := range []string{"progress (journal):", "lease (journal):", "last none recorded silent unknown inactivity limit"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("text does not mark the journal source %q:\n%s", want, out.String())
		}
	}
}

// #352: while a structured tool holds the window, the text says so and never
// presents silence past the limit as a pending kill; the absolute deadline
// stays on its own line.
func TestASuspendedWindowIsNotRenderedAsAnImpendingKill(t *testing.T) {
	last := time.Unix(1_800_000_000, 0).UTC()
	deadline := last.Add(30 * time.Minute)
	operation := &runtime.OperationStatus{
		ID: "op-1", State: runtime.Running, ProgressSource: "row",
		LastProgressAt: &last, SilentFor: 25 * time.Minute, InactivityLimit: 10 * time.Minute,
		InactivitySuspension: "active", InactivitySuspendedSince: &last, Deadline: &deadline, DeadlineBound: runtime.AttemptBound("attempt_wall"),
	}
	var out bytes.Buffer
	if err := renderStatusText(&out, statusView{StatusReport: runtime.StatusReport{Operation: operation}}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"inactivity suspended — structured tool open since " + last.Format(time.RFC3339),
		deadline.Format(time.RFC3339) + " (attempt_wall)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "silent 25m0s") {
		t.Fatalf("a suspended window was rendered as silence against the limit:\n%s", text)
	}
	// A suspension whose owner's liveness cannot be established is not
	// presented as one: the silence stands, and the record is marked.
	operation.InactivitySuspension = "unverified"
	out.Reset()
	if err := renderStatusText(&out, statusView{StatusReport: runtime.StatusReport{Operation: operation}}); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "silent 25m0s inactivity limit 10m0s") ||
		!strings.Contains(text, "unverified — a structured tool was recorded open since "+last.Format(time.RFC3339)) ||
		strings.Contains(text, "inactivity suspended") {
		t.Fatalf("an unverified suspension was rendered as active:\n%s", text)
	}
	operation.InactivitySuspension, operation.InactivitySuspendedSince = "", nil
	out.Reset()
	if err := renderStatusText(&out, statusView{StatusReport: runtime.StatusReport{Operation: operation}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "silent 25m0s inactivity limit 10m0s") {
		t.Fatalf("an unsuspended window lost its silence line:\n%s", out.String())
	}
}

// #355: `autonomy status --text` for a continuation whose report carries no
// suspension - which runtime's TestAContinuationsStatusInheritsNoSuspension
// proves the real Status returns - says nothing about one, while the
// predecessor's report, suspended, does.
func TestAContinuationsStatusTextShowsNoInheritedSuspension(t *testing.T) {
	last := time.Unix(1_800_000_000, 0).UTC()
	report := func(opID string, suspension string, since *time.Time) *scriptedRuntime {
		return &scriptedRuntime{runID: "run-1", report: runtime.StatusReport{
			SchemaVersion: runtime.SchemaVersion, RunID: "run-1", Repository: "zenchron/seeded",
			Phase: runtime.Execute, Disposition: runtime.Active,
			Operation: &runtime.OperationStatus{
				ID: opID, Kind: runtime.OpExecutionInvoke, State: runtime.Running, ProgressSource: "row",
				LastProgressAt: &last, SilentFor: time.Minute, InactivityLimit: 10 * time.Minute,
				InactivitySuspension: suspension, InactivitySuspendedSince: since,
			},
		}}
	}
	render := func(engine *scriptedRuntime) string {
		var text bytes.Buffer
		if _, err := autonomy([]string{"status", "run-1", "--text"}, autonomyOverrides{Runtime: engine}, &text); err != nil {
			t.Fatal(err)
		}
		return text.String()
	}
	if text := render(report("op-predecessor", "active", &last)); !strings.Contains(text, "inactivity suspended") {
		t.Fatalf("the predecessor's open tool is not rendered:\n%s", text)
	}
	if text := render(report("op-continuation", "", nil)); strings.Contains(text, "inactivity suspended") ||
		!strings.Contains(text, "silent 1m0s inactivity limit 10m0s") {
		t.Fatalf("the continuation's text claims a suspension it does not have:\n%s", text)
	}
}
