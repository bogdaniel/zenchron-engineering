// Package intelligence extracts generic structural facts about a Go workspace:
// a content-addressed file manifest, packages, declarations, imports, resolved
// references, call edges split by how much is known about them, and inferred
// test associations, each with provenance.
//
// It carries no engineering meaning: no ownership, impact, security or policy
// interpretation (that is #67, which consumes these facts through its own
// translation seam). Every fact is an observation of workspace bytes, trusted
// as workspace content and never as host instruction.
//
// A base Index is immutable once built. Execution-scoped overlays derive new
// indexes from it without mutating it, and a View binds one index to the
// workspace it describes so a stale snapshot is refused rather than served.
package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"time"
)

// BuildConfig is one extraction request. Root is the workspace directory;
// nothing outside it is read and no host environment is consulted.
type BuildConfig struct {
	Root     string
	Scope    Scope
	Settings Settings
}

// Stats are the cost counters of producing one index, for benchmarks.
type Stats struct {
	FilesHashed     int           `json:"files_hashed"`
	BytesHashed     int64         `json:"bytes_hashed"`
	FilesRead       int           `json:"files_read"`
	FilesParsed     int           `json:"files_parsed"`
	PackagesChecked int           `json:"packages_checked"`
	DirsExtracted   int           `json:"dirs_extracted"`
	DirsReused      int           `json:"dirs_reused"`
	ManifestTime    time.Duration `json:"manifest_time"`
	ExtractTime     time.Duration `json:"extract_time"`
	// CacheHit is set by Open when the index came from the cache.
	CacheHit bool `json:"cache_hit"`
	// CacheDegraded explains why a usable cache was not used or written.
	// Degradation costs efficiency only; the index is always freshly correct.
	CacheDegraded string `json:"cache_degraded,omitempty"`
}

// Index is one immutable snapshot of extracted facts.
type Index struct {
	data Snapshot
	// types are the non-test package types of the session that built this
	// index, reused read-only by overlays for unaffected packages. Nil after
	// Load: overlays then re-check dependencies from verified bytes.
	types map[string]*types.Package
	stats Stats
}

// Build extracts an index for the scope. With Scope.Packages set it reads only
// those directories and their go.mod chain: there is no mandatory full
// workspace pass before the first useful answer.
func Build(ctx context.Context, cfg BuildConfig) (*Index, error) {
	root, scope, settings, err := cfg.canonical()
	if err != nil {
		return nil, err
	}
	var stats Stats
	start := time.Now()
	m, contents, err := buildManifest(ctx, root, scope, &stats)
	if err != nil {
		return nil, err
	}
	stats.ManifestTime = time.Since(start)
	return buildFrom(ctx, root, scope, settings, m, contents, stats)
}

func buildFrom(ctx context.Context, root string, scope Scope, settings Settings, m Manifest,
	contents map[string][]byte, stats Stats) (*Index, error) {
	s := newSession(ctx, root, settings, scope, m, contents, &stats)
	if err := s.prepare(); err != nil {
		return nil, err
	}
	return s.build()
}

// build extracts a prepared session and stamps its cost counters.
func (s *session) build() (*Index, error) {
	start := time.Now()
	ix, err := s.finish()
	if err != nil {
		return nil, err
	}
	s.stats.ExtractTime = time.Since(start)
	ix.stats = *s.stats
	return ix, nil
}

// finish extracts and assembles the index.
func (s *session) finish() (*Index, error) {
	dirs, err := s.extract()
	if err != nil {
		return nil, err
	}
	ix := &Index{data: Snapshot{
		Identity:   newIdentity(s.manifest, s.settings, s.scope, s.modules),
		Manifest:   s.manifest,
		Modules:    s.modules,
		Dirs:       dirs,
		Tests:      linkTests(dirs),
		Incomplete: s.incomplete,
	}, types: map[string]*types.Package{}}
	for ip, c := range s.checked {
		ix.types[ip] = c.pkg
	}
	for ip, pkg := range s.baseTypes {
		_, reused := s.reuse[s.dirOf[ip]]
		if _, checked := ix.types[ip]; reused && !checked {
			ix.types[ip] = pkg
		}
	}
	return ix, nil
}

func (cfg BuildConfig) canonical() (string, Scope, Settings, error) {
	root, err := canonicalRoot(cfg.Root)
	if err != nil {
		return "", Scope{}, Settings{}, err
	}
	scope, err := cfg.Scope.canonical()
	if err != nil {
		return "", Scope{}, Settings{}, err
	}
	settings, err := cfg.Settings.canonical()
	if err != nil {
		return "", Scope{}, Settings{}, err
	}
	return root, scope, settings, nil
}

func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("intelligence: workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("intelligence: workspace root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("intelligence: workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("intelligence: workspace root %s is not a directory", abs)
	}
	return abs, nil
}

// Key is the snapshot identity key.
func (ix *Index) Key() string { return ix.data.Identity.Key() }

// Identity returns a copy of the snapshot identity.
func (ix *Index) Identity() Identity { return ix.data.Identity.clone() }

// Stats returns the cost counters of producing this index.
func (ix *Index) Stats() Stats { return ix.stats }

// Snapshot returns a deep copy of every fact; mutating it cannot affect the
// index.
func (ix *Index) Snapshot() Snapshot {
	b, err := json.Marshal(ix.data)
	if err != nil {
		panic("intelligence: snapshot is plain data and always encodes: " + err.Error())
	}
	var out Snapshot
	if err := json.Unmarshal(b, &out); err != nil {
		panic("intelligence: snapshot round trip: " + err.Error())
	}
	return out
}
