package controlplane

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed assets/design
var designAssets embed.FS

// The app and its exact bootstrap assets contain code only. Runtime reads
// still require Bearer authentication; fixture modules retain their gate.
func engineeringBootstrapPath(p string) bool {
	switch p {
	case "/app", "/design/assets/react.js", "/design/assets/tokens.css", "/design/assets/layout.css",
		"/design/assets/components.css", "/design/assets/runtime.css":
		return true
	}
	return false
}

func designPreviewHeaders(rw http.ResponseWriter) {
	rw.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	rw.Header().Set("Referrer-Policy", "no-referrer")
}

// One packaged application shares the existing cookie authentication and listener.
// /design is fixture-only; /app uses the existing bearer API and governed actions.
func (w *Web) registerEngineeringApplication(mux *http.ServeMux) {
	assets := http.FileServerFS(mustSubFS(designAssets, "assets/design"))
	serveShell := func(rw http.ResponseWriter, r *http.Request) {
		designPreviewHeaders(rw)
		if r.URL.Path == "/app" {
			rw.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		}
		body, err := designAssets.ReadFile("assets/design/index.html")
		if err != nil {
			w.renderError(rw, err)
			return
		}
		if r.URL.Path == "/app" {
			body = []byte(strings.Replace(string(body), "/design/assets/app.js", "/design/assets/react.js", 1))
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = rw.Write(body)
	}
	mux.HandleFunc("GET /design", serveShell)
	mux.HandleFunc("GET /app", serveShell)
	assetHandler := http.StripPrefix("/design/assets/", assets)
	mux.HandleFunc("GET /design/assets/", func(rw http.ResponseWriter, r *http.Request) {
		designPreviewHeaders(rw)
		if ext := path.Ext(r.URL.Path); ext != ".css" && ext != ".js" {
			http.NotFound(rw, r)
			return
		}
		assetHandler.ServeHTTP(rw, r)
	})
}
