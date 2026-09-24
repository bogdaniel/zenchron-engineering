// Package benchmark evaluates observed serve and direct-agent cohorts. It does
// not execute agents or infer human attention from runtime event counts.
package benchmark

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const Version = "1"

var Classes = []string{"trivial/documentation", "normal_behavior", "api_business_rule", "security_sensitive", "hidden_scope_expansion", "failing_tests_remediation", "review_feedback", "provider_wait", "base_drift_conflict", "concurrent_cohort"}

type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
type Attention struct {
	Interval
	Category string `json:"category"`
}
type Cost struct {
	Amount   *float64 `json:"amount"`
	Currency string   `json:"currency"`
	Reason   string   `json:"reason,omitempty"`
}
type Task struct {
	ID                     string   `json:"id"`
	Class                  string   `json:"class"`
	Acceptance             string   `json:"acceptance"`
	Worker                 string   `json:"worker"`
	Work                   Interval `json:"work"`
	Accepted               bool     `json:"accepted"`
	FirstPass              bool     `json:"first_pass"`
	ReviewCycles           int      `json:"review_cycles"`
	Rework                 int      `json:"rework"`
	EscapedDefects         int      `json:"escaped_defects"`
	UnsafeAuthority        int      `json:"unsafe_authority"`
	RelayActions           int      `json:"relay_actions"`
	Workarounds            int      `json:"workarounds"`
	AuthorityInterventions int      `json:"authority_interventions"`
	FalseBlocks            int      `json:"false_blocks"`
	// Authority counts are optional: absence means no ground truth.
	Authority *Confusion `json:"authority"`
}
type Confusion struct {
	TP int `json:"tp"`
	FP int `json:"fp"`
	FN int `json:"fn"`
}
type Cohort struct {
	ID                      string      `json:"id"`
	Mode                    string      `json:"mode"`
	Operator                string      `json:"operator"`
	Repository              string      `json:"repository"`
	Base                    string      `json:"base_revision"`
	Controller              string      `json:"controller"`
	Agent                   string      `json:"agent"`
	Provider                string      `json:"provider"`
	Model                   string      `json:"model"`
	TrustMode               string      `json:"trust_mode"`
	EngineeringVersion      string      `json:"engineering_version"`
	Window                  Interval    `json:"window"`
	Attention               []Attention `json:"attention"`
	ProviderCIWaitMinutes   float64     `json:"unattended_provider_ci_minutes"`
	Cost                    Cost        `json:"cost"`
	ProviderUsage           *int        `json:"provider_usage_tokens"`
	FailureBlockedUnrelated *bool       `json:"failure_blocked_unrelated"`
	Tasks                   []Task      `json:"tasks"`
}
type Input struct {
	SchemaVersion string `json:"schema_version"`
	Benchmark     string `json:"benchmark"`
	CorpusVersion string `json:"corpus_version"`
	// Repetitions must be predeclared, not selected after seeing results.
	MinimumPerClass int      `json:"minimum_per_class"`
	Cohorts         []Cohort `json:"cohorts"`
}
type Summary struct {
	ClassRepetitions              map[string]int     `json:"class_repetitions"`
	AuthorityGroundTruthTasks     int                `json:"authority_ground_truth_tasks"`
	ProviderUsageTokens           []*int             `json:"provider_usage_tokens_by_cohort"`
	Cohorts                       int                `json:"cohorts"`
	Tasks                         int                `json:"tasks"`
	Accepted                      int                `json:"accepted"`
	SupervisionMinutes            float64            `json:"supervision_minutes"`
	MinutesByCategory             map[string]float64 `json:"minutes_by_category"`
	AcceptedPerHour               *float64           `json:"accepted_per_supervision_hour"`
	NonAuthorityPerAccepted       *float64           `json:"non_authority_interventions_per_accepted"`
	FirstPassAcceptance           *float64           `json:"first_pass_acceptance"`
	AuthorityPrecision            *float64           `json:"authority_precision"`
	AuthorityRecall               *float64           `json:"authority_recall"`
	ReviewCycles                  int                `json:"review_cycles"`
	Rework                        int                `json:"rework"`
	EscapedDefects                int                `json:"escaped_defects"`
	UnsafeAuthority               int                `json:"unsafe_authority"`
	FalseBlocks                   int                `json:"false_blocks"`
	AuthorityInterventions        int                `json:"authority_interventions"`
	RelayActions                  int                `json:"relay_actions"`
	Workarounds                   int                `json:"workarounds"`
	CycleMinutes                  float64            `json:"cycle_minutes"`
	UnattendedWaitMinutes         float64            `json:"unattended_wait_minutes"`
	WorkerOverlapMinutes          float64            `json:"worker_overlap_minutes"`
	AttentionDuringOverlapMinutes float64            `json:"attention_during_overlap_minutes"`
	Costs                         []Cost             `json:"costs"`
}
type Report struct {
	SchemaVersion string              `json:"schema_version"`
	CorpusVersion string              `json:"corpus_version"`
	Modes         map[string]*Summary `json:"modes"`
	ExplicitRatio *float64            `json:"explicit_leverage_ratio"`
	Gate          string              `json:"gate"`
	Reasons       []string            `json:"reasons"`
}

func quotient(a, b float64) *float64 {
	if b <= 0 {
		return nil
	}
	v := a / b
	return &v
}
func minutes(i Interval) float64 { return i.End.Sub(i.Start).Minutes() }
func valid(i Interval) bool      { return !i.Start.IsZero() && i.End.After(i.Start) }
func contained(i, w Interval) bool {
	return valid(i) && !i.Start.Before(w.Start) && !i.End.After(w.End)
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func knownClass(s string) bool {
	for _, c := range Classes {
		if s == c {
			return true
		}
	}
	return false
}
func overlap(c Cohort) []Interval {
	points := []time.Time{}
	for _, t := range c.Tasks {
		points = append(points, t.Work.Start, t.Work.End)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })
	out := []Interval{}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if !b.After(a) {
			continue
		}
		workers := map[string]bool{}
		for _, t := range c.Tasks {
			if !t.Work.Start.After(a) && !t.Work.End.Before(b) {
				workers[t.Worker] = true
			}
		}
		if len(workers) >= 2 {
			out = append(out, Interval{a, b})
		}
	}
	return out
}

// Evaluate rejects incomplete or incomparable records rather than treating
// missing observations as zero. Attention intervals must be disjoint per cohort.
func Evaluate(in Input) (Report, error) {
	r := Report{SchemaVersion: Version, CorpusVersion: in.CorpusVersion, Modes: map[string]*Summary{}, Gate: "unproven", Reasons: []string{}}
	if in.SchemaVersion != Version || in.Benchmark != "zenchron-leverage" || in.CorpusVersion == "" || in.MinimumPerClass < 3 || len(in.Cohorts) == 0 {
		return r, fmt.Errorf("require version 1, zenchron-leverage, corpus version, minimum_per_class >= 3 and cohorts")
	}
	pairs := map[string]map[string]Cohort{}
	counts := map[string]map[string]int{}
	authorities := map[string]Confusion{}
	first := map[string]int{}
	for _, c := range in.Cohorts {
		if c.Mode != "direct_agent" && c.Mode != "zenchron_explicit" && c.Mode != "zenchron_planned" {
			return r, fmt.Errorf("unknown mode %q", c.Mode)
		}
		if c.ID == "" || c.Operator == "" || c.Repository == "" || c.Base == "" || c.Controller == "" || c.Agent == "" || c.Provider == "" || c.Model == "" || c.TrustMode == "" || c.EngineeringVersion == "" || !valid(c.Window) || c.Attention == nil || len(c.Tasks) == 0 || !finite(c.ProviderCIWaitMinutes) {
			return r, fmt.Errorf("incomplete cohort %q", c.ID)
		}
		if c.Cost.Amount == nil {
			if c.Cost.Reason == "" {
				return r, fmt.Errorf("unknown cost requires reason")
			}
		} else if !finite(*c.Cost.Amount) || c.Cost.Currency == "" {
			return r, fmt.Errorf("invalid cost")
		}
		if c.ProviderUsage != nil && *c.ProviderUsage < 0 {
			return r, fmt.Errorf("negative usage")
		}
		if pairs[c.ID] == nil {
			pairs[c.ID] = map[string]Cohort{}
		}
		if _, ok := pairs[c.ID][c.Mode]; ok {
			return r, fmt.Errorf("duplicate cohort/mode")
		}
		pairs[c.ID][c.Mode] = c
		for _, previous := range in.Cohorts {
			if previous.ID == c.ID && previous.Mode == c.Mode {
				break
			}
			if previous.Operator == c.Operator && previous.Window.Start.Before(c.Window.End) && c.Window.Start.Before(previous.Window.End) {
				return r, fmt.Errorf("operator observation windows overlap: %s/%s and %s/%s", previous.ID, previous.Mode, c.ID, c.Mode)
			}
		}
		s := r.Modes[c.Mode]
		if s == nil {
			s = &Summary{MinutesByCategory: map[string]float64{}}
			r.Modes[c.Mode] = s
			counts[c.Mode] = map[string]int{}
		}
		s.Cohorts++
		s.CycleMinutes += minutes(c.Window)
		s.UnattendedWaitMinutes += c.ProviderCIWaitMinutes
		s.Costs = append(s.Costs, c.Cost)
		s.ProviderUsageTokens = append(s.ProviderUsageTokens, c.ProviderUsage)
		intervals := append([]Attention(nil), c.Attention...)
		sort.Slice(intervals, func(i, j int) bool { return intervals[i].Start.Before(intervals[j].Start) })
		ov := overlap(c)
		for _, i := range ov {
			s.WorkerOverlapMinutes += minutes(i)
		}
		for j, a := range intervals {
			switch a.Category {
			case "active_before_review", "selection_assignment", "review", "relay", "workaround", "authority":
			default:
				return r, fmt.Errorf("unknown attention category %q", a.Category)
			}
			if !contained(a.Interval, c.Window) || (j > 0 && a.Start.Before(intervals[j-1].End)) {
				return r, fmt.Errorf("invalid or overlapping attention in %s", c.ID)
			}
			m := minutes(a.Interval)
			s.SupervisionMinutes += m
			s.MinutesByCategory[a.Category] += m
			for _, i := range ov {
				start, end := a.Start, a.End
				if i.Start.After(start) {
					start = i.Start
				}
				if i.End.Before(end) {
					end = i.End
				}
				if end.After(start) {
					s.AttentionDuringOverlapMinutes += end.Sub(start).Minutes()
				}
			}
		}
		ids := map[string]bool{}
		seenClasses := map[string]bool{}
		for _, t := range c.Tasks {
			if t.ID == "" || ids[t.ID] || !knownClass(t.Class) || t.Acceptance == "" || t.Worker == "" || !contained(t.Work, c.Window) || t.FirstPass && !t.Accepted {
				return r, fmt.Errorf("invalid task in %s", c.ID)
			}
			ids[t.ID] = true
			for _, n := range []int{t.ReviewCycles, t.Rework, t.EscapedDefects, t.UnsafeAuthority, t.RelayActions, t.Workarounds, t.AuthorityInterventions, t.FalseBlocks} {
				if n < 0 {
					return r, fmt.Errorf("negative count")
				}
			}
			s.Tasks++
			if !seenClasses[t.Class] {
				counts[c.Mode][t.Class]++
				seenClasses[t.Class] = true
			}
			if t.Accepted {
				s.Accepted++
			}
			if t.FirstPass {
				first[c.Mode]++
			}
			s.ReviewCycles += t.ReviewCycles
			s.Rework += t.Rework
			s.EscapedDefects += t.EscapedDefects
			s.UnsafeAuthority += t.UnsafeAuthority
			s.RelayActions += t.RelayActions
			s.Workarounds += t.Workarounds
			s.AuthorityInterventions += t.AuthorityInterventions
			s.FalseBlocks += t.FalseBlocks
			if t.Authority != nil {
				s.AuthorityGroundTruthTasks++
				a := t.Authority
				if a.TP < 0 || a.FP < 0 || a.FN < 0 {
					return r, fmt.Errorf("negative authority count")
				}
				v := authorities[c.Mode]
				v.TP += a.TP
				v.FP += a.FP
				v.FN += a.FN
				authorities[c.Mode] = v
			}
		}
	}
	for mode, s := range r.Modes {
		s.ClassRepetitions = counts[mode]
		s.AcceptedPerHour = quotient(float64(s.Accepted)*60, s.SupervisionMinutes)
		s.NonAuthorityPerAccepted = quotient(float64(s.RelayActions+s.Workarounds), float64(s.Accepted))
		s.FirstPassAcceptance = quotient(float64(first[mode]), float64(s.Tasks))
		a := authorities[mode]
		s.AuthorityPrecision = quotient(float64(a.TP), float64(a.TP+a.FP))
		s.AuthorityRecall = quotient(float64(a.TP), float64(a.TP+a.FN))
	}
	for _, m := range []string{"direct_agent", "zenchron_explicit"} {
		for _, cl := range Classes {
			if counts[m][cl] < in.MinimumPerClass {
				r.Reasons = append(r.Reasons, "thin sample: "+m+"/"+cl)
			}
		}
	}
	parallel := false
	for _, id := range sortedKeys(pairs) {
		p := pairs[id]
		d, dok := p["direct_agent"]
		z, zok := p["zenchron_explicit"]
		if !dok || !zok {
			r.Reasons = append(r.Reasons, "missing baseline pair: "+id)
			continue
		}
		if !comparable(d, z) {
			return r, fmt.Errorf("incomparable pair %s", id)
		}
		if len(overlap(z)) > 0 && sequential(d) && z.FailureBlockedUnrelated != nil && !*z.FailureBlockedUnrelated {
			parallel = true
		}
	}
	if !parallel {
		r.Reasons = append(r.Reasons, "missing concurrent workers versus sequential baseline with observed failure isolation")
	}
	d, z := r.Modes["direct_agent"], r.Modes["zenchron_explicit"]
	if d != nil && z != nil && d.AcceptedPerHour != nil && z.AcceptedPerHour != nil {
		r.ExplicitRatio = quotient(*z.AcceptedPerHour, *d.AcceptedPerHour)
	}
	if r.ExplicitRatio == nil {
		r.Reasons = append(r.Reasons, "ratio unavailable: require positive baseline throughput and measured supervision")
	}
	if z != nil && d != nil && (z.UnsafeAuthority > 0 || z.EscapedDefects > 0) {
		r.Reasons = append(r.Reasons, "safety gate requires zero observed escaped defects and unsafe authority outcomes")
	}
	if len(r.Reasons) == 0 {
		r.Gate = "below_target"
		if *r.ExplicitRatio >= 2 {
			r.Gate = "meets_observed_target"
		}
	}
	return r, nil
}
func sortedKeys(m map[string]map[string]Cohort) []string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func sequential(c Cohort) bool {
	for i, a := range c.Tasks {
		for _, b := range c.Tasks[i+1:] {
			if a.Work.Start.Before(b.Work.End) && b.Work.Start.Before(a.Work.End) {
				return false
			}
		}
	}
	return true
}
func comparable(a, b Cohort) bool {
	if a.Operator != b.Operator || a.Repository != b.Repository || a.Base != b.Base || a.Agent != b.Agent || a.Provider != b.Provider || a.Model != b.Model || a.TrustMode != b.TrustMode || len(a.Tasks) != len(b.Tasks) {
		return false
	}
	tasks := map[string]Task{}
	for _, t := range a.Tasks {
		tasks[t.ID] = t
	}
	for _, t := range b.Tasks {
		v, ok := tasks[t.ID]
		if !ok || v.Class != t.Class || v.Acceptance != t.Acceptance {
			return false
		}
	}
	return true
}
