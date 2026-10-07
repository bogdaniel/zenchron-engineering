package orchestration

import (
	"errors"
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
// refused, never repaired or truncated. #492 adds the deterministic class of
// each refusal: a protocol mistake a result-only correction may address, or a
// bound that is never handed back to a model.
func TestAnInvalidHandoffReportIsRefused(t *testing.T) {
	many := `"` + strings.Repeat(`x","`, maxHandoffListItems) + `x"`
	for name, tc := range map[string]struct {
		document string
		defect   HandoffDefect
	}{
		"malformed json":        {`{"schema_version":"0.1",`, DefectProtocol},
		"not an object":         {`["completed"]`, DefectProtocol},
		"trailing data":         {validReport + ` {}`, DefectProtocol},
		"unknown member":        {`{"schema_version":"0.1","outcome":"completed","summary":"s","verdict":"accept"}`, DefectProtocol},
		"restated run identity": {`{"schema_version":"0.1","outcome":"completed","summary":"s","run_id":"run-x"}`, DefectProtocol},
		"restated candidate":    {`{"schema_version":"0.1","outcome":"completed","summary":"s","candidate":"abc"}`, DefectProtocol},
		"wrong schema version":  {`{"schema_version":"0.2","outcome":"completed","summary":"s"}`, DefectProtocol},
		"absent schema version": {`{"outcome":"completed","summary":"s"}`, DefectProtocol},
		"invalid outcome":       {`{"schema_version":"0.1","outcome":"done","summary":"s"}`, DefectProtocol},
		"absent outcome":        {`{"schema_version":"0.1","summary":"s"}`, DefectProtocol},
		"empty summary":         {`{"schema_version":"0.1","outcome":"completed","summary":"  "}`, DefectProtocol},
		"empty list item":       {`{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":[""]}`, DefectProtocol},
		"completed+unresolved":  {`{"schema_version":"0.1","outcome":"completed","summary":"s","unresolved":["x"]}`, DefectProtocol},
		"partial+nothing open":  {`{"schema_version":"0.1","outcome":"partial","summary":"s"}`, DefectProtocol},
		"oversized summary":     {`{"schema_version":"0.1","outcome":"completed","summary":"` + strings.Repeat("s", maxSummaryBytes+1) + `"}`, DefectBound},
		"over-bound list":       {`{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":[` + many + `]}`, DefectBound},
		"oversized list item":   {`{"schema_version":"0.1","outcome":"completed","summary":"s","recommended_next":["` + strings.Repeat("r", maxHandoffItemBytes+1) + `"]}`, DefectBound},
		"oversized document":    {`{"schema_version":"0.1","outcome":"completed","summary":"s"}` + strings.Repeat(" ", MaxHandoffReportBytes), DefectBound},
		"not utf-8":             {"{\"schema_version\":\"0.1\",\"outcome\":\"done\",\"summary\":\"\xff\"}", DefectBound},
	} {
		_, err := DecodeHandoffReport([]byte(tc.document))
		if err == nil {
			t.Errorf("%s: an invalid handoff report was admitted", name)
			continue
		}
		var refusal *HandoffRefusal
		if !errors.As(err, &refusal) || refusal.Defect != tc.defect {
			t.Errorf("%s: refusal %v classified %+v, want %s", name, err, refusal, tc.defect)
		}
		if ProtocolRepairable(err) != (tc.defect == DefectProtocol) {
			t.Errorf("%s: repairable=%t for a %s refusal", name, ProtocolRepairable(err), tc.defect)
		}
	}
	if ProtocolRepairable(errors.New("the slot holds a symlink")) {
		t.Error("an error the decoder did not raise was classified repairable")
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
