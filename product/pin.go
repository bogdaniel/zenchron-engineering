package product

// ContextPin (#480): the durable identity of the product context one WorkUnit
// execution was admitted with.
//
// CompileContext answers "what may this unit read NOW". A unit that is retried,
// recovered after a restart or replayed must not be answered differently
// because an operator adopted a newer configuration revision or somebody
// promoted another claim in the meantime: the unit was approved against ONE
// context. The pin records exactly that context by identity - the configuration
// revision and its content digest, and the ids of the knowledge entries (each
// itself content-addressed) - never the content, which stays in its own store.
//
// A pin is only ever produced from a CompileContext result, and a later
// execution only ever resolves through Resolve, which compiles again at the
// pinned revision and refuses anything that no longer reproduces the pin.

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// maxPinnedKnowledgeEntries bounds a pin to what CompileContext can return:
// each of the widely promoted, work-unit-audience and feature-audience reads is
// capped at MaxContextKnowledgeEntries.
const maxPinnedKnowledgeEntries = 3 * MaxContextKnowledgeEntries

// WorkUnitOwnerRef is the one exact identity a WorkGraph unit reads knowledge
// as. It is derived from runtime-owned ids, never from anything a model wrote,
// and it is the only value the runtime passes as ContextRequest.OwnerRef.
func WorkUnitOwnerRef(graphID, unitID string) string {
	return graphID + "/" + unitID
}

// ContextPin is the frozen identity of a compiled ProductContext.
type ContextPin struct {
	ProductID string `json:"product_id"`
	// Repositories is the product's repository set at admission. It is part of
	// the context a worker sees, and the product's list can grow, so it is
	// frozen like the configuration and knowledge.
	Repositories []string `json:"repositories"`
	// ConfigurationRevision is the exact revision the unit was admitted with.
	// Zero means NO configuration was adopted at admission: it is a real pin,
	// not "unpinned", so a configuration adopted later never reaches the unit.
	ConfigurationRevision int    `json:"configuration_revision"`
	ConfigurationDigest   string `json:"configuration_digest,omitempty"`
	// KnowledgeIDs is the sorted set of knowledge entries the unit was
	// admitted with. Knowledge only ever grows, so a newer entry is excluded
	// by omission here rather than by the store forgetting it.
	KnowledgeIDs []string `json:"knowledge_ids,omitempty"`
	// ContextDigest is the identity of the fields above. It lets a corrupted
	// or hand-edited pin be refused instead of silently reinterpreted.
	ContextDigest string `json:"context_digest"`
}

// NewContextPin freezes the identity of a compiled context.
func NewContextPin(compiled ProductContext) (ContextPin, error) {
	pin := ContextPin{ProductID: compiled.ProductID, Repositories: sortedCopy(compiled.Repositories)}
	if compiled.Configuration != nil {
		digest, err := compiled.Configuration.RevisionDigest()
		if err != nil {
			return ContextPin{}, err
		}
		pin.ConfigurationRevision, pin.ConfigurationDigest = compiled.Configuration.Revision, digest
	}
	for _, entry := range compiled.Knowledge {
		pin.KnowledgeIDs = append(pin.KnowledgeIDs, entry.ID)
	}
	sort.Strings(pin.KnowledgeIDs)
	pin.KnowledgeIDs = slices.Compact(pin.KnowledgeIDs)
	digest, err := pin.identity()
	if err != nil {
		return ContextPin{}, err
	}
	pin.ContextDigest = digest
	return pin, pin.Validate()
}

func (p ContextPin) identity() (string, error) {
	p.ContextDigest = ""
	return domain.Digest(p)
}

// Validate refuses a pin that is not exactly what its own digest says.
func (p ContextPin) Validate() error {
	if strings.TrimSpace(p.ProductID) == "" {
		return errors.New("a product context pin names its product")
	}
	if len(p.Repositories) == 0 || len(p.Repositories) > MaxProductRepositories {
		return fmt.Errorf("product context pin names %d repositories, outside 1..%d", len(p.Repositories), MaxProductRepositories)
	}
	for i, repository := range p.Repositories {
		if strings.TrimSpace(repository) == "" || (i > 0 && repository <= p.Repositories[i-1]) {
			return errors.New("product context pin repositories are sorted, distinct and non-empty")
		}
	}
	if p.ConfigurationRevision < 0 {
		return fmt.Errorf("product context pin configuration revision %d is not a revision", p.ConfigurationRevision)
	}
	if (p.ConfigurationRevision == 0) != (p.ConfigurationDigest == "") {
		return errors.New("a product context pin names a configuration digest if and only if it names a configuration revision")
	}
	if len(p.KnowledgeIDs) > maxPinnedKnowledgeEntries {
		return fmt.Errorf("product context pin names %d knowledge entries, above the %d entry bound", len(p.KnowledgeIDs), maxPinnedKnowledgeEntries)
	}
	for i, id := range p.KnowledgeIDs {
		if strings.TrimSpace(id) == "" || (i > 0 && id <= p.KnowledgeIDs[i-1]) {
			return errors.New("product context pin knowledge ids are sorted, distinct and non-empty")
		}
	}
	digest, err := p.identity()
	if err != nil {
		return err
	}
	if digest != p.ContextDigest {
		return fmt.Errorf("product context pin digest %q does not match its contents", p.ContextDigest)
	}
	return nil
}

// Resolve compiles the context this pin was admitted with. It goes through
// CompileContext - the one owner of visibility - at the pinned configuration
// revision, then refuses unless the result is exactly the pinned identity.
//
// A newer knowledge entry is dropped; a pinned entry the unit can no longer
// read, or a configuration whose content no longer matches its pinned digest,
// is an error and never an emptier context.
func (p ContextPin) Resolve(store Store, request ContextRequest) (ProductContext, error) {
	if err := p.Validate(); err != nil {
		return ProductContext{}, err
	}
	if request.ProductID != p.ProductID {
		return ProductContext{}, fmt.Errorf("context pin is for product %q, not %q", p.ProductID, request.ProductID)
	}
	revision := p.ConfigurationRevision
	request.ConfigurationRevision = &revision
	compiled, err := CompileContext(store, request)
	if err != nil {
		return ProductContext{}, err
	}
	// Repositories are append-only, so the current list must still contain
	// every pinned one; the worker sees the pinned set, not the grown one.
	for _, repository := range p.Repositories {
		if !slices.Contains(compiled.Repositories, repository) {
			return ProductContext{}, fmt.Errorf("product %q no longer contains pinned repository %q", p.ProductID, repository)
		}
	}
	compiled.Repositories = slices.Clone(p.Repositories)
	pinned := make(map[string]bool, len(p.KnowledgeIDs))
	for _, id := range p.KnowledgeIDs {
		pinned[id] = true
	}
	var kept []KnowledgeEntry
	for _, entry := range compiled.Knowledge {
		if pinned[entry.ID] {
			kept = append(kept, entry)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].ID < kept[j].ID })
	compiled.Knowledge = kept
	reproduced, err := NewContextPin(compiled)
	if err != nil {
		return ProductContext{}, err
	}
	if reproduced.ContextDigest != p.ContextDigest {
		return ProductContext{}, fmt.Errorf("product %q context no longer reproduces its pinned identity %s", p.ProductID, p.ContextDigest)
	}
	return compiled, nil
}
