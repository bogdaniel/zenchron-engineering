package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// harnessUnknowns are dimensions this harness never measures per run.
var harnessUnknowns = []string{"cpu_time", "resident_memory", "interventions"}

// observation is the raw record of one (task, trial, mode) run.
type observation struct {
	Task         string `json:"task"`
	HeldOut      bool   `json:"held_out"`
	Trial        int    `json:"trial"`
	Mode         string `json:"mode"`
	Drift        string `json:"drift,omitempty"`
	ScriptDigest string `json:"script_digest,omitempty"`
	HarnessError string `json:"harness_error,omitempty"`

	Outcome         string        `json:"outcome,omitempty"`
	Cause           string        `json:"cause,omitempty"`
	Dimension       string        `json:"dimension,omitempty"`
	ExpectedMatched bool          `json:"expected_matched"`
	Checks          []checkResult `json:"checks,omitempty"`
	Verified        bool          `json:"verified"`

	WallMillis  float64        `json:"wall_ms"`
	AllocBytes  uint64         `json:"alloc_bytes"`
	StoreBytes  int64          `json:"derived_store_bytes"`
	Index       *indexObs      `json:"index,omitempty"`
	Memory      *memoryObs     `json:"memory,omitempty"`
	Retrieval   []retrievalObs `json:"retrieval,omitempty"`
	ContextKept map[string]int `json:"context_selected_by_kind,omitempty"`

	Reported        api.TokenUsage `json:"reported_tokens"`
	Estimated       api.TokenUsage `json:"estimated_tokens"`
	ProviderCalls   int            `json:"provider_calls"`
	ToolCalls       int            `json:"tool_calls"`
	Retries         int            `json:"retries"`
	ToolOutputBytes int            `json:"tool_output_bytes_to_model"`
	FilesRead       *int           `json:"files_read"` // nil: unknown
	Rereads         *rereads       `json:"rereads"`    // nil: unknown
	Unknowns        []string       `json:"unknowns"`

	read map[string]string // path -> digest of the first read, for memory
}

type indexObs struct {
	CacheHit              bool     `json:"cache_hit"`
	CacheDegraded         string   `json:"cache_degraded,omitempty"`
	Invalidation          string   `json:"invalidation,omitempty"`
	OpenMillis            float64  `json:"open_ms"`
	ManifestMillis        float64  `json:"manifest_ms"`
	ExtractMillis         float64  `json:"extract_ms"`
	FilesHashed           int      `json:"files_hashed"`
	FilesParsed           int      `json:"files_parsed"`
	DirsExtracted         int      `json:"dirs_extracted"`
	DirsReused            int      `json:"dirs_reused"`
	ManifestAgrees        bool     `json:"manifest_agrees"`
	RebuildMillis         *float64 `json:"rebuild_ms,omitempty"`
	OverlayMatchesRebuild *bool    `json:"overlay_matches_rebuild,omitempty"`
}

func newIndexObs(s intelligence.Stats, open time.Duration) *indexObs {
	return &indexObs{
		CacheHit: s.CacheHit, CacheDegraded: s.CacheDegraded, OpenMillis: millis(open),
		ManifestMillis: millis(s.ManifestTime), ExtractMillis: millis(s.ExtractTime),
		FilesHashed: s.FilesHashed, FilesParsed: s.FilesParsed, DirsExtracted: s.DirsExtracted, DirsReused: s.DirsReused,
	}
}

type memoryObs struct {
	RecordsWritten int `json:"records_written"`
	Invalidated    int `json:"invalidated"`
	ItemsSelected  int `json:"items_selected"`
}

type retrievalObs struct {
	Source string  `json:"source"`
	Millis float64 `json:"ms"`
	Items  int     `json:"items"`
	Error  string  `json:"error,omitempty"`
}

// rereads splits repeated reads of one path: justified when the bytes had
// changed since the last read, redundant when the digest was identical.
type rereads struct {
	Justified int `json:"justified"`
	Redundant int `json:"redundant"`
}

type checkResult struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// record turns one settled result into the observation's measurements.
func (h *harness) record(obs *observation, t task, res api.ExecutionResult, p *scripted.Provider,
	ws string, before map[string][]byte, env runEnv) {
	term := res.Termination
	obs.Outcome, obs.Cause, obs.Dimension = string(term.Outcome), string(term.Cause), string(term.Dimension)
	obs.ExpectedMatched = term.Outcome == t.Expect.Outcome && term.Cause == t.Expect.Cause &&
		term.Dimension == t.Expect.Dimension
	obs.Checks = runChecks(t.Checks, res, ws, before)
	obs.Verified = obs.ExpectedMatched
	for _, c := range obs.Checks {
		obs.Verified = obs.Verified && c.Passed
	}
	u := res.Usage
	obs.Reported, obs.Estimated = u.Reported, u.Estimated
	obs.ProviderCalls, obs.ToolCalls, obs.Retries = u.ProviderCalls, u.ToolCalls, u.Retries
	obs.Unknowns = append(append([]string{}, u.Unknowns...), harnessUnknowns...)
	analyzeTranscript(obs, p.Requests())
	if obs.Rereads != nil {
		n := len(obs.read)
		obs.FilesRead = &n
	}
	obs.ContextKept = map[string]int{}
	if res.Context != nil {
		for _, e := range res.Context.Entries {
			if e.Selected {
				obs.ContextKept[string(e.Kind)]++
			}
		}
	}
	if obs.Memory != nil {
		obs.Memory.ItemsSelected = obs.ContextKept[string(api.ContextMemory)]
	}
	for _, s := range env.sources {
		r := retrievalObs{Source: s.name, Millis: millis(s.elapsed), Items: s.items}
		if s.err != nil {
			r.Error = s.err.Error()
		}
		obs.Retrieval = append(obs.Retrieval, r)
	}
}

// readHeader matches the "path:/digest:" header read_file (and each step of
// read_files) puts on its output.
var readHeader = regexp.MustCompile(`(?m)^path: (\S+)\ndigest: (sha256:[0-9a-f]+)$`)

// analyzeTranscript measures what reached the model: the final provider
// request carries every tool result delivered before the last call. Results
// produced after the last provider call (for example at budget exhaustion)
// were never delivered and are not counted.
func analyzeTranscript(obs *observation, requests []api.ProviderRequest) {
	obs.read, obs.Rereads = map[string]string{}, &rereads{}
	if len(requests) == 0 {
		return
	}
	last := map[string]string{}
	for _, m := range requests[len(requests)-1].Messages {
		if m.Role != api.RoleTool {
			continue
		}
		obs.ToolOutputBytes += len(m.Content)
		if m.IsError {
			continue
		}
		// ponytail: the engine renders a tool result as JSON with an "output"
		// field; there is no structured per-call result in the API to use instead.
		var rendered struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(m.Content), &rendered); err != nil {
			obs.Unknowns = append(obs.Unknowns, "files_read", "rereads")
			obs.read, obs.Rereads = map[string]string{}, nil
			return
		}
		for _, match := range readHeader.FindAllStringSubmatch(rendered.Output, -1) {
			path, digest := match[1], match[2]
			prev, seen := last[path]
			switch {
			case !seen:
				obs.read[path] = digest
			case prev == digest:
				obs.Rereads.Redundant++
			default:
				obs.Rereads.Justified++
			}
			last[path] = digest
		}
	}
}

// readChecked snapshots every checked file before the run; absent is nil.
func readChecked(ws string, checks []check) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, c := range checks {
		if c.Path == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(c.Path)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out[c.Path] = data
	}
	return out, nil
}

func runChecks(checks []check, res api.ExecutionResult, ws string, before map[string][]byte) []checkResult {
	out := make([]checkResult, 0, len(checks))
	for _, c := range checks {
		out = append(out, runCheck(c, res, ws, before))
	}
	return out
}

func runCheck(c check, res api.ExecutionResult, ws string, before map[string][]byte) checkResult {
	r := checkResult{Kind: c.Kind, Path: c.Path}
	var data []byte
	var readErr error
	if c.Path != "" {
		data, readErr = os.ReadFile(filepath.Join(ws, filepath.FromSlash(c.Path)))
	}
	switch c.Kind {
	case "unchanged":
		r.Passed = readErr == nil && bytes.Equal(data, before[c.Path])
	case "file_contains":
		r.Passed = readErr == nil && bytes.Contains(data, []byte(c.Text))
	case "file_absent":
		r.Passed = errors.Is(readErr, fs.ErrNotExist)
	case "observations":
		n := 0
		for _, o := range res.Observations {
			if string(o.Kind) == c.Event {
				n++
			}
		}
		r.Passed, r.Detail = n == c.Count, fmt.Sprintf("%d %s", n, c.Event)
	case "retries":
		r.Passed, r.Detail = res.Usage.Retries == c.Count, fmt.Sprintf("%d retries", res.Usage.Retries)
	default:
		r.Detail = "unknown check kind"
	}
	if readErr != nil && r.Detail == "" && c.Kind != "file_absent" {
		r.Detail = readErr.Error()
	}
	return r
}

// encodeJSON writes v indented, for the report file.
func encodeJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	return append(data, '\n'), err
}
