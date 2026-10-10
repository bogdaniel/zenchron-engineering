package controlplane

import (
	"net/http"

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

// ProductDetail is the Product Workspace's product-oriented entry point: one
// product, its current configuration, and one bounded, database-paged page
// of its associated WorkGraphs as lightweight summaries. It deliberately
// does NOT embed each graph's full detail (units, runs, review state,
// decisions) - that is an unbounded, recursive read this console polls
// every few seconds, and a product with several graphs would multiply it
// per graph on every refresh. Drill into /workgraphs/{id} for one graph's
// full operational detail. GraphsHasMore is true when a page of
// associations beyond GraphsOffset exists - a 201st association is
// reported, never silently dropped.
type ProductDetail struct {
	ID            string                   `json:"product_id"`
	Name          string                   `json:"name"`
	Revision      int                      `json:"revision"`
	Repositories  []string                 `json:"repositories"`
	Configuration *ProductConfigurationDTO `json:"configuration,omitempty"`
	Graphs        []WorkGraphSummary       `json:"graphs"`
	GraphsOffset  int                      `json:"graphs_offset"`
	GraphsHasMore bool                     `json:"graphs_has_more"`
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
	out := ProductDetail{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories, Graphs: []WorkGraphSummary{}}
	if config != nil {
		out.Configuration = &ProductConfigurationDTO{
			Revision: config.Revision, Policy: objectRevisionRef(config.Policy),
			ContextPolicy: config.ContextPolicy, Settings: config.Settings,
		}
	}
	return out
}

// readProductDetail is shared by the JSON route and the page. Each
// associated graph is read as a bounded WorkGraphSummary alone - never the
// full recursive readWorkGraphDetail - so this page's cost stays
// proportional to the page SIZE, not to the sum of every graph's units,
// runs and decisions, and the association read itself is a database-level
// LIMIT/OFFSET, never a full scan. An association naming a graph that
// cannot be read is an integrity violation, not an empty result: it fails
// closed (500), never silently dropped.
func readProductDetail(store *rt.ReadStore, productID string, offset, limit int) (ProductDetail, int, string) {
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
	out.GraphsOffset = offset
	graphIDs, hasMore, err := store.AssociatedGraphs(productID, offset, limit)
	if err != nil {
		return ProductDetail{}, 500, "read_failed"
	}
	out.GraphsHasMore = hasMore
	for _, graphID := range graphIDs {
		g, found, err := store.WorkGraph(graphID)
		if err != nil {
			return ProductDetail{}, 500, "read_failed"
		}
		if !found {
			return ProductDetail{}, 500, "read_failed"
		}
		out.Graphs = append(out.Graphs, WorkGraphSummary{ID: g.ID, Repository: g.Repository, AgentID: g.AgentID, Name: g.Name, Revision: g.Revision})
	}
	return out, 200, ""
}

// Products is the bounded list page /v1/products returns, the same
// offset/limit/has_more shape /v1/runs already uses - backed by a
// database-level LIMIT/OFFSET, never a full table scan trimmed in Go.
type Products struct {
	Products []ProductSummary `json:"products"`
	Offset   int              `json:"offset"`
	HasMore  bool             `json:"has_more"`
}

func (a *API) products(w http.ResponseWriter, r *http.Request) {
	offset, ok := pageNumber(r, "offset", 0, 1<<31-1)
	limit, ok2 := pageNumber(r, "limit", 100, 500)
	if !ok || !ok2 || limit == 0 {
		fail(w, 400, "invalid_page")
		return
	}
	products, hasMore, err := a.Store.Products(int(offset), int(limit))
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := Products{Products: []ProductSummary{}, Offset: int(offset), HasMore: hasMore}
	for _, p := range products {
		out.Products = append(out.Products, ProductSummary{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories})
	}
	send(w, 200, out)
}

// productDetailPage bounds the GET /v1/products/{id} and /products/{id}
// query's graphs_offset/graphs_limit the same way every other bounded page
// here does.
func productDetailPage(r *http.Request) (offset, limit int64, ok bool) {
	offset, ok1 := pageNumber(r, "graphs_offset", 0, 1<<31-1)
	limit, ok2 := pageNumber(r, "graphs_limit", 100, 500)
	return offset, limit, ok1 && ok2 && limit != 0
}

func (a *API) product(w http.ResponseWriter, r *http.Request) {
	offset, limit, ok := productDetailPage(r)
	if !ok {
		fail(w, 400, "invalid_page")
		return
	}
	detail, status, code := readProductDetail(a.Store, r.PathValue("id"), int(offset), int(limit))
	if status != 200 {
		fail(w, status, code)
		return
	}
	send(w, 200, detail)
}
