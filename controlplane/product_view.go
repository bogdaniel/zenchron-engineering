package controlplane

import (
	"net/http"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/product"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// ProductSummary is one Product (#476) as a list, or a WorkGraph's
// association, sees it.
type ProductSummary struct {
	ID           string   `json:"product_id"`
	Name         string   `json:"name"`
	Revision     int      `json:"revision"`
	Repositories []string `json:"repositories"`
}

// ProductConfigurationDTO carries the product's current mutable settings -
// references only, never policy/context-policy content (#476 stores no
// content of its own).
type ProductConfigurationDTO struct {
	Revision      int               `json:"revision"`
	Policy        *RefDTO           `json:"policy,omitempty"`
	ContextPolicy string            `json:"context_policy,omitempty"`
	Settings      map[string]string `json:"settings,omitempty"`
}

// ProductDetail is the Product Workspace's product-oriented entry point:
// one product, every fact its associated WorkGraphs already project
// (units, workers, review state, open decisions, candidates, budgets), and
// its current configuration - so a product owner answers #479's questions
// from this one page, never required to drill into /workgraphs/{id} first.
type ProductDetail struct {
	ID            string                   `json:"product_id"`
	Name          string                   `json:"name"`
	Revision      int                      `json:"revision"`
	Repositories  []string                 `json:"repositories"`
	Configuration *ProductConfigurationDTO `json:"configuration,omitempty"`
	Graphs        []WorkGraphDetail        `json:"graphs"`
}

func objectRevisionRef(r *domain.ObjectRevision) *RefDTO {
	if r == nil || r.ID == "" {
		return nil
	}
	return &RefDTO{ID: r.ID, Revision: r.Revision}
}

// productDetailProjection assembles one product's own fields (never its
// graphs - those need store reads of their own, see readProductDetail). It
// is pure and unit-testable without a store.
func productDetailProjection(p product.Product, config *product.ProductConfiguration) ProductDetail {
	out := ProductDetail{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories, Graphs: []WorkGraphDetail{}}
	if config != nil {
		out.Configuration = &ProductConfigurationDTO{
			Revision: config.Revision, Policy: objectRevisionRef(config.Policy),
			ContextPolicy: config.ContextPolicy, Settings: config.Settings,
		}
	}
	return out
}

// readProductDetail is shared by the JSON route and the page. Each
// associated graph's full detail is the same projection
// readWorkGraphDetail/the /v1/workgraphs/{id} route already builds - never a
// second, thinner reader of the same WorkGraph.
func readProductDetail(store *rt.ReadStore, now time.Time, productID string) (ProductDetail, int, string) {
	p, found, err := store.CurrentProduct(productID)
	if err != nil {
		return ProductDetail{}, 500, "read_failed"
	}
	if !found {
		return ProductDetail{}, 404, "not_found"
	}
	var config *product.ProductConfiguration
	if c, found, err := store.CurrentConfiguration(productID); err != nil {
		return ProductDetail{}, 500, "read_failed"
	} else if found {
		config = &c
	}
	out := productDetailProjection(p, config)
	graphIDs, err := store.AssociatedGraphs(productID)
	if err != nil {
		return ProductDetail{}, 500, "read_failed"
	}
	for _, graphID := range graphIDs {
		graph, status, code := readWorkGraphDetail(store, now, graphID)
		if status != 200 {
			return ProductDetail{}, status, code
		}
		out.Graphs = append(out.Graphs, graph)
	}
	return out, 200, ""
}

func (a *API) products(w http.ResponseWriter, r *http.Request) {
	products, err := a.Store.Products()
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := make([]ProductSummary, 0, len(products))
	for _, p := range products {
		out = append(out, ProductSummary{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories})
	}
	send(w, 200, out)
}

func (a *API) product(w http.ResponseWriter, r *http.Request) {
	detail, status, code := readProductDetail(a.Store, a.now(), r.PathValue("id"))
	if status != 200 {
		fail(w, status, code)
		return
	}
	send(w, 200, detail)
}
