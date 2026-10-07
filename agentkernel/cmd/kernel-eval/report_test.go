package main

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestSummarizeKeepsFullDenominatorsAndUnknowns(t *testing.T) {
	known := api.Count(40)
	obs := []observation{
		{Task: "a", Mode: "baseline", Outcome: "completed", ExpectedMatched: true, Verified: true, WallMillis: 1,
			Reported: api.TokenUsage{Input: known}, Unknowns: []string{"cost"}},
		{Task: "b", Mode: "baseline", HarnessError: "fixture missing", Unknowns: nil},
		{Task: "a", Mode: "cold", Outcome: "failed", ExpectedMatched: false, WallMillis: 3,
			Unknowns: []string{"cost", "reported.input"}},
		{Task: "h", Mode: "cold", HeldOut: true, Outcome: "completed", ExpectedMatched: true, Verified: false,
			WallMillis: 2, Unknowns: []string{"reported.input"}},
	}
	total, groups, unknowns, failures := summarize(6, obs)

	if total.Planned != 6 || total.Attempted != 4 || total.Executed != 3 || total.HarnessErrors != 1 ||
		total.Verified != 1 || total.VerificationFailed != 2 {
		t.Fatalf("denominators %+v", total)
	}
	if total.Outcomes["completed"] != 2 || total.Outcomes["failed"] != 1 {
		t.Fatalf("outcomes %v", total.Outcomes)
	}
	if len(unknowns) != 2 || unknowns[0] != (unknownCount{"cost", 2}) || unknowns[1] != (unknownCount{"reported.input", 2}) {
		t.Fatalf("unknowns %+v", unknowns)
	}
	if len(failures) != 3 {
		t.Fatalf("want harness error, unexpected termination and failed check listed, got %+v", failures)
	}
	base := groups["baseline"].HeldIn
	if base.Denominators.Attempted != 2 || base.Denominators.Executed != 1 {
		t.Fatalf("baseline group %+v", base.Denominators)
	}
	// The harness-error run is in the denominator but not in any spread.
	if base.ReportedInputTokens.N != 1 || base.ReportedInputTokens.Unknown != 0 || base.ReportedInputTokens.Median != 40 {
		t.Fatalf("baseline reported tokens %+v", base.ReportedInputTokens)
	}
	if base.IndexOpenMillis != nil {
		t.Fatal("index cost reported for the baseline, where it does not apply")
	}
	cold := groups["cold"]
	if cold.HeldIn.ReportedInputTokens.N != 0 || cold.HeldIn.ReportedInputTokens.Unknown != 1 {
		t.Fatalf("unknown reported tokens must count as unknown, not zero: %+v", cold.HeldIn.ReportedInputTokens)
	}
	if cold.HeldOut.Denominators.Attempted != 1 || cold.HeldIn.Denominators.Attempted != 1 {
		t.Fatal("held-out runs must be summarized separately")
	}
}

func TestSpreadMedian(t *testing.T) {
	if s := spreadOf([]float64{5, 1, 3}, 0); s.Min != 1 || s.Median != 3 || s.Max != 5 || s.N != 3 {
		t.Fatalf("odd spread %+v", s)
	}
	if s := spreadOf([]float64{4, 1, 3, 2}, 2); s.Median != 2.5 || s.Unknown != 2 {
		t.Fatalf("even spread %+v", s)
	}
	if s := spreadOf(nil, 3); s.N != 0 || s.Unknown != 3 {
		t.Fatalf("empty spread %+v", s)
	}
}

// TestCorpusVerifiesAndDetectsUnexpected runs the real corpus once in every
// mode, then again with one expectation altered, which must be reported.
func TestCorpusVerifiesAndDetectsUnexpected(t *testing.T) {
	c, err := loadCorpus("../../testdata/eval")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := evaluate(context.Background(), &harness{c: c, scratch: t.TempDir()}, 1, allModes)
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Denominators
	if d.Attempted != len(c.Tasks)*len(allModes) || d.Verified != d.Attempted || len(rep.Failures) != 0 {
		t.Fatalf("denominators %+v failures %+v", d, rep.Failures)
	}
	c.Tasks[0].Expect.Outcome = api.OutcomeFailed
	rep, err = evaluate(context.Background(), &harness{c: c, scratch: t.TempDir()}, 1, []mode{modeBaseline})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Denominators.VerificationFailed != 1 || len(rep.Failures) != 1 || rep.Failures[0].Task != c.Tasks[0].ID {
		t.Fatalf("altered expectation not reported: %+v %+v", rep.Denominators, rep.Failures)
	}
}
