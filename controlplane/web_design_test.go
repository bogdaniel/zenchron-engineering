package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesignPreviewSharesAuthenticationAndCannotMutate(t *testing.T) {
	w := webFrom(fixture(t, true))
	for _, route := range []string{"/design", "/app", "/design/assets/app.js", "/design/assets/fixtures.js", "/design/assets/react.js"} {
		status := http.StatusUnauthorized
		if engineeringBootstrapPath(route) {
			status = http.StatusOK
		}
		getWeb(t, w, route, "", status)
		getWeb(t, w, route, "wrong-token", status)
		getWeb(t, w, route, w.Token, http.StatusOK)
		req := httptest.NewRequest(http.MethodPost, route, nil)
		req.AddCookie(&http.Cookie{Name: webTokenCookie, Value: w.Token})
		out := httptest.NewRecorder()
		w.Handler().ServeHTTP(out, req)
		if out.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: got %d", route, out.Code)
		}
	}
	getWeb(t, w, "/overview", w.Token, http.StatusOK)
	getWeb(t, w, "/design/assets/missing.js", w.Token, http.StatusNotFound)
	getWeb(t, w, "/design/assets/", w.Token, http.StatusNotFound)
	getWeb(t, w, "/design/assets/index.html", w.Token, http.StatusNotFound)
	getWeb(t, w, "/design/assets/../fixtures.js", w.Token, http.StatusNotFound)
}

func TestDesignPreviewIsFixtureOnlyAndForbidsConnections(t *testing.T) {
	w := webFrom(fixture(t, true))
	req := httptest.NewRequest(http.MethodGet, "/design", nil)
	req.AddCookie(&http.Cookie{Name: webTokenCookie, Value: w.Token})
	out := httptest.NewRecorder()
	w.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("preview: %d", out.Code)
	}
	for _, directive := range []string{"connect-src 'none'", "frame-ancestors 'none'", "form-action 'none'"} {
		if !strings.Contains(out.Header().Get("Content-Security-Policy"), directive) {
			t.Fatalf("missing preview restriction %q", directive)
		}
	}
	if strings.Contains(out.Body.String(), "SECRET") || strings.Contains(out.Body.String(), w.Token) {
		t.Fatal("runtime fixture state or token escaped into the design shell")
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("preview must reuse no-store authentication boundary")
	}
	for _, asset := range []string{"app.js", "actions.js", "components.js", "fixtures.js", "overview.js", "portfolio.js", "workspace.js"} {
		body := getWeb(t, w, "/design/assets/"+asset, w.Token, http.StatusOK)
		if strings.Contains(string(body), "fetch(") || strings.Contains(string(body), "XMLHttpRequest") {
			t.Fatalf("fixture module %s must not request runtime state", asset)
		}
	}
}

func TestEngineeringApplicationSharesShellAndKeepsBearerAPI(t *testing.T) {
	w := webFrom(fixture(t, true))
	req := httptest.NewRequest(http.MethodGet, "/app", nil)
	req.AddCookie(&http.Cookie{Name: webTokenCookie, Value: w.Token})
	out := httptest.NewRecorder()
	w.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Header().Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatal("runtime application must permit only same-origin integration")
	}
	preview := getWeb(t, w, "/design", w.Token, http.StatusOK)
	if out.Body.String() != strings.Replace(string(preview), "/design/assets/app.js", "/design/assets/react.js", 1) {
		t.Fatal("runtime and fixture modes must reuse the shell and CSS; only the entry module differs")
	}
	api := fixture(t, true)
	request := httptest.NewRequest(http.MethodGet, "/v1/controller", nil)
	request.AddCookie(&http.Cookie{Name: webTokenCookie, Value: api.Token})
	result := httptest.NewRecorder()
	api.Handler().ServeHTTP(result, request)
	if result.Code != http.StatusUnauthorized {
		t.Fatal("frontend integration must not weaken bearer API authentication")
	}
}

func TestEngineeringBootstrapIsCodeOnlyWithoutCredential(t *testing.T) {
	w := webFrom(fixture(t, true))
	for _, route := range []string{"/app", "/design/assets/react.js", "/design/assets/tokens.css", "/design/assets/layout.css", "/design/assets/components.css", "/design/assets/runtime.css"} {
		body := getWeb(t, w, route, "", http.StatusOK)
		if strings.Contains(string(body), "SECRET_PRIVATE_GOAL") || strings.Contains(string(body), "SECRET_DIAGNOSTIC") {
			t.Fatalf("private observation escaped through bootstrap: %s", route)
		}
	}
	getWeb(t, w, "/design/assets/fixtures.js", "", http.StatusUnauthorized)
	getWeb(t, w, "/overview", "", http.StatusUnauthorized)
}
