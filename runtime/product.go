package runtime

// Product intake (#476): the explicit gate a proposed Product revision passes
// before anything executes against it.
//
// Unlike a WorkGraph, adopting a product spends no agent capacity and starts
// nothing: it names which repositories belong together. Composition and the
// full revision-transition invariant are ProductProposal.Compose and
// AdoptProductRevision's own job (runtime/product_store.go); this method adds
// no gate a caller could get wrong by skipping it.

import (
	"errors"

	"github.com/bogdaniel/zenchron-engineering/product"
)

// AdoptProduct admits one proposed Product revision under the operator who
// made the request and the supervisor's own clock.
func (s *Supervisor) AdoptProduct(request ControlRequest) (product.Product, error) {
	if request.Product == nil {
		return product.Product{}, errors.New("a product request carries the proposed revision")
	}
	proposed, err := request.Product.Compose(request.Operator, s.deps.Clock.Now())
	if err != nil {
		return product.Product{}, err
	}
	adopted, _, err := s.deps.Store.AdoptProductRevision(proposed)
	if err != nil {
		return product.Product{}, err
	}
	return adopted, nil
}
