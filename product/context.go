package product

// Product-scoped context compilation (#476): the narrow read path a WorkUnit
// uses to get the minimum product context relevant to it, without loading any
// other product's state. Every method Store exposes takes the product id it
// is scoped to, and CompileContext passes the SAME id to every one of them -
// there is no step in this file that can read a sibling product's row.

import (
	"errors"
	"fmt"
	"strings"
)

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
	// KnowledgeAtOrBelow returns one product's knowledge entries whose scope is
	// at or below the given boundary, and ONLY that product's entries.
	KnowledgeAtOrBelow(productID string, scope Scope) ([]KnowledgeEntry, error)
}

// ContextRequest is what a caller - ultimately a WorkUnit's execution context
// compiler - asks for: one product, and the narrowest boundary it is
// compiling context for.
type ContextRequest struct {
	ProductID string
	Scope     Scope
}

// ProductContext is the minimum relevant slice CompileContext returns:
// references and the product's own current configuration and knowledge,
// never another product's state and never a run, plan or evidence body.
type ProductContext struct {
	ProductID     string                `json:"product_id"`
	Scope         Scope                 `json:"scope"`
	Repositories  []string              `json:"repositories"`
	Configuration *ProductConfiguration `json:"configuration,omitempty"`
	Knowledge     []KnowledgeEntry      `json:"knowledge,omitempty"`
}

// CompileContext is the ONE place a product-scoped context is assembled. A
// missing product is an error, never an empty context: a WorkUnit that named
// the wrong product id must fail closed, not silently see nothing.
func CompileContext(store Store, request ContextRequest) (ProductContext, error) {
	if strings.TrimSpace(request.ProductID) == "" {
		return ProductContext{}, errors.New("a context request needs a product id")
	}
	if !KnownScope(request.Scope) {
		return ProductContext{}, fmt.Errorf("context request scope %q is not a known scope", request.Scope)
	}
	current, found, err := store.CurrentProduct(request.ProductID)
	if err != nil {
		return ProductContext{}, err
	}
	if !found {
		return ProductContext{}, fmt.Errorf("product %q does not exist", request.ProductID)
	}
	context := ProductContext{ProductID: current.ID, Scope: request.Scope, Repositories: current.Repositories}
	configuration, found, err := store.CurrentConfiguration(current.ID)
	if err != nil {
		return ProductContext{}, err
	}
	if found {
		context.Configuration = &configuration
	}
	knowledge, err := store.KnowledgeAtOrBelow(current.ID, request.Scope)
	if err != nil {
		return ProductContext{}, err
	}
	context.Knowledge = knowledge
	return context, nil
}
