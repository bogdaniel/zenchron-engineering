package main

import (
	"slices"
	"sort"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
)

const reportSchema = "agentkernel.kernel-eval.report/v0.1"

type report struct {
	Schema         string               `json:"schema"`
	GeneratedAt    time.Time            `json:"generated_at"`
	KernelVersion  string               `json:"kernel_version"`
	GoVersion      string               `json:"go_version"`
	GOOS           string               `json:"goos"`
	GOARCH         string               `json:"goarch"`
	Command        []string             `json:"command"`
	Config         reportConfig         `json:"config"`
	ConfigDigest   string               `json:"config_digest"`
	Corpus         corpusInfo           `json:"corpus"`
	Denominators   denominators         `json:"denominators"`
	ByMode         map[string]modeGroup `json:"by_mode"`
	Unknowns       []unknownCount       `json:"unknowns"`
	Failures       []failure            `json:"failures"`
	NotRun         []notRun             `json:"not_run"`
	Limitations    []string             `json:"limitations"`
	DurationMillis float64              `json:"duration_ms"`
	Observations   []observation        `json:"observations"`
}

type reportConfig struct {
	Trials          int                   `json:"trials"`
	Modes           []string              `json:"modes"`
	Settings        intelligence.Settings `json:"extraction_settings"`
	Binding         bindingLimits         `json:"binding"`
	DeadlineSeconds int                   `json:"deadline_seconds"`
	MemoryLimits    string                `json:"memory_limits"`
}

type corpusInfo struct {
	Dir            string            `json:"dir"`
	Digest         string            `json:"digest"`
	FixtureDigests map[string]string `json:"fixture_digests"`
	ScriptDigests  map[string]string `json:"script_digests"`
	Tasks          int               `json:"tasks"`
	HeldOut        int               `json:"held_out_tasks"`
}

// denominators count runs, never percentages, so nothing hides a failure.
// Planned is tasks x trials x modes; Attempted is runs actually started.
type denominators struct {
	Planned            int            `json:"planned"`
	Attempted          int            `json:"attempted"`
	Executed           int            `json:"executed"`
	HarnessErrors      int            `json:"harness_errors"`
	Verified           int            `json:"verified"`
	VerificationFailed int            `json:"verification_failed"`
	Outcomes           map[string]int `json:"outcomes"`
}

// modeGroup reports held-in and held-out tasks separately.
type modeGroup struct {
	HeldIn  group `json:"held_in"`
	HeldOut group `json:"held_out"`
}

type group struct {
	Denominators          denominators `json:"denominators"`
	WallMillis            spread       `json:"wall_ms"`
	IndexOpenMillis       *spread      `json:"index_open_ms,omitempty"`
	RebuildMillis         *spread      `json:"rebuild_ms,omitempty"`
	RetrievalMillis       *spread      `json:"retrieval_ms,omitempty"`
	EstimatedInputTokens  spread       `json:"estimated_input_tokens"`
	ReportedInputTokens   spread       `json:"reported_input_tokens"`
	ToolCalls             spread       `json:"tool_calls"`
	ToolOutputBytes       spread       `json:"tool_output_bytes_to_model"`
	FilesRead             spread       `json:"files_read"`
	DerivedStoreBytes     *spread      `json:"derived_store_bytes,omitempty"`
	RedundantRereads      int          `json:"redundant_rereads"`
	JustifiedRereads      int          `json:"justified_rereads"`
	Retries               int          `json:"retries"`
	CacheHits             int          `json:"cache_hits"`
	CacheDegraded         int          `json:"cache_degraded"`
	OverlayMatchesRebuild int          `json:"overlay_matches_rebuild"`
	OverlayDiffers        int          `json:"overlay_differs"`
	MemoryItemsSelected   int          `json:"memory_items_selected"`
	MemoryInvalidated     int          `json:"memory_records_invalidated"`
}

// spread is min/median/max over the N runs where the value is known.
// Unknown is how many executed runs in the group did not establish it. A
// metric that does not apply to a mode (index cost in the baseline, rebuild
// outside drift) is omitted from that mode's group instead.
type spread struct {
	N       int     `json:"n"`
	Unknown int     `json:"unknown"`
	Min     float64 `json:"min"`
	Median  float64 `json:"median"`
	Max     float64 `json:"max"`
}

type unknownCount struct {
	Name string `json:"name"`
	Runs int    `json:"runs"`
}

type failure struct {
	Task   string `json:"task"`
	Trial  int    `json:"trial"`
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

type notRun struct {
	Baseline string `json:"baseline"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

var gateBNotRun = notRun{
	Baseline: "5: native CLI/provider execution vs kernel on isolated identical task snapshots",
	Status:   "not run: Gate B",
	Reason: "requires production host integration, host-owned assurance and explicit spending approval; " +
		"Gate A spends no provider credit",
}

var limitations = []string{
	"Offline scripted fixtures prove kernel mechanics (budgets, capability refusals, cache reuse, invalidation, " +
		"memory staleness), not paid-model quality or accepted engineering throughput.",
	"The scripted provider reports no token usage: every reported token count is unknown, and no token or cost " +
		"saving is claimed for any provider.",
	"Estimated tokens are the engine's local approximation (about four bytes per token) of the compiled prompt. " +
		"They include retrieved context, so cold/warm/drift runs can estimate more input than the baseline. " +
		"They are not billing data.",
	"The scripted model's actions are fixed, so retrieved context and memory cannot change its tool calls or " +
		"outcome. Equal outcomes across modes show no execution regression under these fixtures, not a quality benefit.",
	"No pass threshold such as a fixed percentage saving is applied. The harness fails only on harness errors " +
		"or a verification result that differs from the task's expectation.",
	"Wall times are sequential runs in one process on one machine and are sensitive to scheduling. The fixture " +
		"is tiny, so index costs here say nothing about large repositories.",
	"Memory records are written only after the cold run of a non-held-out task. Held-out tasks never receive a " +
		"remembered summary of themselves.",
	"Tool output bytes, files read and rereads are measured from the transcript delivered to the provider. " +
		"Results produced after the final provider call (for example at budget exhaustion) are not counted, and " +
		"search scans are not counted as file reads.",
	"alloc_bytes is the process-wide allocation delta around Execute (sequential, includes harness bookkeeping). " +
		"CPU time, resident memory and human interventions are not measured.",
	"Maintenance cost of the corpus and harness is not measured by this run.",
}

// summarize computes denominators and per-mode spreads from raw observations.
func summarize(planned int, obs []observation) (denominators, map[string]modeGroup, []unknownCount, []failure) {
	total := newDenominators(planned)
	byMode := map[mode][]observation{}
	unknowns := map[string]int{}
	var failures []failure
	for _, o := range obs {
		total.add(o)
		byMode[mode(o.Mode)] = append(byMode[mode(o.Mode)], o)
		for _, u := range o.Unknowns {
			unknowns[u]++
		}
		if f, ok := failureOf(o); ok {
			failures = append(failures, f)
		}
	}
	groups := map[string]modeGroup{}
	for m, list := range byMode {
		var in, out []observation
		for _, o := range list {
			if o.HeldOut {
				out = append(out, o)
			} else {
				in = append(in, o)
			}
		}
		groups[string(m)] = modeGroup{HeldIn: summarizeGroup(m, in), HeldOut: summarizeGroup(m, out)}
	}
	return total, groups, sortedUnknowns(unknowns), failures
}

func newDenominators(planned int) denominators {
	return denominators{Planned: planned, Outcomes: map[string]int{}}
}

func (d *denominators) add(o observation) {
	d.Attempted++
	if o.HarnessError != "" {
		d.HarnessErrors++
		return
	}
	d.Executed++
	d.Outcomes[o.Outcome]++
	if o.Verified {
		d.Verified++
	} else {
		d.VerificationFailed++
	}
}

func failureOf(o observation) (failure, bool) {
	f := failure{Task: o.Task, Trial: o.Trial, Mode: o.Mode}
	switch {
	case o.HarnessError != "":
		f.Reason = "harness error: " + o.HarnessError
	case !o.ExpectedMatched:
		f.Reason = "unexpected termination " + o.Outcome + "/" + o.Cause
	case !o.Verified:
		f.Reason = "verification check failed"
	default:
		return f, false
	}
	return f, true
}

func sortedUnknowns(m map[string]int) []unknownCount {
	out := make([]unknownCount, 0, len(m))
	for name, n := range m {
		out = append(out, unknownCount{Name: name, Runs: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// spreadOf summarizes known values; unknown counts the runs without one.
func spreadOf(values []float64, unknown int) spread {
	s := spread{N: len(values), Unknown: unknown}
	if len(values) == 0 {
		return s
	}
	v := slices.Clone(values)
	slices.Sort(v)
	s.Min, s.Max = v[0], v[len(v)-1]
	mid := len(v) / 2
	s.Median = v[mid]
	if len(v)%2 == 0 {
		s.Median = (v[mid-1] + v[mid]) / 2
	}
	return s
}
