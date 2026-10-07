package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// mode is one paired baseline of issue #446 section 16 (items 1-4).
type mode string

const (
	modeBaseline mode = "baseline" // 1: intelligence and memory disabled
	modeCold     mode = "cold"     // 2: empty index and memory stores
	modeWarm     mode = "warm"     // 3: file-backed stores reopened, snapshot validated
	modeDrift    mode = "drift"    // 4: controlled source/config/settings change
)

var allModes = []mode{modeBaseline, modeCold, modeWarm, modeDrift}

const bindingID = "scripted"

// harness runs corpus tasks. Every workspace and store lives under scratch.
type harness struct {
	c       *corpus
	scratch string
}

// cache is one (trial, task) pair's file-backed derived state, shared by its
// cold, warm and drift runs in that order.
type cache struct {
	indexDir, memoryDir string
	baseKey             string
}

func (h *harness) runTask(ctx context.Context, trial int, t task, modes []mode) []observation {
	dir := filepath.Join(h.scratch, fmt.Sprintf("trial-%d", trial), t.ID)
	st := &cache{indexDir: filepath.Join(dir, "index"), memoryDir: filepath.Join(dir, "memory")}
	out := make([]observation, 0, len(modes))
	for _, m := range modes {
		obs := observation{Task: t.ID, HeldOut: t.HeldOut, Trial: trial, Mode: string(m)}
		if m == modeDrift {
			obs.Drift = t.Drift
		}
		if err := h.execute(ctx, t, m, st, filepath.Join(dir, string(m)), &obs); err != nil {
			obs.HarnessError = err.Error()
		}
		out = append(out, obs)
	}
	return out
}

// execute runs one task in one mode on a fresh copy of its fixture.
func (h *harness) execute(ctx context.Context, t task, m mode, st *cache, dir string, obs *observation) error {
	ws := filepath.Join(dir, "workspace")
	if err := os.CopyFS(ws, os.DirFS(h.c.fixtureDir(t.Fixture))); err != nil {
		return err
	}
	if m == modeDrift {
		if err := applyDrift(ws, h.c.Drift[t.Drift]); err != nil {
			return err
		}
	}
	env, err := h.prepare(ctx, t, m, st, ws, obs)
	if err != nil {
		return err
	}
	before, err := readChecked(ws, t.Checks)
	if err != nil {
		return err
	}
	if obs.ScriptDigest, err = t.scriptDigest(); err != nil {
		return err
	}
	provider, err := t.script(ws)
	if err != nil {
		return err
	}
	res, err := h.run(ctx, t, m, obs, ws, provider, env)
	if err != nil {
		return err
	}
	h.record(obs, t, res, provider, ws, before, env)
	if m == modeCold && !t.HeldOut {
		return remember(ctx, env.memory, t, obs)
	}
	return nil
}

// run builds a fresh engine and times exactly one Execute.
func (h *harness) run(ctx context.Context, t task, m mode, obs *observation, ws string,
	provider *scripted.Provider, env runEnv) (api.ExecutionResult, error) {
	eng, err := newEngine(ws, provider, env.contextSources())
	if err != nil {
		return api.ExecutionResult{}, err
	}
	req, err := h.request(t, m, obs.Trial, env.digest)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	alloc := ms.TotalAlloc
	start := time.Now()
	res, err := eng.Execute(ctx, req)
	obs.WallMillis = millis(time.Since(start))
	runtime.ReadMemStats(&ms)
	obs.AllocBytes = ms.TotalAlloc - alloc
	if err != nil {
		return res, fmt.Errorf("execute: %w", err)
	}
	return res, nil
}

func newEngine(ws string, provider api.Provider, sources []api.ContextSource) (*engine.Engine, error) {
	w, err := tools.NewWorkspace(ws, nil)
	if err != nil {
		return nil, err
	}
	readFiles, err := w.ReadFiles()
	if err != nil {
		return nil, err
	}
	broker, err := tools.NewBroker(w.ReadFile(), readFiles, w.Search(), w.WriteFile(), w.ApplyPatch())
	if err != nil {
		return nil, err
	}
	artifacts, err := storage.NewMemoryArtifacts(8 << 20)
	if err != nil {
		return nil, err
	}
	return engine.New(engine.Config{
		Providers: map[string]api.Provider{bindingID: provider}, Broker: broker, Artifacts: artifacts,
		Events: discardEvents{}, Clock: systemClock{}, Sources: sources, OutputLimit: 8192,
	})
}

func (h *harness) request(t task, m mode, trial int, digest string) (api.ExecutionRequest, error) {
	b, err := h.c.budget(t)
	if err != nil {
		return api.ExecutionRequest{}, err
	}
	return api.ExecutionRequest{
		Version: api.ExecutionVersion, ExecutionID: fmt.Sprintf("%s-%s-%d", t.ID, m, trial), AttemptID: "attempt-1",
		Objective: t.Objective, Mode: t.Mode,
		Workspace: api.WorkspaceRef{ID: t.Fixture, ManifestDigest: digest},
		Constraints: api.Constraints{
			RequiredFeatures: []string{api.FeatureTools}, InstructionDigest: h.c.digest,
		},
		Grants: t.Grants,
		Budget: api.Budget{
			Deadline:      time.Now().Add(time.Duration(h.c.DeadlineSeconds) * time.Second),
			MaxIterations: b.MaxIterations, MaxToolCalls: b.MaxToolCalls, MaxInputTokens: b.MaxInputTokens,
			MaxOutputTokens: b.MaxOutputTokens, MaxArtifactBytes: b.MaxArtifactBytes,
			MaxProviderRetries: b.MaxProviderRetries,
		},
		Providers: []api.ProviderBinding{{
			ID: bindingID, Kind: "scripted", Model: "corpus-script", ModelVersion: api.UnknownVersion,
			Features: []string{api.FeatureTools}, ConfigFingerprint: h.c.digest, Eligible: true,
			Isolation: api.IsolationUnproven, ContextWindow: h.c.Binding.ContextWindow,
			MaxOutputTokens: h.c.Binding.MaxOutputTokens,
		}},
	}, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// discardEvents accepts every event; the result's observations mirror them.
type discardEvents struct{}

func (discardEvents) Record(context.Context, api.Event) error { return nil }

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func applyDrift(ws string, sc driftScenario) error {
	for _, ch := range sc.Changes {
		if !api.ValidRelativePath(ch.Path) {
			return fmt.Errorf("drift path %q is not a clean relative path", ch.Path)
		}
		target := filepath.Join(ws, filepath.FromSlash(ch.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(ch.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
