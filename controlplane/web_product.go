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
	"fmt"
	"net/http"
	"time"
)

// webPageSize is this console's one page size: every bounded list here -
// /products, /workgraphs, and one product's associated-graph list - pages
// at this size, so there is one mental model instead of a per-page default.
const webPageSize = 100

// pageOffset reads ?offset= (default 0), bounded the same way every other
// page on this boundary is.
func pageOffset(r *http.Request, key string) (offset int64, ok bool) {
	return pageNumber(r, key, 0, 1<<31-1)
}

// pageLinks builds the prev/next link for a bounded list page: path is the
// bare route ("/products"), key is the SAME query parameter name the
// handler reads back with pageOffset (an offset link whose key the handler
// does not read is a link to the first page, silently - see #550 review),
// offset is the CURRENT page's, and hasMore says whether a next page
// exists. An empty string means no link.
func pageLinks(path, key string, offset int64, hasMore bool) (prev, next string) {
	link := func(o int64) string {
		if o == 0 {
			return path
		}
		return fmt.Sprintf("%s?%s=%d", path, key, o)
	}
	if offset > 0 {
		prev = link(max(0, offset-webPageSize))
	}
	if hasMore {
		next = link(offset + webPageSize)
	}
	return prev, next
}

type productsData struct {
	ObservedAt time.Time
	Products   []ProductSummary
	Offset     int64
	PrevLink   string
	NextLink   string
}

func (w *Web) handleProducts(rw http.ResponseWriter, r *http.Request) {
	offset, ok := pageOffset(r, "offset")
	if !ok {
		w.renderNotFound(rw)
		return
	}
	products, hasMore, err := w.Store.Products(int(offset), webPageSize)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	out := make([]ProductSummary, 0, len(products))
	for _, p := range products {
		out = append(out, ProductSummary{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories})
	}
	prev, next := pageLinks("/products", "offset", offset, hasMore)
	w.render(rw, productsTemplate, productsData{ObservedAt: w.now(), Products: out, Offset: offset, PrevLink: prev, NextLink: next})
}

type productDetailData struct {
	ObservedAt time.Time
	Product    ProductDetail
	PrevLink   string
	NextLink   string
}

func (w *Web) handleProductDetail(rw http.ResponseWriter, r *http.Request) {
	offset, ok := pageOffset(r, "graphs_offset")
	if !ok {
		w.renderNotFound(rw)
		return
	}
	detail, status, _ := readProductDetail(w.Store, r.PathValue("id"), int(offset), webPageSize)
	switch status {
	case 200:
		prev, next := pageLinks("/products/"+detail.ID, "graphs_offset", offset, detail.GraphsHasMore)
		w.render(rw, productDetailTemplate, productDetailData{ObservedAt: w.now(), Product: detail, PrevLink: prev, NextLink: next})
	case 404:
		w.renderNotFound(rw)
	default:
		w.renderError(rw, nil)
	}
}

type workGraphsData struct {
	ObservedAt time.Time
	Graphs     []WorkGraphSummary
	Offset     int64
	PrevLink   string
	NextLink   string
}

func (w *Web) handleWorkGraphs(rw http.ResponseWriter, r *http.Request) {
	offset, ok := pageOffset(r, "offset")
	if !ok {
		w.renderNotFound(rw)
		return
	}
	graphs, hasMore, err := w.Store.WorkGraphs(int(offset), webPageSize)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	out := make([]WorkGraphSummary, 0, len(graphs))
	for _, g := range graphs {
		out = append(out, WorkGraphSummary{ID: g.ID, Repository: g.Repository, AgentID: g.AgentID, Name: g.Name, Revision: g.Revision})
	}
	prev, next := pageLinks("/workgraphs", "offset", offset, hasMore)
	w.render(rw, workGraphsTemplate, workGraphsData{ObservedAt: w.now(), Graphs: out, Offset: offset, PrevLink: prev, NextLink: next})
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
