package controlplane

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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

// TestProductAndWorkGraphListsPaginateWithStableBoundaries proves F4's
// second and third gaps are closed: both JSON list routes page in stable,
// non-overlapping, exhaustive boundaries (never materializing more than
// limit+1 rows per call, since the bound is a SQL LIMIT/OFFSET - see
// Products/WorkGraphsPage), reject an invalid or over-bound limit exactly
// like /v1/runs already does, and the HTML console pages carry working
// previous/next navigation across real multiple pages instead of rendering
// every row unbounded.
func TestProductAndWorkGraphListsPaginateWithStableBoundaries(t *testing.T) {
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(500, 0).UTC()
	// One more than webPageSize, so the console's own fixed page size (which
	// takes no limit query parameter) genuinely spans two pages too - not
	// only the JSON routes, which can be exercised at any limit.
	const total = webPageSize + 1
	for i := range total {
		at := at.Add(time.Duration(i) * time.Second)
		name := fmt.Sprintf("product-%03d", i)
		p := product.Product{SchemaVersion: product.ProductSchemaVersion, Revision: 1, Repositories: []string{"acme/repo"}, CreatedAt: at}
		p.Name = name
		if p.ID, err = product.ProductID(name); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.AdoptProductRevision(p); err != nil {
			t.Fatal(err)
		}
		graph, err := orchestration.WorkGraphProposal{Name: fmt.Sprintf("graph-%03d", i), Revision: 1, Units: []orchestration.WorkUnit{
			{ID: "a", Purpose: "do the work", Role: "implementer", Issue: 1000 + i},
		}}.Compose("acme/repo", "claude", "operator@example", at)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.AdoptWorkGraphRevision(graph); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	a := &API{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"), Now: func() time.Time { return at }}
	w := webFrom(a)

	for _, route := range []struct{ path, idField, schema string }{
		{"/v1/products", "product_id", "product"}, {"/v1/workgraphs", "graph_id", "workgraph"},
	} {
		seen := map[string]bool{}
		offset, pages := 0, 0
		listKey := "products"
		if route.idField == "graph_id" {
			listKey = "workgraphs"
		}
		for {
			body := wire(t, a, fmt.Sprintf("%s?offset=%d&limit=50", route.path, offset), "GET", a.Token, route.schema, 200)
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			var hasMore bool
			if err := json.Unmarshal(raw["has_more"], &hasMore); err != nil {
				t.Fatal(err)
			}
			var rows []map[string]any
			if err := json.Unmarshal(raw[listKey], &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatalf("%s offset %d returned no rows", route.path, offset)
			}
			for _, row := range rows {
				id, _ := row[route.idField].(string)
				if seen[id] {
					t.Fatalf("%s: id %s returned on more than one page", route.path, id)
				}
				seen[id] = true
			}
			pages++
			if !hasMore {
				break
			}
			if len(rows) != 50 {
				t.Fatalf("%s: a page reporting has_more must be full: got %d", route.path, len(rows))
			}
			offset += 50
			if pages > total {
				t.Fatalf("%s: pagination never terminated", route.path)
			}
		}
		if len(seen) != total {
			t.Fatalf("%s: paging recovered %d of %d rows", route.path, len(seen), total)
		}
		if pages < 2 {
			t.Fatalf("%s: expected at least 2 pages at limit=50 for %d rows, got %d", route.path, total, pages)
		}

		wire(t, a, route.path+"?limit=0", "GET", a.Token, "error", 400)
		wire(t, a, route.path+"?limit=501", "GET", a.Token, "error", 400)
	}

	// The HTML console lists are bounded too, with real next/previous
	// navigation across the same two pages - not every row rendered
	// unbounded on every refresh.
	for _, console := range []string{"/products", "/workgraphs"} {
		first := getWeb(t, w, console, a.Token, 200)
		if !strings.Contains(string(first), fmt.Sprintf(`href="%s?offset=%d"`, console, webPageSize)) {
			t.Fatalf("%s: first page must link to a second page, got: %s", console, first)
		}
		if strings.Contains(string(first), `previous`) {
			t.Fatalf("%s: first page must not offer a previous link", console)
		}
		second := getWeb(t, w, fmt.Sprintf("%s?offset=%d", console, webPageSize), a.Token, 200)
		if !strings.Contains(string(second), fmt.Sprintf(`href="%s"`, console)) {
			t.Fatalf("%s: second page must link back to the first, got: %s", console, second)
		}
		if strings.Contains(string(second), `next &rarr;`) {
			t.Fatalf("%s: second (final) page must not offer a next link", console)
		}
	}
}

// rawExec runs one statement directly against runtime.db, bypassing every
// store writer's own invariant checks - the only way a test can reproduce a
// genuinely corrupt document or a dangling association, as opposed to a
// merely absent one.
func rawExec(t *testing.T, dir, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// TestWorkGraphDetailHidesAResolvedDecisionThroughTheRealHTTPRouteAfterRestart
// is F5's second half: the acceptance scenario in
// runtime.TestProductWorkspaceCombinesProductWorkGraphReviewAndDecision
// cannot itself call controlplane's HTTP route (controlplane imports
// runtime, so an internal "package runtime" test - needed for its
// unexported fleet/messaging fixtures - cannot import controlplane without
// a cycle). This test closes that gap from the other side: it builds a
// unit with a real open #473 decision through the real store writers,
// resolves it through the real #508 ResolveDecisionRequest, and proves -
// through an actual GET /v1/workgraphs/{id} against a FRESH ReadStore
// opened after the writer closes - that unresolvedOpenDecisions hides it.
func TestWorkGraphDetailHidesAResolvedDecisionThroughTheRealHTTPRouteAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(400, 0).UTC()
	graph, err := orchestration.WorkGraphProposal{Name: "decision-graph", Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "c", Purpose: "integrate", Role: "implementer", Issue: 301},
	}}.Compose("acme/repo", "claude", "operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	const runID = "run-c"
	origin := orchestration.BatchOrigin{GraphID: graph.ID, UnitID: "c"}
	batchID, err := orchestration.WorkUnitBatchID("acme/repo", "claude", 301, origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRun(rt.EngineeringRun{
		SchemaVersion: rt.SchemaVersion, ID: runID, Repository: "acme/repo",
		Phase: rt.Execute, Disposition: rt.Waiting, Reason: rt.ReasonDecisionPending,
		ControllerSHA256: strings.Repeat("4", 64), CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOrchestrationBatch(orchestration.Batch{
		SchemaVersion: orchestration.BatchSchemaVersion, ID: batchID, Repository: "acme/repo", AgentID: "claude",
		CreatedAt: at, Items: []orchestration.BatchItem{{Issue: 301, RunID: runID}}, Origin: &origin,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivateWorkUnit(rt.WorkUnitActivation{
		GraphID: graph.ID, UnitID: "c", BatchID: batchID, RunID: runID, InputsDigest: "inputs-digest-1", ActivatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	decision := orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion, ID: "decision-1", Scope: batchID, Kind: orchestration.KindDecisionRequest,
		Source:         orchestration.MessageSource{Unit: "c", RunID: runID, AgentID: "claude", OperationID: "op-1", Attempt: 1},
		Route:          orchestration.MessageRoute{Audience: orchestration.AudienceAuthority},
		Purpose:        "which backend?",
		Body:           "v1 or v2?",
		DocumentSHA256: strings.Repeat("a", 64), AdmittedAt: at,
	}
	if err := store.AdmitMessages([]orchestration.EngineeringMessage{decision}); err != nil {
		t.Fatal(err)
	}
	outcome := orchestration.DecisionOutcome{Kind: orchestration.DecisionText, Value: "use backend v2"}
	authority := orchestration.DecisionResolutionAuthority{
		Actor: "operator@example", AuthorityKind: orchestration.AuthorityKindOperator, Provenance: "local_control_endpoint",
	}
	if _, err := store.ResolveDecisionRequest(decision.ID, outcome, "resolved in review", authority, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	a := &API{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"), Now: func() time.Time { return at }}

	// The raw message store still has it (OrchestrationStatus is
	// liveness-only) - the real regression this test pins is that the HTTP
	// route built on top of it does NOT.
	raw, err := reader.OrchestrationStatus(batchID, at)
	if err != nil || len(raw.OpenDecisions) != 1 {
		t.Fatalf("the raw message store must still carry the resolved request: %+v (%v)", raw, err)
	}
	body := wire(t, a, "/v1/workgraphs/"+graph.ID, "GET", a.Token, "workgraph-detail", 200)
	if bytes.Contains(body, []byte(`"open_decisions"`)) {
		t.Fatalf("a resolved decision must be hidden through the real HTTP route after restart: %s", body)
	}
}

// TestWorkGraphDetailFailsClosedOnIntegrityBreaks pins F1: an unreadable
// (corrupt) graph document, and a graph whose association names a product
// that does not exist, are both durable-state failures (500 read_failed) -
// never indistinguishable from a merely unknown graph (404 not_found), and
// never silently shown as "no association".
func TestWorkGraphDetailFailsClosedOnIntegrityBreaks(t *testing.T) {
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(300, 0).UTC()
	corruptGraph, err := orchestration.WorkGraphProposal{Name: "corrupt-graph", Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "a", Purpose: "do the work", Role: "implementer", Issue: 201},
	}}.Compose("acme/repo", "claude", "operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdoptWorkGraphRevision(corruptGraph); err != nil {
		t.Fatal(err)
	}
	danglingGraph, err := orchestration.WorkGraphProposal{Name: "dangling-graph", Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "a", Purpose: "do the work", Role: "implementer", Issue: 202},
	}}.Compose("acme/repo", "claude", "operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdoptWorkGraphRevision(danglingGraph); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A corrupt stored document: the graph is known to exist (its revision
	// row is there), but it cannot be read back.
	rawExec(t, dir, `UPDATE work_graph_revisions SET document = 'not valid json' WHERE graph_id = ?`, corruptGraph.ID)
	// A dangling association: the row exists, but the product it names was
	// never adopted - unreachable through AssociateWorkGraph's own
	// existence check, reachable only by writing the row directly.
	rawExec(t, dir, `INSERT INTO product_graph_associations (graph_id, product_id, associated_unix_nano) VALUES (?, ?, ?)`,
		danglingGraph.ID, "product-does-not-exist", at.UnixNano())

	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	a := &API{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"), Now: func() time.Time { return at }}

	wire(t, a, "/v1/workgraphs/"+corruptGraph.ID, "GET", a.Token, "error", 500)
	wire(t, a, "/v1/workgraphs/"+danglingGraph.ID, "GET", a.Token, "error", 500)
}
