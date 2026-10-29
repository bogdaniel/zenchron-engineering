package memory

import (
	"cmp"
	"errors"
	"slices"
	"time"
)

// ErrFull reports a write refused because the partition bounds cannot be met
// without evicting records that active work still references.
var ErrFull = errors.New("memory: partition bounds cannot be met without evicting referenced records")

// settle derives every record's current validity from the others. It runs on
// every read and before every write, so a crash between the writes of one
// update can never leave a contradiction or a stale basis surfacing as valid.
//
// Conflicts: valid or conflicted records sharing a subject with differing
// content all become conflicted; they stay conflicted until Resolve.
// Dependencies: a valid record whose dependency is missing or not valid
// becomes stale, transitively.
func settle(recs map[string]Record) {
	contents := map[string]map[string]bool{}
	for _, r := range recs {
		if r.Subject == "" || (r.Validity != Valid && r.Validity != Conflicted) {
			continue
		}
		if contents[r.Subject] == nil {
			contents[r.Subject] = map[string]bool{}
		}
		contents[r.Subject][r.Content] = true
	}
	for id, r := range recs {
		if r.Validity == Valid && len(contents[r.Subject]) > 1 {
			r.Validity = Conflicted
			recs[id] = r
		}
	}
	// ponytail: fixpoint rescans all records, O(n·depth); fine at partition bounds of a few thousand.
	for changed := true; changed; {
		changed = false
		for id, r := range recs {
			if r.Validity == Valid && !basisValid(recs, r) {
				r.Validity = Stale
				recs[id] = r
				changed = true
			}
		}
	}
}

func basisValid(recs map[string]Record, r Record) bool {
	for _, dep := range r.DependsOn {
		if d, ok := recs[dep]; !ok || d.Validity != Valid {
			return false
		}
	}
	return true
}

// protectedSet is every pinned record plus everything it depends on, so the
// provenance of records still referenced by active work is never evicted.
func protectedSet(recs map[string]Record, pinned map[string]bool) map[string]bool {
	protected := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if protected[id] {
			return
		}
		protected[id] = true
		for _, dep := range recs[id].DependsOn {
			visit(dep)
		}
	}
	for id := range pinned {
		visit(id)
	}
	return protected
}

// planEvictions returns the records to delete: unprotected records past
// retention age, then unprotected records until the bounds hold, preferring
// records that are no longer valid, then the oldest, then by ID. When the
// bounds still cannot hold it returns what it could evict and ErrFull.
func planEvictions(recs map[string]Record, sizes map[string]int64, protected map[string]bool,
	limits Limits, now time.Time) ([]string, error) {
	var evict, candidates []string
	count, total := len(recs), int64(0)
	for _, s := range sizes {
		total += s
	}
	for id, r := range recs {
		if protected[id] {
			continue
		}
		if limits.MaxAge > 0 && now.Sub(r.CreatedAt) > limits.MaxAge {
			evict = append(evict, id)
			count--
			total -= sizes[id]
			continue
		}
		candidates = append(candidates, id)
	}
	slices.SortFunc(candidates, func(a, b string) int {
		ra, rb := recs[a], recs[b]
		return cmp.Or(
			cmp.Compare(boolRank(ra.Validity == Valid), boolRank(rb.Validity == Valid)),
			ra.CreatedAt.Compare(rb.CreatedAt),
			cmp.Compare(a, b),
		)
	})
	for _, id := range candidates {
		if count <= limits.MaxRecords && total <= limits.MaxBytes {
			break
		}
		evict = append(evict, id)
		count--
		total -= sizes[id]
	}
	slices.Sort(evict)
	if count > limits.MaxRecords || total > limits.MaxBytes {
		return evict, ErrFull
	}
	return evict, nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
