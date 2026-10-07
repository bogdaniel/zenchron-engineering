package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/memory"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

var memoryLimits = memory.Limits{MaxRecords: 16, MaxBytes: 1 << 20}

// runEnv is what a mode supplies to one execution.
type runEnv struct {
	digest  string
	sources []*timedSource
	memory  *memory.Store
}

func (e runEnv) contextSources() []api.ContextSource {
	out := make([]api.ContextSource, 0, len(e.sources))
	for _, s := range e.sources {
		out = append(out, s)
	}
	return out
}

// timedSource measures retrieval: the time and items a source returned.
// The engine queries sources sequentially, once per execution.
type timedSource struct {
	name    string
	src     api.ContextSource
	elapsed time.Duration
	items   int
	err     error
}

func (s *timedSource) ContextItems(ctx context.Context, q api.ContextQuery) ([]api.ContextItem, error) {
	start := time.Now()
	items, err := s.src.ContextItems(ctx, q)
	s.elapsed += time.Since(start)
	s.items += len(items)
	s.err = err
	return items, err
}

// prepare binds the workspace digest and, outside the baseline, opens the
// file-backed index and memory stores the mode calls for.
func (h *harness) prepare(ctx context.Context, t task, m mode, st *cache, ws string, obs *observation) (runEnv, error) {
	man, err := intelligence.BuildManifest(ctx, ws, intelligence.Scope{})
	if err != nil {
		return runEnv{}, err
	}
	if m == modeBaseline {
		return runEnv{digest: man.Digest()}, nil
	}
	if m != modeCold && st.baseKey == "" {
		if err := h.prime(ctx, t, st, filepath.Dir(ws)); err != nil {
			return runEnv{}, fmt.Errorf("prime cache: %w", err)
		}
	}
	idx, err := storage.OpenFileRecords(st.indexDir)
	if err != nil {
		return runEnv{}, err
	}
	store, err := openMemory(st.memoryDir)
	if err != nil {
		return runEnv{}, err
	}
	obs.Memory = &memoryObs{}
	ix, err := h.index(ctx, t, m, st, idx, store, ws, obs)
	if err != nil {
		return runEnv{}, err
	}
	digest := ix.Identity().ManifestDigest
	obs.Index.ManifestAgrees = digest == man.Digest()
	view, err := intelligence.NewView(ix, api.WorkspaceRef{ID: t.Fixture, ManifestDigest: digest})
	if err != nil {
		return runEnv{}, err
	}
	return runEnv{digest: digest, memory: store, sources: []*timedSource{
		{name: "intelligence", src: view}, {name: "memory", src: store.Source(partition(t))},
	}}, nil
}

// prime fills the cache from a pristine fixture copy when warm or drift runs
// without a preceding cold run. It is setup, not an observation.
func (h *harness) prime(ctx context.Context, t task, st *cache, dir string) error {
	ws := filepath.Join(dir, "prime")
	if err := os.CopyFS(ws, os.DirFS(h.c.fixtureDir(t.Fixture))); err != nil {
		return err
	}
	rec, err := storage.OpenFileRecords(st.indexDir)
	if err != nil {
		return err
	}
	ix, err := intelligence.Open(ctx, rec, intelligence.BuildConfig{Root: ws, Settings: h.c.Settings})
	if err != nil {
		return err
	}
	st.baseKey = ix.Key()
	return nil
}

func (h *harness) index(ctx context.Context, t task, m mode, st *cache, rec storage.Records,
	store *memory.Store, ws string, obs *observation) (*intelligence.Index, error) {
	if m == modeDrift {
		return h.drifted(ctx, t, st, rec, store, ws, obs)
	}
	start := time.Now()
	ix, err := intelligence.Open(ctx, rec, intelligence.BuildConfig{Root: ws, Settings: h.c.Settings})
	if err != nil {
		return nil, err
	}
	obs.Index = newIndexObs(ix.Stats(), time.Since(start))
	st.baseKey = ix.Key()
	obs.StoreBytes, err = dirBytes(st.indexDir, st.memoryDir)
	return ix, err
}

// drifted reopens the warm base snapshot and derives an execution overlay
// for the drift scenario. A fresh Open of the drifted bytes is timed beside
// it and must neither hit the stale cache nor disagree with the overlay.
func (h *harness) drifted(ctx context.Context, t task, st *cache, rec storage.Records, store *memory.Store,
	ws string, obs *observation) (*intelligence.Index, error) {
	sc := h.c.Drift[t.Drift]
	settings := h.c.Settings
	spec := intelligence.OverlaySpec{Root: ws}
	for _, ch := range sc.Changes {
		spec.Changes = append(spec.Changes, intelligence.Change{Path: ch.Path, Content: []byte(ch.Content)})
	}
	if sc.BuildTags != nil {
		settings.BuildTags = sc.BuildTags
		spec.Settings = &settings
	}
	start := time.Now()
	base, err := intelligence.Load(ctx, rec, st.baseKey)
	if err != nil {
		return nil, fmt.Errorf("reopen base snapshot: %w", err)
	}
	ix, err := base.Overlay(ctx, spec)
	if err != nil {
		return nil, err
	}
	obs.Index = newIndexObs(ix.Stats(), time.Since(start))
	obs.Index.Invalidation = ix.Snapshot().Overlay.Invalidation
	start = time.Now()
	rebuilt, err := intelligence.Open(ctx, rec, intelligence.BuildConfig{Root: ws, Settings: settings})
	if err != nil {
		return nil, fmt.Errorf("rebuild drifted index: %w", err)
	}
	rebuild := millis(time.Since(start))
	agrees := rebuilt.Key() == ix.Key() && !rebuilt.Stats().CacheHit
	obs.Index.RebuildMillis, obs.Index.OverlayMatchesRebuild = &rebuild, &agrees
	if obs.Memory.Invalidated, err = h.invalidate(ctx, store, t, sc); err != nil {
		return nil, err
	}
	obs.StoreBytes, err = dirBytes(st.indexDir, st.memoryDir)
	return ix, err
}

// invalidate marks memory derived from each drifted file's prior bytes stale.
func (h *harness) invalidate(ctx context.Context, store *memory.Store, t task, sc driftScenario) (int, error) {
	total := 0
	for _, ch := range sc.Changes {
		old, err := os.ReadFile(filepath.Join(h.c.fixtureDir(t.Fixture), filepath.FromSlash(ch.Path)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return total, err
		}
		n, err := store.InvalidateSource(ctx, partition(t), api.Digest(old))
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func openMemory(dir string) (*memory.Store, error) {
	rec, err := storage.OpenFileRecords(dir)
	if err != nil {
		return nil, err
	}
	return memory.New(rec, memoryLimits, time.Now)
}

func partition(t task) memory.Partition {
	return memory.Partition{Repository: t.Fixture, Workspace: "fixture", Access: "kernel-eval"}
}

// remember writes one observation record after a cold run of a non-held-out
// task, bound to the exact digests it read. Held-out tasks never get one, so
// no remembered solution can masquerade as general improvement.
func remember(ctx context.Context, store *memory.Store, t task, obs *observation) error {
	if len(obs.read) == 0 {
		return nil
	}
	paths := make([]string, 0, len(obs.read))
	for p := range obs.read {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	digests := make([]string, 0, len(paths))
	for _, p := range paths {
		digests = append(digests, obs.read[p])
	}
	err := store.Put(ctx, memory.Record{
		ID: "run-" + t.ID, Kind: memory.KindObservation, Partition: partition(t), Subject: t.ID,
		Content:       fmt.Sprintf("an earlier run of %s read %s and settled %s", t.ID, strings.Join(paths, ", "), obs.Outcome),
		SourceDigests: digests,
		Derivation:    memory.Derivation{Method: "kernel-eval.run-summary", Version: "1"},
	})
	if err != nil {
		return fmt.Errorf("remember: %w", err)
	}
	obs.Memory.RecordsWritten = 1
	return nil
}

// dirBytes is the on-disk size of the derived stores, a disk-cost measure.
func dirBytes(dirs ...string) (int64, error) {
	var total int64
	for _, d := range dirs {
		err := filepath.Walk(d, func(_ string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
