package product

// Product-scoped context compilation (#476): the narrow read path a WorkUnit
// uses to get the minimum product context relevant to it, without loading any
// other product's state, another owner's raw discoveries, or a mutable
// configuration default in place of the exact revision it was frozen to.
//
// Every Store method takes the product id CompileContext was given, and
// CompileContext passes that SAME id to every one of them - there is no step
// here that can read a sibling product's row. Knowledge visibility is split
// into two separate reads on purpose (OwnedKnowledge, PromotedKnowledgeAtOrBelow):
// a single "scope_rank <= requested" read would let a product-scoped request
// see every unpromoted agent discovery in the product, which is exactly the
// leak #476's acceptance refuses.

import (
	"errors"
	"fmt"
	"strings"
)

// MaxContextKnowledgeEntries bounds how many knowledge entries one compiled
// context carries, on each of the owned and promoted reads. A product's
// knowledge is unbounded over its lifetime; the context a single caller
// compiles is not, so a store with many entries cannot produce an unbounded
// agent context merely because no caller asked for a narrower slice.
const MaxContextKnowledgeEntries = 200

// Store is the narrow persistence boundary CompileContext reads through. It
// is satisfied by runtime's SQLite-backed implementation and faked in tests
// without a database (the #6(b) I/O boundary gate).
type Store interface {
	// CurrentProduct returns a product's highest adopted revision.
	CurrentProduct(productID string) (Product, bool, error)
	// CurrentConfiguration returns a product's highest adopted configuration
	// revision. found is false for a product with no configuration adopted yet
	// - that is not an error, since configuration is the optional mutable half
	// of #476.
	CurrentConfiguration(productID string) (ProductConfiguration, bool, error)
	// ConfigurationRevision returns one EXACT configuration revision, so a
	// caller compiling context for a WorkUnit already bound to revision N can
	// ask for N by number rather than whatever is current.
	ConfigurationRevision(productID string, revision int) (ProductConfiguration, bool, error)
	// OwnedKnowledge returns ONLY the directly authored entries at or below
	// scope that belong to exactly ownerRef, in this product. It never
	// returns a promoted entry (a promoted entry has no owner) and never
	// another owner's entry, however wide scope is.
	OwnedKnowledge(productID string, scope Scope, ownerRef string) ([]KnowledgeEntry, error)
	// PromotedKnowledgeAtOrBelow returns ONLY entries that went through
	// Promote, at or below scope, in this product. It never returns a
	// directly authored, unpromoted entry.
	PromotedKnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error)
}

// ContextRequest is what a caller - ultimately a WorkUnit's execution context
// compiler - asks for: one product, the narrowest boundary it is compiling
// context for, and which exact owner and configuration revision it is.
type ContextRequest struct {
	ProductID string
	Scope     Scope
	// OwnerRef identifies the exact execution or work unit this request is
	// for. It is required when Scope is ScopeExecution or ScopeWorkUnit - the
	// only scopes a raw discovery can exist at - and ignored above that,
	// since nothing is ever directly authored at feature scope or wider.
	OwnerRef string
	// ConfigurationRevision pins the EXACT configuration revision a caller
	// already froze into its own contract (domain.ContractProvenance). A
	// caller with no frozen binding yet - nothing to pin - leaves it nil and
	// gets the product's current configuration. A caller that DOES have one
	// must pass it: substituting "current" for a bound WorkUnit's frozen
	// revision is the exact drift #476's acceptance refuses.
	ConfigurationRevision *int
}

// ProductContext is the minimum relevant slice CompileContext returns:
// references and the product's own configuration and knowledge, never
// another product's state, never another owner's raw discovery, and never a
// run, plan or evidence body.
type ProductContext struct {
	ProductID     string                `json:"product_id"`
	Scope         Scope                 `json:"scope"`
	Repositories  []string              `json:"repositories"`
	Configuration *ProductConfiguration `json:"configuration,omitempty"`
	Knowledge     []KnowledgeEntry      `json:"knowledge,omitempty"`
}

// CompileContext is the ONE place a product-scoped context is assembled. A
// missing product, or a missing PINNED configuration revision, is an error,
// never an empty context: a caller that named the wrong product id or a
// revision that no longer exists must fail closed, not silently see nothing
// or silently see something else instead.
func CompileContext(store Store, request ContextRequest) (ProductContext, error) {
	if strings.TrimSpace(request.ProductID) == "" {
		return ProductContext{}, errors.New("a context request needs a product id")
	}
	if !KnownScope(request.Scope) {
		return ProductContext{}, fmt.Errorf("context request scope %q is not a known scope", request.Scope)
	}
	ownsRawScope := request.Scope == ScopeExecution || request.Scope == ScopeWorkUnit
	if ownsRawScope && strings.TrimSpace(request.OwnerRef) == "" {
		return ProductContext{}, fmt.Errorf("a %q-scoped context request needs its owner reference", request.Scope)
	}

	current, found, err := store.CurrentProduct(request.ProductID)
	if err != nil {
		return ProductContext{}, err
	}
	if !found {
		return ProductContext{}, fmt.Errorf("product %q does not exist", request.ProductID)
	}
	context := ProductContext{ProductID: current.ID, Scope: request.Scope, Repositories: current.Repositories}

	configuration, configurationFound, err := resolveConfiguration(store, current.ID, request.ConfigurationRevision)
	if err != nil {
		return ProductContext{}, err
	}
	if configurationFound {
		context.Configuration = &configuration
	}

	var knowledge []KnowledgeEntry
	if ownsRawScope {
		owned, err := store.OwnedKnowledge(current.ID, request.Scope, request.OwnerRef)
		if err != nil {
			return ProductContext{}, err
		}
		knowledge = append(knowledge, owned...)
	}
	promoted, err := store.PromotedKnowledgeAtOrBelow(current.ID, request.Scope)
	if err != nil {
		return ProductContext{}, err
	}
	context.Knowledge = append(knowledge, promoted...)
	return context, nil
}

func resolveConfiguration(store Store, productID string, pinned *int) (ProductConfiguration, bool, error) {
	if pinned == nil {
		return store.CurrentConfiguration(productID)
	}
	configuration, found, err := store.ConfigurationRevision(productID, *pinned)
	if err != nil {
		return ProductConfiguration{}, false, err
	}
	if !found {
		return ProductConfiguration{}, false, fmt.Errorf("product %q has no configuration revision %d", productID, *pinned)
	}
	return configuration, true, nil
}
