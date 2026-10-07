package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

const corpusVersion = "agentkernel.kernel-eval.corpus/v0.1"

// corpus is testdata/eval/corpus.json: fixtures, drift scenarios and tasks.
type corpus struct {
	Version         string                   `json:"version"`
	Description     string                   `json:"description"`
	Fixtures        []string                 `json:"fixtures"`
	Settings        intelligence.Settings    `json:"settings"`
	Binding         bindingLimits            `json:"binding"`
	DeadlineSeconds int                      `json:"deadline_seconds"`
	Budget          budgetBounds             `json:"budget"`
	Drift           map[string]driftScenario `json:"drift"`
	Tasks           []task                   `json:"tasks"`
	dir             string
	digest          string
	fixtureDigests  map[string]string
}

type bindingLimits struct {
	ContextWindow   int64 `json:"context_window"`
	MaxOutputTokens int64 `json:"max_output_tokens"`
}

// budgetBounds is api.Budget without the deadline, which is set per run.
type budgetBounds struct {
	MaxIterations      int   `json:"max_iterations"`
	MaxToolCalls       int   `json:"max_tool_calls"`
	MaxInputTokens     int64 `json:"max_input_tokens"`
	MaxOutputTokens    int64 `json:"max_output_tokens"`
	MaxArtifactBytes   int64 `json:"max_artifact_bytes"`
	MaxProviderRetries int   `json:"max_provider_retries"`
}

type driftScenario struct {
	Description string        `json:"description"`
	Changes     []driftChange `json:"changes,omitempty"`
	BuildTags   []string      `json:"build_tags,omitempty"`
}

type driftChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type task struct {
	ID        string           `json:"id"`
	Fixture   string           `json:"fixture"`
	HeldOut   bool             `json:"held_out"`
	Drift     string           `json:"drift"`
	Objective string           `json:"objective"`
	Mode      api.Mode         `json:"mode"`
	Grants    []api.Capability `json:"grants"`
	Budget    json.RawMessage  `json:"budget,omitempty"`
	Steps     []step           `json:"steps"`
	Expect    expectation      `json:"expect"`
	Checks    []check          `json:"checks"`
}

// step is one scripted provider answer. It has no usage field: the corpus
// never invents token counts.
type step struct {
	ToolCalls []stepCall         `json:"tool_calls,omitempty"`
	Text      string             `json:"text,omitempty"`
	Error     *api.ProviderError `json:"error,omitempty"`
}

type stepCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type expectation struct {
	Outcome   api.Outcome         `json:"outcome"`
	Cause     api.Cause           `json:"cause"`
	Dimension api.BudgetDimension `json:"dimension,omitempty"`
}

var checkKinds = []string{"unchanged", "file_contains", "file_absent", "observations", "retries"}

type check struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Text  string `json:"text,omitempty"`
	Event string `json:"event,omitempty"`
	Count int    `json:"count,omitempty"`
}

// loadCorpus strictly decodes dir/corpus.json and records the digests the
// report binds to: the corpus file and each fixture's content manifest.
func loadCorpus(dir string) (*corpus, error) {
	data, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c corpus
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("corpus.json: %w", err)
	}
	c.dir, c.digest, c.fixtureDigests = dir, api.Digest(data), map[string]string{}
	if err := c.validate(); err != nil {
		return nil, err
	}
	for _, f := range c.Fixtures {
		m, err := intelligence.BuildManifest(context.Background(), c.fixtureDir(f), intelligence.Scope{})
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", f, err)
		}
		c.fixtureDigests[f] = m.Digest()
	}
	return &c, nil
}

func (c *corpus) validate() error {
	if c.Version != corpusVersion {
		return fmt.Errorf("corpus version %q, want %q", c.Version, corpusVersion)
	}
	if len(c.Tasks) == 0 || c.DeadlineSeconds <= 0 {
		return fmt.Errorf("corpus needs tasks and a positive deadline_seconds")
	}
	seen := map[string]bool{}
	for _, t := range c.Tasks {
		switch {
		case !api.ValidIdentifier(t.ID) || seen[t.ID]:
			return fmt.Errorf("task id %q invalid or duplicate", t.ID)
		case !slices.Contains(c.Fixtures, t.Fixture):
			return fmt.Errorf("task %s: unknown fixture %q", t.ID, t.Fixture)
		case c.Drift[t.Drift].Changes == nil && c.Drift[t.Drift].BuildTags == nil:
			return fmt.Errorf("task %s: unknown or empty drift scenario %q", t.ID, t.Drift)
		case len(t.Steps) == 0 || t.Expect.Outcome == "":
			return fmt.Errorf("task %s: needs steps and an expected outcome", t.ID)
		}
		for _, ch := range t.Checks {
			if !slices.Contains(checkKinds, ch.Kind) {
				return fmt.Errorf("task %s: unknown check kind %q", t.ID, ch.Kind)
			}
		}
		seen[t.ID] = true
	}
	return nil
}

func (c *corpus) fixtureDir(name string) string { return filepath.Join(c.dir, "fixtures", name) }

// budget overlays a task's partial budget on the corpus defaults.
func (c *corpus) budget(t task) (budgetBounds, error) {
	b := c.Budget
	if len(t.Budget) == 0 {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(t.Budget))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("task %s budget: %w", t.ID, err)
	}
	return b, nil
}

// scriptDigest binds a task's scripted provider behaviour.
func (t task) scriptDigest() (string, error) {
	data, err := json.Marshal(t.Steps)
	return api.Digest(data), err
}

var digestPlaceholder = regexp.MustCompile(`\{\{sha256:([^}]+)\}\}`)

// script turns steps into a scripted provider. A "{{sha256:path}}" argument is
// the digest of that workspace file at run start: the precondition a model
// would have copied from its own earlier read.
func (t task) script(workspace string) (*scripted.Provider, error) {
	var steps []scripted.Step
	for _, s := range t.Steps {
		if s.Error != nil {
			e := *s.Error
			steps = append(steps, scripted.Step{Err: &e})
			continue
		}
		resp := api.ProviderResponse{Text: s.Text, Stop: api.StopEnd}
		for _, c := range s.ToolCalls {
			args, err := resolveDigests(c.Arguments, workspace)
			if err != nil {
				return nil, fmt.Errorf("task %s call %s: %w", t.ID, c.ID, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, api.ToolCall{ID: c.ID, Name: c.Name, Arguments: args})
			resp.Stop = api.StopToolUse
		}
		steps = append(steps, scripted.Step{Response: resp})
	}
	return scripted.New(steps...), nil
}

func resolveDigests(args json.RawMessage, workspace string) (json.RawMessage, error) {
	var failure error
	out := digestPlaceholder.ReplaceAllFunc(args, func(m []byte) []byte {
		rel := string(digestPlaceholder.FindSubmatch(m)[1])
		data, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel)))
		if err != nil {
			failure = err
			return m
		}
		return []byte(api.Digest(data))
	})
	return out, failure
}
