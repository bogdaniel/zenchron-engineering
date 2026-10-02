package controlplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// webFrom reuses api_test.go's fixture so the console and the JSON API are
// exercised against the identical store, token and clock: Web is a second
// presentation of the same boundary, not a second one with its own setup.
func webFrom(a *API) *Web {
	return &Web{Store: a.Store, Token: a.Token, ControllerRoot: a.ControllerRoot, Observe: a.Observe, Now: a.Now}
}

func getWeb(t *testing.T, w *Web, path string, cookie string, status int) []byte {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: webTokenCookie, Value: cookie})
	}
	out := httptest.NewRecorder()
	w.Handler().ServeHTTP(out, req)
	if out.Code != status {
		t.Fatalf("%s: %d: %s", path, out.Code, out.Body.String())
	}
	if bytes.Contains(out.Body.Bytes(), []byte("SECRET")) {
		t.Fatalf("secret escaped: %s", out.Body.String())
	}
	return out.Body.Bytes()
}

func TestWebRequiresAuthenticationExceptStaticAssets(t *testing.T) {
	w := webFrom(fixture(t, true))
	for _, path := range []string{"/overview", "/runs", "/runs/r"} {
		body := getWeb(t, w, path, "", 401)
		if !bytes.Contains(body, []byte("sign in")) {
			t.Fatalf("%s: expected the login page, got: %s", path, body)
		}
		if !bytes.Contains(body, []byte(`src="/static/console.js"`)) {
			t.Fatalf("%s: expected the console's own script tag, got: %s", path, body)
		}
	}
	// Static assets carry no run, controller or fleet state and are served
	// before the token is presented, because the login page itself needs
	// them.
	css := getWeb(t, w, "/static/console.css", "", 200)
	if len(css) == 0 {
		t.Fatal("expected console.css to be served")
	}
	getWeb(t, w, "/static/console.js", "", 200)
}

func TestWebCookieGrantsAccessAndWrongTokenDoesNot(t *testing.T) {
	w := webFrom(fixture(t, true))
	getWeb(t, w, "/overview", "wrong-token", 401)
	body := getWeb(t, w, "/overview", w.Token, 200)
	if !bytes.Contains(body, []byte("overview")) {
		t.Fatalf("expected the overview page, got: %s", body)
	}
	body = getWeb(t, w, "/runs", w.Token, 200)
	if !bytes.Contains(body, []byte(">r<")) && !bytes.Contains(body, []byte("/runs/r")) {
		t.Fatalf("expected the fixture run to be listed, got: %s", body)
	}
	body = getWeb(t, w, "/runs/r", w.Token, 200)
	if !bytes.Contains(body, []byte("causal event timeline")) {
		t.Fatalf("expected the run detail page, got: %s", body)
	}
	getWeb(t, w, "/runs/missing", w.Token, 404)
}

// The fixture run carries an EventRunWaiting event (api_test.go), so its
// REPLAYED disposition is "waiting" regardless of the "active" value PutRun
// wrote to the row - exactly the fact runtime/fleet.go's own doc comment
// insists on: the journal is authoritative over the row.
func TestWebRunsFilterByStatusAndHeldMaterial(t *testing.T) {
	w := webFrom(fixture(t, true))
	body := getWeb(t, w, "/runs?status=waiting", w.Token, 200)
	if !bytes.Contains(body, []byte("/runs/r")) {
		t.Fatalf("expected the waiting fixture run under the waiting filter, got: %s", body)
	}
	body = getWeb(t, w, "/runs?status=failed", w.Token, 200)
	if bytes.Contains(body, []byte("/runs/r")) {
		t.Fatalf("expected the waiting fixture run excluded from the failed filter, got: %s", body)
	}
	body = getWeb(t, w, "/runs?status=held", w.Token, 200)
	if bytes.Contains(body, []byte("/runs/r")) {
		t.Fatalf("expected a run holding no material excluded from the held filter, got: %s", body)
	}
}

func TestWebRejectsNonGETAndUncleanPaths(t *testing.T) {
	w := webFrom(fixture(t, true))
	req := httptest.NewRequest("POST", "/overview", nil)
	req.AddCookie(&http.Cookie{Name: webTokenCookie, Value: w.Token})
	out := httptest.NewRecorder()
	w.Handler().ServeHTTP(out, req)
	if out.Code != 405 {
		t.Fatalf("POST /overview: %d", out.Code)
	}
	getWeb(t, w, "//overview", w.Token, 404)
}
