// Package controlplane: the Product Workspace (#479).
//
// Two console pages over the same read-only boundary web.go already
// describes: a Product's associated WorkGraphs (the product-oriented
// entry point), and one WorkGraph's units, runs, review state and open
// decisions (the engineering-oriented drill-down). Both reuse
// readProductDetail/readWorkGraphDetail and the identical projections the
// /v1/products and /v1/workgraphs JSON routes already call - never a second
// reader of the rich runtime structs.
package controlplane

import (
	"net/http"
	"time"
)

type productsData struct {
	ObservedAt time.Time
	Products   []ProductSummary
}

func (w *Web) handleProducts(rw http.ResponseWriter, r *http.Request) {
	products, err := w.Store.Products()
	if err != nil {
		w.renderError(rw, err)
		return
	}
	out := make([]ProductSummary, 0, len(products))
	for _, p := range products {
		out = append(out, ProductSummary{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories})
	}
	w.render(rw, productsTemplate, productsData{ObservedAt: w.now(), Products: out})
}

type productDetailData struct {
	ObservedAt time.Time
	Product    ProductDetail
}

func (w *Web) handleProductDetail(rw http.ResponseWriter, r *http.Request) {
	detail, status, _ := readProductDetail(w.Store, w.now(), r.PathValue("id"))
	switch status {
	case 200:
		w.render(rw, productDetailTemplate, productDetailData{ObservedAt: w.now(), Product: detail})
	case 404:
		w.renderNotFound(rw)
	default:
		w.renderError(rw, nil)
	}
}

type workGraphsData struct {
	ObservedAt time.Time
	Graphs     []WorkGraphSummary
}

func (w *Web) handleWorkGraphs(rw http.ResponseWriter, r *http.Request) {
	graphs, err := w.Store.WorkGraphs()
	if err != nil {
		w.renderError(rw, err)
		return
	}
	out := make([]WorkGraphSummary, 0, len(graphs))
	for _, g := range graphs {
		out = append(out, WorkGraphSummary{ID: g.ID, Repository: g.Repository, AgentID: g.AgentID, Name: g.Name, Revision: g.Revision})
	}
	w.render(rw, workGraphsTemplate, workGraphsData{ObservedAt: w.now(), Graphs: out})
}

type workGraphDetailData struct {
	ObservedAt time.Time
	Graph      WorkGraphDetail
}

func (w *Web) handleWorkGraphDetail(rw http.ResponseWriter, r *http.Request) {
	detail, status, _ := readWorkGraphDetail(w.Store, w.now(), r.PathValue("id"))
	switch status {
	case 200:
		w.render(rw, workGraphDetailTemplate, workGraphDetailData{ObservedAt: w.now(), Graph: detail})
	case 404:
		w.renderNotFound(rw)
	default:
		w.renderError(rw, nil)
	}
}
