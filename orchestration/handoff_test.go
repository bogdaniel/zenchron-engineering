package orchestration

import (
	"strings"
	"testing"
)

const validReport = `{"schema_version":"0.1","outcome":"completed","summary":"Implemented the change and added a regression test.","unresolved":[],"recommended_next":["review the error wording"]}`

func TestAValidHandoffReportDecodes(t *testing.T) {
	report, err := DecodeHandoffReport([]byte(validReport))
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeCompleted || len(report.RecommendedNext) != 1 {
		t.Fatalf("decoded %+v", report)
	}
	partial := `{"schema_version":"0.1","outcome":"partial","summary":"Half done.","unresolved":["the migration"]}`
	if _, err := DecodeHandoffReport([]byte(partial)); err != nil {
		t.Fatalf("a partial report naming what is unresolved was refused: %v", err)
	}
}

// TestAnInvalidHandoffReportIsRefused is #470 acceptance H: every way a
// worker-written document can fail to be one answer this build understands is
// refused, never repaired or truncated.
func TestAnInvalidHandoffReportIsRefused(t *testing.T) {
	many := `"` + strings.Repeat(`x","`, maxHandoffListItems) + `x"`
	for name, document := range map[string]string{
		"malformed json":        `{"schema_version":"0.1",`,
		"not an object":         `["completed"]`,
		"trailing data":         validReport + ` {}`,
		"unknown member":        `{"schema_version":"0.1","outcome":"completed","summary":"s","verdict":"accept"}`,
		"restated run identity": `{"schema_version":"0.1","outcome":"completed","summary":"s","run_id":"run-x"}`,
		"restated candidate":    `{"schema_version":"0.1","outcome":"completed","summary":"s","candidate":"abc"}`,
		"wrong schema version":  `{"schema_version":"0.2","outcome":"completed","summary":"s"}`,
		"absent schema version": `{"outcome":"completed","summary":"s"}`,
		"invalid outcome":       `{"schema_version":"0.1","outcome":"done","summary":"s"}`,
		"absent outcome":        `{"schema_version":"0.1","summary":"s"}`,
		"empty summary":         `{"schema_version":"0.1","outcome":"completed","summary":"  "}`,
		"oversized summary":     `{"schema_version":"0.1","outcome":"completed","summary":"` + strings.Repeat("s", maxSummaryBytes+1) + `"}`,
		"over-bound list":       `{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":[` + many + `]}`,
		"oversized list item":   `{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":["` + strings.Repeat("r", maxHandoffItemBytes+1) + `"]}`,
		"empty list item":       `{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":[""]}`,
		"completed+unresolved":  `{"schema_version":"0.1","outcome":"completed","summary":"s","unresolved":["x"]}`,
		"partial+nothing open":  `{"schema_version":"0.1","outcome":"partial","summary":"s"}`,
		"oversized document":    `{"schema_version":"0.1","outcome":"completed","summary":"s"}` + strings.Repeat(" ", MaxHandoffReportBytes),
	} {
		if _, err := DecodeHandoffReport([]byte(document)); err == nil {
			t.Errorf("%s: an invalid handoff report was admitted", name)
		}
	}
}

func TestBatchIdentityIsDeterministicAndExplicit(t *testing.T) {
	first, err := BatchID("Acme/Repo", "claude", []int{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BatchID("acme/repo", "claude", []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same request named two batches: %s and %s", first, second)
	}
	other, err := BatchID("acme/repo", "codex", []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("a different agent named the same batch")
	}
	for name, issues := range map[string][]int{
		"empty": nil, "duplicate": {4, 4}, "zero": {0}, "negative": {-2},
		"over bound": make([]int, MaxBatchItems+1),
	} {
		if _, err := NormalizeIssues(issues); err == nil {
			t.Errorf("%s: an invalid issue list was accepted", name)
		}
	}
}

func TestItemStateIsAProjectionThatFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		facts ChildFacts
		want  ItemState
	}{
		{ChildFacts{}, ItemNotCreated},
		{ChildFacts{true, RunLive, ActivityIdle, HandoffNone, ""}, ItemQueued},
		{ChildFacts{true, RunLive, ActivityWorking, HandoffNone, ""}, ItemRunning},
		{ChildFacts{true, RunLive, ActivityWaiting, HandoffNone, ""}, ItemWaiting},
		{ChildFacts{true, RunLive, ActivityIdle, HandoffReported, ""}, ItemQueued},
		{ChildFacts{true, RunLive, ActivityIdle, HandoffRefused, ""}, ItemQueued},
		{ChildFacts{true, RunLive, ActivityWorking, HandoffAdmitted, OutcomeCompleted}, ItemRunning},
		// A valid, admitted transfer of PARTIAL work is not completed work.
		{ChildFacts{true, RunLive, ActivityFinished, HandoffAdmitted, OutcomePartial}, ItemPartial},
		{ChildFacts{true, RunCompleted, ActivityIdle, HandoffAdmitted, OutcomePartial}, ItemPartial},
		// A provider that succeeded and a run that ended are still not a
		// completed item without an admitted handoff.
		{ChildFacts{true, RunCompleted, ActivityIdle, HandoffNone, ""}, ItemHandoffPending},
		{ChildFacts{true, RunCompleted, ActivityIdle, HandoffReported, ""}, ItemHandoffPending},
		{ChildFacts{true, RunCompleted, ActivityIdle, HandoffAdmitted, OutcomeCompleted}, ItemCompleted},
		{ChildFacts{true, RunFailed, ActivityIdle, HandoffAdmitted, OutcomeCompleted}, ItemFailed},
		{ChildFacts{true, RunCancelled, ActivityIdle, HandoffNone, ""}, ItemStopped},
	} {
		got, err := ProjectItem(tc.facts)
		if err != nil || got != tc.want {
			t.Errorf("%+v projected %q (%v), want %q", tc.facts, got, err, tc.want)
		}
	}
	for _, facts := range []ChildFacts{
		{true, "paused?", ActivityIdle, HandoffNone, ""},
		{true, RunLive, "busy", HandoffNone, ""},
		{true, RunLive, ActivityIdle, "maybe", ""},
		{true, RunLive, ActivityIdle, HandoffAdmitted, "done"},
	} {
		if state, err := ProjectItem(facts); err == nil {
			t.Errorf("%+v projected %q from an unrecognized fact", facts, state)
		}
	}
}
