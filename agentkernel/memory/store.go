package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/strictjson"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// Limits bound each partition. MaxRecords and MaxBytes are required; MaxAge,
// when positive, retires unreferenced records older than it.
type Limits struct {
	MaxRecords int
	MaxBytes   int64
	MaxAge     time.Duration
}

// Store is the memory record store over one storage.Records. Use one Store per
// Records value: its lock serializes the read-settle-write of each update.
type Store struct {
	records storage.Records
	limits  Limits
	now     func() time.Time
	mu      sync.RWMutex
	pins    map[string]int
}

// New opens a store. Reopening over the same Records after a restart sees
// every record written before; validity is re-derived on every read.
func New(records storage.Records, limits Limits, now func() time.Time) (*Store, error) {
	if records == nil || now == nil {
		return nil, errors.New("memory: records and clock are required")
	}
	if limits.MaxRecords <= 0 || limits.MaxBytes <= 0 || limits.MaxAge < 0 {
		return nil, errors.New("memory: max records and max bytes must be positive, max age non-negative")
	}
	return &Store{records: records, limits: limits, now: now, pins: map[string]int{}}, nil
}

// partitionKey derives the storage partition from all three scope components;
// a digest avoids any separator ambiguity between components.
func partitionKey(p Partition) string {
	data, _ := json.Marshal(p) // a struct of strings always encodes
	sum := sha256.Sum256(data)
	return "memory-" + hex.EncodeToString(sum[:])
}

func pinKey(p Partition, id string) string { return partitionKey(p) + "/" + id }

// Pin marks a record as referenced by active work until release is called.
// Pinned records and their dependencies are never evicted or retired.
func (s *Store) Pin(p Partition, id string) (release func()) {
	key := pinKey(p, id)
	s.mu.Lock()
	s.pins[key]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.pins[key]--; s.pins[key] <= 0 {
				delete(s.pins, key)
			}
		})
	}
}

// Put writes a record, replacing any record with the same ID. Empty Version,
// Validity and CreatedAt default to RecordVersion, valid and now. A write that
// cannot fit without evicting referenced records is refused with ErrFull.
func (s *Store) Put(ctx context.Context, r Record) error {
	if r.Version == "" {
		r.Version = RecordVersion
	}
	if r.Validity == "" {
		r.Validity = Valid
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now()
	}
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load(ctx, r.Partition)
	if err != nil {
		return err
	}
	l.recs[r.ID] = r
	_, err = s.commit(ctx, r.Partition, l, r.ID)
	return err
}

// Get returns one record with its current validity, or storage.ErrNotFound.
func (s *Store) Get(ctx context.Context, p Partition, id string) (Record, error) {
	recs, _, err := s.List(ctx, p)
	if err != nil {
		return Record{}, err
	}
	for _, r := range recs {
		if r.ID == id {
			return r, nil
		}
	}
	return Record{}, storage.ErrNotFound
}

// List returns the partition's readable records by ID with current validity,
// and the keys of records skipped because they are corrupt.
func (s *Store) List(ctx context.Context, p Partition) ([]Record, []string, error) {
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, err := s.load(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	settle(l.recs)
	ids := slices.Sorted(maps.Keys(l.recs))
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, l.recs[id])
	}
	return out, l.corrupt, nil
}

// InvalidateSource marks every record derived from digest stale, and every
// record depending on one of them, transitively. It returns how many records
// became stale.
func (s *Store) InvalidateSource(ctx context.Context, p Partition, digest string) (int, error) {
	return s.invalidate(ctx, p, func(r Record) bool { return slices.Contains(r.SourceDigests, digest) })
}

// InvalidateDerivation marks every record produced by exactly d stale, with
// its dependents. Use it when a tool, model or configuration version changes.
func (s *Store) InvalidateDerivation(ctx context.Context, p Partition, d Derivation) (int, error) {
	return s.invalidate(ctx, p, func(r Record) bool { return r.Derivation == d })
}

func (s *Store) invalidate(ctx context.Context, p Partition, match func(Record) bool) (int, error) {
	if err := p.validate(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load(ctx, p)
	if err != nil {
		return 0, err
	}
	settle(l.recs)
	before := maps.Clone(l.recs)
	for id, r := range l.recs {
		if r.Validity != Stale && match(r) {
			r.Validity = Stale
			l.recs[id] = r
		}
	}
	if _, err := s.commit(ctx, p, l, ""); err != nil && !errors.Is(err, ErrFull) {
		return 0, err
	}
	n := 0
	for id, r := range l.recs {
		if r.Validity == Stale && before[id].Validity != Stale {
			n++
		}
	}
	return n, nil
}

// Resolve settles a conflict on subject in favour of winner: the winner (and
// any record with identical content) becomes valid, the rest become stale.
func (s *Store) Resolve(ctx context.Context, p Partition, subject, winner string) error {
	if err := p.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load(ctx, p)
	if err != nil {
		return err
	}
	settle(l.recs)
	w, ok := l.recs[winner]
	if !ok || w.Subject != subject || (w.Validity != Valid && w.Validity != Conflicted) {
		return fmt.Errorf("memory: %q is not a valid or conflicted record on subject %q", winner, subject)
	}
	now := s.now()
	for id, r := range l.recs {
		if r.Subject != subject || (r.Validity != Valid && r.Validity != Conflicted) {
			continue
		}
		r.Validity = Stale
		if r.Content == w.Content {
			r.Validity, r.ValidatedAt = Valid, now
		}
		l.recs[id] = r
	}
	_, err = s.commit(ctx, p, l, "")
	if errors.Is(err, ErrFull) {
		return nil
	}
	return err
}

// Prune applies retention and bounds without a write. It returns the evicted
// IDs, and ErrFull when referenced records alone exceed the bounds.
func (s *Store) Prune(ctx context.Context, p Partition) ([]string, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load(ctx, p)
	if err != nil {
		return nil, err
	}
	return s.commit(ctx, p, l, "")
}

// DropCorrupt deletes records that fail integrity or decoding so the
// partition can be rebuilt by re-deriving them. It returns the dropped keys.
func (s *Store) DropCorrupt(ctx context.Context, p Partition) ([]string, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.load(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, key := range l.corrupt {
		if err := s.records.Delete(ctx, partitionKey(p), key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("memory: drop corrupt %q: %w", key, err)
		}
	}
	return l.corrupt, nil
}

type loaded struct {
	recs    map[string]Record
	raw     map[string][]byte
	corrupt []string
}

// load reads a partition. Corrupt or foreign entries are skipped and named;
// any other storage failure is returned, never treated as an empty partition.
func (s *Store) load(ctx context.Context, p Partition) (loaded, error) {
	part := partitionKey(p)
	keys, err := s.records.List(ctx, part)
	if err != nil {
		return loaded{}, fmt.Errorf("memory: list: %w", err)
	}
	l := loaded{recs: map[string]Record{}, raw: map[string][]byte{}}
	for _, key := range keys {
		data, err := s.records.Get(ctx, part, key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if errors.Is(err, storage.ErrCorrupt) {
			l.corrupt = append(l.corrupt, key)
			continue
		}
		if err != nil {
			return loaded{}, fmt.Errorf("memory: get %q: %w", key, err)
		}
		r, ok := decode(data, p, key)
		if !ok {
			l.corrupt = append(l.corrupt, key)
			continue
		}
		l.recs[key], l.raw[key] = r, data
	}
	return l, nil
}

// decode accepts only a well-formed record stored under its own ID and
// partition; anything else is corrupt rather than silently reinterpreted.
func decode(data []byte, p Partition, key string) (Record, bool) {
	var r Record
	if strictjson.Decode(data, &r) != nil {
		return Record{}, false
	}
	if r.Validate() != nil || r.ID != key || r.Partition != p {
		return Record{}, false
	}
	return r, true
}

// commit settles, evicts and writes the changed records, the fresh record
// last, then deletes evictions. A crash part-way leaves a state that settle
// still reads safely. With fresh set, unmet bounds refuse before any write.
func (s *Store) commit(ctx context.Context, p Partition, l loaded, fresh string) ([]string, error) {
	settle(l.recs)
	sizes := map[string]int64{}
	for id, r := range l.recs {
		data, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("memory: encode %q: %w", id, err)
		}
		sizes[id] = int64(len(data))
	}
	evict, full := planEvictions(l.recs, sizes, s.protected(p, l.recs, fresh), s.limits, s.now())
	if full != nil && fresh != "" {
		return nil, full
	}
	for _, id := range evict {
		delete(l.recs, id)
	}
	// Re-settling can only turn valid into stale, which encodes to the same size.
	settle(l.recs)
	ids := slices.DeleteFunc(slices.Sorted(maps.Keys(l.recs)), func(id string) bool { return id == fresh })
	if fresh != "" {
		ids = append(ids, fresh)
	}
	part := partitionKey(p)
	for _, id := range ids {
		data, err := json.Marshal(l.recs[id])
		if err != nil {
			return nil, fmt.Errorf("memory: encode %q: %w", id, err)
		}
		if bytes.Equal(data, l.raw[id]) {
			continue
		}
		if err := s.records.Put(ctx, part, id, data); err != nil {
			return nil, fmt.Errorf("memory: write %q: %w", id, err)
		}
	}
	for _, id := range evict {
		if err := s.records.Delete(ctx, part, id); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("memory: evict %q: %w", id, err)
		}
	}
	return evict, full
}

func (s *Store) protected(p Partition, recs map[string]Record, fresh string) map[string]bool {
	pinned := map[string]bool{}
	for id := range recs {
		if s.pins[pinKey(p, id)] > 0 {
			pinned[id] = true
		}
	}
	if fresh != "" {
		pinned[fresh] = true
	}
	return protectedSet(recs, pinned)
}
