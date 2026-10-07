package intelligence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// CachePartition is the storage.Records partition holding index snapshots,
// keyed by identity key.
const CachePartition = "intelligence.index"

// cacheFormat versions the record envelope independently of the snapshot.
const cacheFormat = 1

// ErrCacheInvalid reports a cached record that exists but cannot be used:
// corrupt, from another format, or for another identity.
var ErrCacheInvalid = errors.New("intelligence: cached snapshot unusable")

type cacheRecord struct {
	Format   int             `json:"format"`
	Checksum string          `json:"checksum"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// Save stores a base snapshot under its identity key. Overlays are execution
// scoped and are refused.
func Save(ctx context.Context, rec storage.Records, ix *Index) error {
	if ix.data.Overlay != nil {
		return fmt.Errorf("intelligence: overlays are execution-private and are not cached")
	}
	snap, err := json.Marshal(ix.data)
	if err != nil {
		return fmt.Errorf("intelligence: encode snapshot: %w", err)
	}
	b, err := json.Marshal(cacheRecord{Format: cacheFormat, Checksum: api.Digest(snap), Snapshot: snap})
	if err != nil {
		return fmt.Errorf("intelligence: encode cache record: %w", err)
	}
	return rec.Put(ctx, CachePartition, ix.Key(), b)
}

// Load returns the cached snapshot for key. A missing record is
// storage.ErrNotFound; an unusable one wraps ErrCacheInvalid. A loaded index
// is identical in facts to the one saved; it has no in-memory package types,
// so its overlays re-check unaffected dependencies from verified bytes.
func Load(ctx context.Context, rec storage.Records, key string) (*Index, error) {
	b, err := rec.Get(ctx, CachePartition, key)
	if err != nil {
		return nil, err
	}
	var r cacheRecord
	if err := decodeStrict(b, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCacheInvalid, err)
	}
	switch {
	case r.Format != cacheFormat:
		return nil, fmt.Errorf("%w: format %d, want %d", ErrCacheInvalid, r.Format, cacheFormat)
	case api.Digest(r.Snapshot) != r.Checksum:
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCacheInvalid)
	}
	var snap Snapshot
	if err := decodeStrict(r.Snapshot, &snap); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCacheInvalid, err)
	}
	if snap.Identity.Key() != key || snap.Overlay != nil {
		return nil, fmt.Errorf("%w: snapshot identity does not match key", ErrCacheInvalid)
	}
	return &Index{data: snap}, nil
}

// Open returns the index for cfg, from rec when a valid snapshot with the
// exact identity is cached, otherwise by building it and caching the result.
// Identity always comes from hashing the current workspace, so the cache can
// save extraction work but never serve facts for other bytes. Every cache
// failure falls back to a fresh build and is reported in Stats.CacheDegraded;
// rec may be nil to disable caching.
func Open(ctx context.Context, rec storage.Records, cfg BuildConfig) (*Index, error) {
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
	s := newSession(ctx, root, settings, scope, m, contents, &stats)
	if err := s.prepare(); err != nil {
		return nil, err
	}
	if rec == nil {
		stats.CacheDegraded = "cache disabled"
		return s.build()
	}
	key := newIdentity(m, settings, scope, s.modules).Key()
	ix, lerr := Load(ctx, rec, key)
	if lerr == nil {
		stats.CacheHit = true
		ix.stats = stats
		return ix, nil
	}
	if !errors.Is(lerr, storage.ErrNotFound) {
		stats.CacheDegraded = "cache read failed, rebuilt: " + lerr.Error()
	}
	ix, err = s.build()
	if err != nil {
		return nil, err
	}
	if serr := Save(ctx, rec, ix); serr != nil {
		ix.stats.CacheDegraded = joinReason(ix.stats.CacheDegraded, "cache write failed: "+serr.Error())
	}
	return ix, nil
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data after JSON value")
	}
	return nil
}
