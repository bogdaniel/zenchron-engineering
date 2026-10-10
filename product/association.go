package product

// GraphAssociation (#476): the fact that one existing WorkGraph (#472) resolves
// its product-scoped context through one Product. It is an append-only fact,
// never a revision - associating a graph changes neither the graph nor the
// product, and a graph belongs to at most one product so a unit's context
// compilation can never be ambiguous about which product it is reading.

import (
	"errors"
	"strings"
	"time"
)

// GraphAssociation links a WorkGraph id to the Product its units resolve
// context against.
type GraphAssociation struct {
	ProductID    string    `json:"product_id"`
	GraphID      string    `json:"graph_id"`
	AssociatedAt time.Time `json:"associated_at"`
}

// NewGraphAssociation validates the fields a caller proposes before anything
// persists them. Idempotency and the one-product-per-graph rule are the
// store's primary-key guarantee (runtime/product_store.go), not this
// function's: two different association attempts for the same graph are a
// store-level conflict, never a question this constructor can answer alone.
func NewGraphAssociation(productID, graphID string, at time.Time) (GraphAssociation, error) {
	if strings.TrimSpace(productID) == "" {
		return GraphAssociation{}, errors.New("a graph association needs a product id")
	}
	if strings.TrimSpace(graphID) == "" {
		return GraphAssociation{}, errors.New("a graph association needs a work graph id")
	}
	if at.IsZero() {
		return GraphAssociation{}, errors.New("a graph association needs its association time")
	}
	return GraphAssociation{ProductID: productID, GraphID: graphID, AssociatedAt: at}, nil
}
