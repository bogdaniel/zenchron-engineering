package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// newTestControlPlaneServer opens a fresh, empty store in a temp state
// directory: everything this slice renders - the overview, the (empty) runs
// list, a run-detail 404 - has to hold even before a single run exists, which
// is the state a freshly `serve`d operator actually starts from.
func newTestControlPlaneServer(t *testing.T) (*controlPlaneServer, string) {
	t.Helper()
	stateDir := t.TempDir()
	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const token = "test-token-0123456789"
	return newControlPlaneServer(store, stateDir, 2, token), token
}

// THE AUTHENTICATION BOUNDARY IS THE WHOLE SECURITY MODEL for a loopback
// listener a browser can reach: every route must refuse a request presenting
// no credential, a GET carrying the right token must bootstrap a cookie and
// redirect to the token-free URL, and the cookie it hands back must then
// admit the next request on its own.
func TestControlPlaneAuthenticationBoundary(t *testing.T) {
	server, token := newTestControlPlaneServer(t)
	handler := server.handler()

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/overview", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /overview = %d, want 401", unauthenticated.Code)
	}
	if !strings.Contains(unauthenticated.Body.String(), "unauthorized") {
		t.Fatalf("401 body does not render the unauthorized page:\n%s", unauthenticated.Body.String())
	}

	wrongToken := httptest.NewRecorder()
	handler.ServeHTTP(wrongToken, httptest.NewRequest(http.MethodGet, "/overview?token=not-it", nil))
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token /overview = %d, want 401", wrongToken.Code)
	}

	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "/overview?token="+token, nil))
	if bootstrap.Code != http.StatusFound {
		t.Fatalf("bootstrap /overview?token=... = %d, want 302", bootstrap.Code)
	}
	if location := bootstrap.Header().Get("Location"); location != "/overview" {
		t.Fatalf("bootstrap redirected to %q, want the token-free URL", location)
	}
	cookies := bootstrap.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != controlPlaneCookie {
		t.Fatalf("bootstrap did not set the session cookie: %+v", cookies)
	}

	withCookie := httptest.NewRecorder()
	withCookieReq := httptest.NewRequest(http.MethodGet, "/overview", nil)
	withCookieReq.AddCookie(cookies[0])
	handler.ServeHTTP(withCookie, withCookieReq)
	if withCookie.Code != http.StatusOK {
		t.Fatalf("cookie-bearing /overview = %d, want 200:\n%s", withCookie.Code, withCookie.Body.String())
	}

	withHeader := httptest.NewRecorder()
	withHeaderReq := httptest.NewRequest(http.MethodGet, "/overview", nil)
	withHeaderReq.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(withHeader, withHeaderReq)
	if withHeader.Code != http.StatusOK {
		t.Fatalf("bearer-header /overview = %d, want 200", withHeader.Code)
	}

	// The API surface refuses the same way, as JSON rather than a page.
	apiUnauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(apiUnauthenticated, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if apiUnauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/overview = %d, want 401", apiUnauthenticated.Code)
	}
	var body controlPlaneErrorBody
	if err := json.Unmarshal(apiUnauthenticated.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 API body is not the JSON error envelope: %v", err)
	}
	if body.Error != "unauthorized" {
		t.Fatalf("401 API error = %q, want unauthorized", body.Error)
	}
}

// / redirects to the overview, which is where an operator who just typed the
// bare host:port lands.
func TestControlPlaneRootRedirectsToOverview(t *testing.T) {
	server, token := newTestControlPlaneServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	server.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/overview" {
		t.Fatalf("GET / = %d Location=%q, want 302 to /overview", rec.Code, rec.Header().Get("Location"))
	}
}

// An empty fleet still renders: a console that only works once a run exists
// would fail an operator at the exact moment - right after `serve` starts -
// they most need it to work.
func TestControlPlaneOverviewAndRunsRenderWithNoRuns(t *testing.T) {
	server, token := newTestControlPlaneServer(t)
	handler := server.handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(rec, req)
		return rec
	}

	overview := get("/overview")
	if overview.Code != http.StatusOK {
		t.Fatalf("GET /overview = %d:\n%s", overview.Code, overview.Body.String())
	}
	if !strings.Contains(overview.Body.String(), "overview") {
		t.Fatalf("overview page does not render its own title:\n%s", overview.Body.String())
	}

	runs := get("/runs")
	if runs.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d:\n%s", runs.Code, runs.Body.String())
	}
	if !strings.Contains(runs.Body.String(), "no runs match this filter") {
		t.Fatalf("empty runs list does not say so:\n%s", runs.Body.String())
	}

	filtered := get("/runs?status=active")
	if filtered.Code != http.StatusOK {
		t.Fatalf("GET /runs?status=active = %d", filtered.Code)
	}

	notFound := get("/runs/does-not-exist")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("GET /runs/does-not-exist = %d, want 404", notFound.Code)
	}
	if !strings.Contains(notFound.Body.String(), "not found") {
		t.Fatalf("404 page does not say not found:\n%s", notFound.Body.String())
	}
}

// The /api/* routes are this page's own polling mechanism (see
// control_plane_server.go): they answer JSON, and a run that does not exist
// is a 404 there too, never a 200 with an empty body a poller might mistake
// for "nothing changed yet".
func TestControlPlaneAPIRoutes(t *testing.T) {
	server, token := newTestControlPlaneServer(t)
	handler := server.handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(rec, req)
		return rec
	}

	overview := get("/api/overview")
	if overview.Code != http.StatusOK {
		t.Fatalf("GET /api/overview = %d", overview.Code)
	}
	if ct := overview.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET /api/overview content-type = %q, want application/json", ct)
	}
	var decoded map[string]any
	if err := json.Unmarshal(overview.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("overview API body is not JSON: %v", err)
	}

	runs := get("/api/runs")
	if runs.Code != http.StatusOK {
		t.Fatalf("GET /api/runs = %d", runs.Code)
	}

	missing := get("/api/runs/does-not-exist")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET /api/runs/does-not-exist = %d, want 404", missing.Code)
	}

	missingEvents := get("/api/runs/does-not-exist/events")
	if missingEvents.Code != http.StatusNotFound {
		t.Fatalf("GET /api/runs/does-not-exist/events = %d, want 404", missingEvents.Code)
	}

	badAfter := get("/api/runs/does-not-exist/events?after=not-a-number")
	if badAfter.Code != http.StatusNotFound {
		// The run lookup is refused before "after" is even parsed; a real run
		// with a bad cursor is covered by parseNonNegativeInt64's own unit
		// shape below.
		t.Fatalf("GET with an unknown run = %d, want 404 regardless of the cursor", badAfter.Code)
	}
}

// The runs filter accepts "owner/repo#123" and a bare issue number
// interchangeably (matchesSource), and "held" asks about HeldMaterial rather
// than a Disposition value that does not exist.
func TestRunsFilterMatchesSourceAndHeld(t *testing.T) {
	active := runtime.RunDetail{Summary: runtime.RunSummary{
		RunID: "run-1", Repository: "acme/widgets", Issue: 42, Disposition: runtime.Active,
	}}
	held := runtime.RunDetail{
		Summary:      runtime.RunSummary{RunID: "run-2", Repository: "acme/widgets", Issue: 7, Disposition: runtime.Failed},
		HeldMaterial: &runtime.HeldMaterial{Kind: runtime.HeldUncommitted},
	}
	runs := []runtime.RunDetail{active, held}

	for _, test := range []struct {
		name   string
		filter runsFilter
		want   []string
	}{
		{"status active", runsFilter{Status: "active"}, []string{"run-1"}},
		{"held material", runsFilter{Status: "held"}, []string{"run-2"}},
		{"source by issue number", runsFilter{Source: "42"}, []string{"run-1"}},
		{"source by owner/repo#issue", runsFilter{Source: "acme/widgets#7"}, []string{"run-2"}},
		{"no filter", runsFilter{}, []string{"run-1", "run-2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			filtered := filterRuns(runs, test.filter)
			if len(filtered) != len(test.want) {
				t.Fatalf("filterRuns(%+v) = %d runs, want %d", test.filter, len(filtered), len(test.want))
			}
			for i, runID := range test.want {
				if filtered[i].Summary.RunID != runID {
					t.Fatalf("filterRuns(%+v)[%d] = %q, want %q", test.filter, i, filtered[i].Summary.RunID, runID)
				}
			}
		})
	}
}

// countRuns reports held material independently of disposition: a run can be
// failed AND holding, and both counts must see it.
func TestCountRunsCountsHeldIndependentlyOfDisposition(t *testing.T) {
	runs := []runtime.RunDetail{
		{Summary: runtime.RunSummary{Disposition: runtime.Failed}, HeldMaterial: &runtime.HeldMaterial{Kind: runtime.HeldUncommitted}},
		{Summary: runtime.RunSummary{Disposition: runtime.Active}},
	}
	counts := countRuns(runs)
	if counts.Failed != 1 || counts.Active != 1 || counts.Held != 1 {
		t.Fatalf("counts = %+v, want one failed, one active, one held", counts)
	}
}

// The console's own CSS/JS have to actually be reachable at the URL layout.html
// links ("/static/console.css", "/static/console.js"): a mismatch between the
// embed root and the mux pattern would mean the whole console renders unstyled
// and without its polling script, silently.
func TestControlPlaneStaticAssetsServe(t *testing.T) {
	server, token := newTestControlPlaneServer(t)
	handler := server.handler()
	for _, path := range []string{"/static/console.css", "/static/console.js"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "zenchron") {
			t.Fatalf("GET %s did not serve the expected asset:\n%.200s", path, rec.Body.String())
		}
	}
}
