package benchmark

import (
	"testing"
	"time"
)

func dataset() Input {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := Input{SchemaVersion: Version, Benchmark: "zenchron-leverage", CorpusVersion: "serve-v1", MinimumPerClass: 3}
	isolated := false
	for n := 0; n < 3; n++ {
		for _, mode := range []string{"direct_agent", "zenchron_explicit"} {
			offset := time.Duration(n*2) * time.Hour
			if mode == "zenchron_explicit" {
				offset += time.Hour
			}
			start := start.Add(offset)
			window := Interval{start, start.Add(time.Hour)}
			c := Cohort{ID: string(rune('a' + n)), Mode: mode, Operator: "operator", Repository: "repo", Base: "sha", Controller: "sha", Agent: "agent", Provider: "provider", Model: "model", TrustMode: "sandbox", EngineeringVersion: "sha", Window: window, Cost: Cost{Reason: "subscription; unknown"}, FailureBlockedUnrelated: &isolated}
			duration := 30 * time.Minute
			if mode == "zenchron_explicit" {
				duration = 15 * time.Minute
			}
			c.Attention = []Attention{{Interval{start, start.Add(duration)}, "active_before_review"}}
			for j, cl := range Classes {
				a := start.Add(time.Duration(j) * 5 * time.Minute)
				if mode == "zenchron_explicit" {
					a = start
				}
				c.Tasks = append(c.Tasks, Task{ID: cl, Class: cl, Acceptance: "same acceptance", Worker: cl, Work: Interval{a, a.Add(5 * time.Minute)}, Accepted: true, FirstPass: true})
			}
			in.Cohorts = append(in.Cohorts, c)
		}
	}
	return in
}
func TestMeasuredRatio(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		ratio   float64
		gate    string
	}{{15, 2, "meets_observed_target"}, {30, 1, "below_target"}, {40, .75, "below_target"}} {
		in := dataset()
		for i := range in.Cohorts {
			c := &in.Cohorts[i]
			if c.Mode == "zenchron_explicit" {
				c.Attention[0].End = c.Window.Start.Add(time.Duration(tc.minutes) * time.Minute)
			}
		}
		r, err := Evaluate(in)
		if err != nil {
			t.Fatal(err)
		}
		if r.ExplicitRatio == nil || *r.ExplicitRatio != tc.ratio || r.Gate != tc.gate {
			t.Fatalf("%+v", r)
		}
		s := r.Modes["zenchron_explicit"]
		if s.WorkerOverlapMinutes != 15 || s.AttentionDuringOverlapMinutes != 15 {
			t.Fatalf("overlap %+v", s)
		}
	}
}
func TestInvalidAndUnproven(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*Input)
		invalid bool
	}{
		{"mismatch", func(in *Input) { in.Cohorts[1].Tasks[0].Acceptance = "easier" }, true},
		{"overlapping attention", func(in *Input) { c := &in.Cohorts[0]; c.Attention = append(c.Attention, c.Attention[0]) }, true},
		{"unknown cost", func(in *Input) { in.Cohorts[0].Cost.Reason = "" }, true},
		{"thin", func(in *Input) { in.Cohorts = in.Cohorts[:2] }, false},
		{"unsafe", func(in *Input) { in.Cohorts[1].Tasks[0].UnsafeAuthority = 1 }, false},
		{"missing baseline", func(in *Input) { in.Cohorts = in.Cohorts[1:] }, false},
		{"zero attention", func(in *Input) {
			for i := range in.Cohorts {
				in.Cohorts[i].Attention = []Attention{}
			}
		}, false},
		{"failure isolation unknown", func(in *Input) {
			for i := range in.Cohorts {
				in.Cohorts[i].FailureBlockedUnrelated = nil
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := dataset()
			tc.change(&in)
			r, err := Evaluate(in)
			if tc.invalid {
				if err == nil {
					t.Fatal("expected rejection")
				}
			} else if err != nil || r.Gate != "unproven" {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
