package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// DegradedItemID names the notice item emitted when corrupt records were
// skipped, so the degradation is visible in the context manifest. It cannot
// collide with a record item, which always starts with ItemPrefix.
const DegradedItemID = "memory-degraded"

// Source returns a context source bound to exactly one partition.
func (s *Store) Source(p Partition) api.ContextSource { return source{store: s, partition: p} }

type source struct {
	store     *Store
	partition Partition
}

// ContextItems returns only currently valid records, as optional memory items
// with memory trust whatever their content claims. Stale, conflicted and
// unknown records are excluded entirely rather than surfaced with a marking.
// Items are ordered by known confidence descending, then ID; Limit, when
// positive, keeps the first Limit of them.
func (v source) ContextItems(ctx context.Context, q api.ContextQuery) ([]api.ContextItem, error) {
	recs, corrupt, err := v.store.List(ctx, v.partition)
	if err != nil {
		return nil, err
	}
	recs = slices.DeleteFunc(recs, func(r Record) bool { return r.Validity != Valid })
	slices.SortFunc(recs, func(a, b Record) int {
		return cmp.Or(cmp.Compare(score(b), score(a)), cmp.Compare(a.ID, b.ID))
	})
	if q.Limit > 0 && len(recs) > q.Limit {
		recs = recs[:q.Limit]
	}
	items := make([]api.ContextItem, 0, len(recs)+1)
	for _, r := range recs {
		items = append(items, toItem(r))
	}
	if len(corrupt) > 0 {
		text := fmt.Sprintf("memory degraded: %d unreadable record(s) skipped: %s", len(corrupt), strings.Join(corrupt, ", "))
		items = append(items, memoryItem(DegradedItemID, text, ""))
	}
	return items, nil
}

func score(r Record) float64 {
	if r.Confidence == nil {
		return 0
	}
	return *r.Confidence
}

// toItem renders a record as untrusted data. The header states kind,
// derivation and confidence so an inference is never read as an observation.
func toItem(r Record) api.ContextItem {
	confidence := "unknown"
	if r.Confidence != nil {
		confidence = fmt.Sprintf("%.2f", *r.Confidence)
	}
	d := r.Derivation
	text := fmt.Sprintf("[remembered %s; derived by %s/%s/%s@%s; confidence %s]\n%s",
		r.Kind, d.Method, d.Tool, d.Model, d.Version, confidence, r.Content)
	sources := slices.Sorted(slices.Values(r.SourceDigests))
	it := memoryItem(ItemPrefix+r.ID, text, api.Digest([]byte(strings.Join(sources, "\n"))))
	it.Score = score(r)
	return it
}

func memoryItem(id, text, revision string) api.ContextItem {
	return api.ContextItem{
		ID:            id,
		Kind:          api.ContextMemory,
		Trust:         api.TrustMemory,
		Content:       text,
		ContentDigest: api.Digest([]byte(text)),
		Revision:      revision,
	}
}
