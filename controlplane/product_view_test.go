package controlplane

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/product"
)

// TestProductDetailProjectionCarriesConfigurationAndNoConfiguration pins the
// two configuration branches: a current configuration's policy/context
// references are carried through, and a product with none adopted yet shows
// an explicit absence rather than a zero-valued configuration object.
func TestProductDetailProjectionCarriesConfigurationAndNoConfiguration(t *testing.T) {
	p := product.Product{ID: "product-1", Name: "checkout", Revision: 2, Repositories: []string{"acme/repo"}}
	config := &product.ProductConfiguration{
		Revision: 3, Policy: &domain.ObjectRevision{ID: "policy-1", Revision: "r1"},
		ContextPolicy: "context-policy-1", Settings: map[string]string{"tier": "default"},
	}

	withConfig := productDetailProjection(p, config)
	if withConfig.Configuration == nil || withConfig.Configuration.Policy == nil || withConfig.Configuration.Policy.ID != "policy-1" {
		t.Fatalf("a current configuration's policy reference must be carried through: %+v", withConfig.Configuration)
	}

	withoutConfig := productDetailProjection(p, nil)
	if withoutConfig.Configuration != nil {
		t.Fatalf("no configuration adopted yet must stay nil, not a zero-valued object: %+v", withoutConfig.Configuration)
	}

	out := renderTemplate(t, productDetailTemplate, productDetailData{Product: withoutConfig})
	for _, want := range []string{"checkout", "acme/repo", "no configuration adopted yet", "no work graph is associated"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered product detail lacks %q", want)
		}
	}
}

// TestProductDetailTemplateRendersAssociatedGraph exercises the lightweight
// associated-graph list a product owner reads before drilling into
// /workgraphs/{id} - this page never embeds a graph's own units/runs, which
// is exactly what keeps it cheap to refresh regardless of graph size.
func TestProductDetailTemplateRendersAssociatedGraph(t *testing.T) {
	data := productDetailData{Product: ProductDetail{
		ID: "product-1", Name: "checkout", Repositories: []string{"acme/repo"},
		Graphs: []WorkGraphSummary{{ID: "graph-1", Name: "checkout graph", Repository: "acme/repo", AgentID: "claude", Revision: 1}},
	}}
	out := renderTemplate(t, productDetailTemplate, data)
	for _, want := range []string{"checkout graph", `href="/workgraphs/graph-1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered product detail lacks %q", want)
		}
	}
}
