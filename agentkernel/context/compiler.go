// Package context compiles a reproducible context manifest for one execution.
// Importers alias it (kcontext) to keep the standard library's context clear.
package context

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Estimator counts tokens for one provider; approximate counts say so.
type Estimator func(text string) api.TokenEstimate

// Input is one compilation.
type Input struct {
	// Required items are never dropped, truncated or summarized.
	Required []api.ContextItem
	// Optional items are untrusted candidates selected within remaining capacity.
	Optional       []api.ContextItem
	Window         int64
	ReservedOutput int64
	Estimate       Estimator
}

// Output is the selection plus its manifest.
type Output struct {
	Selected []api.ContextItem
	Manifest api.ContextManifest
}

// InsufficientCapacityError reports mandatory context that cannot fit.
type InsufficientCapacityError struct {
	Needed, Available int64
}

func (e *InsufficientCapacityError) Error() string { return "required context exceeds capacity" }

// ErrNegativeEstimate reports an estimator that returned a negative count.
// A negative count would make context appear to free capacity, so Compile
// refuses it rather than using or clamping it.
var ErrNegativeEstimate = errors.New("context: estimator returned a negative token count")

// Manifest reasons. A duplicate's reason is "duplicate of <id>".
const (
	ReasonRequired          = "required"
	ReasonFits              = "fits"
	ReasonCapacity          = "capacity"
	ReasonDisclosed         = "disclosed by reference"
	ReasonSupersededByLater = "superseded by later content for the same id"
	ReasonShadowedRequired  = "superseded by required item with the same id"
)

// ApproximateTokens is the fallback estimate when no provider estimator is
// supplied: about four bytes per token, always marked approximate.
func ApproximateTokens(text string) api.TokenEstimate {
	return api.TokenEstimate{Count: int64(len(text)+3) / 4, Exact: false}
}

// Compile selects context deterministically. It returns
// *InsufficientCapacityError when required items alone exceed capacity.
//
// Required items keep the host's supplied order, because that order is part of
// the request the host bound. Optional items are ordered by priority tier, then
// Score descending, then ID, so an identical optional set produces an identical
// manifest whatever order it arrives in. When two optional items share an ID
// with different content, the later-supplied one is the refreshed read and the
// earlier one is excluded with an explicit reason. No item's kind, trust or
// content is ever changed; an oversized optional item with a Ref is replaced by
// a pointer item, never by a silent truncation.
func Compile(in Input) (Output, error) {
	if in.Window <= 0 || in.ReservedOutput < 0 {
		return Output{}, fmt.Errorf("context: window must be positive and reserved output non-negative")
	}
	c := newCompilation(in)
	if err := c.addRequired(in.Required); err != nil {
		return Output{}, err
	}
	if c.badEstimate != nil {
		return Output{}, c.badEstimate
	}
	if c.used > c.capacity {
		return Output{}, &InsufficientCapacityError{Needed: c.used, Available: max(c.capacity, 0)}
	}
	candidates, err := c.resolveOptional(in.Optional)
	if err != nil {
		return Output{}, err
	}
	for _, it := range candidates {
		c.addOptional(it)
	}
	if c.badEstimate != nil {
		return Output{}, c.badEstimate
	}
	manifest := api.ContextManifest{
		Entries: c.entries,
		Capacity: api.Capacity{
			Window: in.Window, ReservedOutput: in.ReservedOutput, Used: c.used, Exact: c.exact,
		},
	}
	digest, err := ManifestDigest(manifest)
	if err != nil {
		return Output{}, err
	}
	manifest.Digest = digest
	return Output{Selected: c.selected, Manifest: manifest}, nil
}

type compilation struct {
	estimate Estimator
	capacity int64
	used     int64
	exact    bool
	selected []api.ContextItem
	entries  []api.ManifestEntry
	// owners maps a content digest to the first item that carried it.
	owners      map[string]string
	requiredIDs map[string]bool
	// badEstimate is the first negative estimate; Compile returns it.
	badEstimate error
}

func newCompilation(in Input) *compilation {
	estimate := in.Estimate
	if estimate == nil {
		estimate = ApproximateTokens
	}
	return &compilation{
		estimate:    estimate,
		capacity:    in.Window - in.ReservedOutput,
		exact:       true,
		owners:      map[string]string{},
		requiredIDs: map[string]bool{},
	}
}

func (c *compilation) tokens(text string) api.TokenEstimate {
	t := c.estimate(text)
	if t.Count < 0 {
		if c.badEstimate == nil {
			c.badEstimate = fmt.Errorf("%w (%d)", ErrNegativeEstimate, t.Count)
		}
		// Counted as nothing so no capacity is freed before Compile refuses.
		t.Count = 0
	}
	if !t.Exact {
		c.exact = false
	}
	return t
}

// addRequired selects every required item in full. Capacity is checked by the
// caller after all are counted, so the error reports the whole need.
func (c *compilation) addRequired(items []api.ContextItem) error {
	for i, it := range items {
		if err := it.Validate(); err != nil {
			return fmt.Errorf("context: required[%d]: %w", i, err)
		}
		if c.requiredIDs[it.ID] {
			return fmt.Errorf("context: required[%d]: duplicate id %q", i, it.ID)
		}
		c.requiredIDs[it.ID] = true
		if _, seen := c.owners[it.ContentDigest]; !seen {
			c.owners[it.ContentDigest] = it.ID
		}
		t := c.tokens(it.Content)
		c.used += t.Count
		c.selected = append(c.selected, it)
		c.entries = append(c.entries, entry(it, true, true, ReasonRequired, t))
	}
	return nil
}

// resolveOptional validates optional items, records the ones superseded by a
// later read or by a required item, and returns the rest in priority order.
func (c *compilation) resolveOptional(items []api.ContextItem) ([]api.ContextItem, error) {
	latest := map[string]int{}
	for i, it := range items {
		if err := it.Validate(); err != nil {
			return nil, fmt.Errorf("context: optional[%d]: %w", i, err)
		}
		// A required item passed as optional could be dropped for capacity.
		if it.Required {
			return nil, fmt.Errorf("context: optional[%d]: item %q is marked required", i, it.ID)
		}
		latest[it.ID] = i
	}
	var candidates []api.ContextItem
	var excluded []api.ManifestEntry
	for i, it := range items {
		switch {
		case c.requiredIDs[it.ID]:
			excluded = append(excluded, entry(it, false, false, ReasonShadowedRequired, c.tokens(it.Content)))
		case latest[it.ID] != i:
			excluded = append(excluded, entry(it, false, false, ReasonSupersededByLater, c.tokens(it.Content)))
		default:
			candidates = append(candidates, it)
		}
	}
	slices.SortFunc(excluded, func(a, b api.ManifestEntry) int {
		return cmp.Or(cmp.Compare(a.ItemID, b.ItemID), cmp.Compare(a.ContentDigest, b.ContentDigest))
	})
	c.entries = append(c.entries, excluded...)
	slices.SortFunc(candidates, comparePriority)
	return candidates, nil
}

func (c *compilation) addOptional(it api.ContextItem) {
	t := c.tokens(it.Content)
	if owner, dup := c.owners[it.ContentDigest]; dup {
		c.entries = append(c.entries, entry(it, false, false, "duplicate of "+owner, t))
		return
	}
	c.owners[it.ContentDigest] = it.ID
	if c.used+t.Count <= c.capacity {
		c.used += t.Count
		c.selected = append(c.selected, it)
		c.entries = append(c.entries, entry(it, false, true, ReasonFits, t))
		return
	}
	if it.Ref == nil {
		c.entries = append(c.entries, entry(it, false, false, ReasonCapacity, t))
		return
	}
	pointer := disclosure(it)
	pt := c.tokens(pointer.Content)
	if c.used+pt.Count > c.capacity {
		c.entries = append(c.entries, entry(it, false, false, ReasonCapacity, t))
		return
	}
	c.used += pt.Count
	c.selected = append(c.selected, pointer)
	// The entry keeps the original content digest so the omission stays
	// inspectable; its tokens are what the pointer actually used.
	c.entries = append(c.entries, entry(it, false, true, ReasonDisclosed, pt))
}

// disclosure replaces an oversized optional item with a pointer to its stored
// artifact. ID, kind, trust, revision, score and ref are carried unchanged.
func disclosure(it api.ContextItem) api.ContextItem {
	text := fmt.Sprintf("[%s item %q withheld for capacity; full content is artifact %s (%d bytes, %s)]",
		it.Kind, it.ID, it.Ref.Digest, it.Ref.Size, it.Ref.MediaType)
	pointer := it
	pointer.Content = text
	pointer.ContentDigest = api.Digest([]byte(text))
	return pointer
}

// Tier is the issue §10 priority: host directives and task first, then
// observations, then source/dependency/test, then memory and background.
func Tier(kind api.ContextKind) int {
	switch kind {
	case api.ContextInstruction, api.ContextConstraint, api.ContextTask:
		return 1
	case api.ContextObservation:
		return 2
	case api.ContextSourceCode, api.ContextDependency, api.ContextTest:
		return 3
	default:
		return 4
	}
}

func comparePriority(a, b api.ContextItem) int {
	return cmp.Or(
		cmp.Compare(Tier(a.Kind), Tier(b.Kind)),
		cmp.Compare(b.Score, a.Score),
		cmp.Compare(a.ID, b.ID),
	)
}

func entry(it api.ContextItem, required, selected bool, reason string, t api.TokenEstimate) api.ManifestEntry {
	return api.ManifestEntry{
		ItemID:        it.ID,
		Kind:          it.Kind,
		Trust:         it.Trust,
		Required:      required,
		Selected:      selected,
		Reason:        reason,
		ContentDigest: it.ContentDigest,
		Tokens:        t,
		Ref:           it.Ref,
	}
}

// ManifestDigest is the digest of the canonical JSON encoding of a manifest's
// entries and capacity; the Digest field itself is not an input.
func ManifestDigest(m api.ContextManifest) (string, error) {
	data, err := json.Marshal(struct {
		Entries  []api.ManifestEntry `json:"entries"`
		Capacity api.Capacity        `json:"capacity"`
	}{m.Entries, m.Capacity})
	if err != nil {
		return "", fmt.Errorf("context: encode manifest: %w", err)
	}
	return api.Digest(data), nil
}
