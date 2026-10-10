package controlplane

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// productFixture adopts one product, one unactivated WorkGraph and their
// association, directly through the real store writers (#476/#472) - no live
// supervisor needed, since nothing here is activated.
func productFixture(t *testing.T) *API {
	t.Helper()
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(200, 0).UTC()
	p := product.Product{SchemaVersion: product.ProductSchemaVersion, Revision: 1, Repositories: []string{"acme/repo"}, CreatedAt: at}
	p.Name = "checkout"
	p.ID, err = product.ProductID(p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	proposal := orchestration.WorkGraphProposal{Name: "checkout-graph", Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "a", Purpose: "land the schema", Role: "implementer", Issue: 101},
		{ID: "b", Purpose: "land the reader", Role: "implementer", Issue: 102, DependsOn: []string{"a"}},
	}}
	graph, err := proposal.Compose("acme/repo", "claude", "operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	association, err := product.NewGraphAssociation(p.ID, graph.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssociateWorkGraph(association); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return &API{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"), Now: func() time.Time { return at }}
}

// TestProductAndWorkGraphWireSchemasAndAssociation pins the one projection
// both the JSON route and the console page use: a product lists its
// associated graph with no fabricated run or decision for a unit that was
// never activated, and an unknown product/graph id is refused, never a
// default.
func TestProductAndWorkGraphWireSchemasAndAssociation(t *testing.T) {
	a := productFixture(t)
	for _, tc := range []struct{ path, schema string }{
		{"/v1/products", "product"}, {"/v1/workgraphs", "workgraph"},
	} {
		wire(t, a, tc.path, "GET", a.Token, tc.schema, 200)
	}
	productID, err := product.ProductID("checkout")
	if err != nil {
		t.Fatal(err)
	}
	graphID, err := orchestration.WorkGraphID("acme/repo", "claude", "checkout-graph")
	if err != nil {
		t.Fatal(err)
	}

	detail := wire(t, a, "/v1/products/"+productID, "GET", a.Token, "product-detail", 200)
	if !bytes.Contains(detail, []byte(`"graph_id":"`+graphID+`"`)) {
		t.Fatalf("product detail must carry its associated graph: %s", detail)
	}
	if bytes.Contains(detail, []byte(`"run":`)) {
		t.Fatalf("a never-activated unit must carry no fabricated run: %s", detail)
	}

	graphDetail := wire(t, a, "/v1/workgraphs/"+graphID, "GET", a.Token, "workgraph-detail", 200)
	if !bytes.Contains(graphDetail, []byte(`"product_id":"`+productID+`"`)) {
		t.Fatalf("work graph detail must carry its associated product: %s", graphDetail)
	}
	if !bytes.Contains(graphDetail, []byte(`"unit_id":"a"`)) || !bytes.Contains(graphDetail, []byte(`"unit_id":"b"`)) {
		t.Fatalf("both units must be projected: %s", graphDetail)
	}

	wire(t, a, "/v1/products/unknown", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/workgraphs/unknown", "GET", a.Token, "error", 404)
}
