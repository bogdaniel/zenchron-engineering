package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/product"
)

// TestAdoptProductComposesUnderTheSupervisorsOperatorAndClock pins AdoptProduct
// to its one job: a proposal becomes a Product under the REQUESTER's stated
// identity and the supervisor's own clock, then lands through the same
// AdoptProductRevision invariant its store-level tests already cover - this
// does not re-prove that invariant, only that the command path reaches it.
func TestAdoptProductComposesUnderTheSupervisorsOperatorAndClock(t *testing.T) {
	fixture := newPhase8Fixture(t)
	supervisor := supervisorFixture(t, fixture, 2)

	adopted, err := supervisor.AdoptProduct(ControlRequest{
		Operator: "operator@example",
		Product:  &product.ProductProposal{Name: "checkout", Revision: 1, Repositories: []string{"acme/repo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.RequestedBy != "operator@example" {
		t.Fatalf("product adopted under %q, not the requester", adopted.RequestedBy)
	}
	if adopted.CreatedAt.IsZero() {
		t.Fatal("product adopted with no creation time")
	}

	current, found, err := fixture.store.CurrentProduct(adopted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || current.Revision != 1 {
		t.Fatalf("adopted product is not durable: found=%v current=%+v", found, current)
	}
}

// TestAdoptProductRequiresAProposal mirrors AdoptWorkGraph's own refusal of a
// request that names its command but carries no revision to adopt.
func TestAdoptProductRequiresAProposal(t *testing.T) {
	fixture := newPhase8Fixture(t)
	supervisor := supervisorFixture(t, fixture, 2)

	if _, err := supervisor.AdoptProduct(ControlRequest{Operator: "operator@example"}); err == nil {
		t.Fatal("a product request with no proposal must be refused")
	}
}
