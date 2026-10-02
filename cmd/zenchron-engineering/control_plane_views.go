package main

// The console's rendering: embedded templates and static assets, and the one
// function every handler calls to turn a page's data into HTML.
//
// Every page template is layout.html PLUS exactly one content file, parsed as
// its own *template.Template, so that "content" and "title" blocks defined by
// one page can never shadow another's. renderControlPlaneTemplate is the one
// place a response becomes bytes: a handler builds data, this turns it into
// the page.

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

//go:embed controlplane_assets/templates/*.html
var controlPlaneTemplateFiles embed.FS

// controlPlaneStaticFiles is the console's own CSS/JS, served verbatim.
// fs.Sub strips the "controlplane_assets" prefix the embed directive keeps,
// so a request for "/static/console.css" resolves against "static/console.css"
// inside this tree - exactly the path http.FileServerFS asks for from a mux
// pattern of "/static/" (no http.StripPrefix: the mux pattern and the file
// tree are deliberately kept in the same shape).
//
//go:embed controlplane_assets/static
var controlPlaneAssetsRoot embed.FS

var controlPlaneStaticFiles = mustSubFS(controlPlaneAssetsRoot, "controlplane_assets")

func mustSubFS(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// controlPlaneFuncs are the only functions a template may call. Every one of
// them is a pure, total formatting function: no template here reaches back
// into the store, the filesystem, or anything else with a side effect.
var controlPlaneFuncs = template.FuncMap{
	"fmtTime":     fmtControlPlaneTime,
	"fmtTimePtr":  fmtControlPlaneTimePtr,
	"fmtDuration": fmtControlPlaneDuration,
	"runsLink":    runsLink,
	"jsonText":    fmtControlPlaneJSON,
}

// fmtControlPlaneJSON renders a journalled payload as plain text, which
// html/template then escapes exactly as it would any other string: the
// payload is untyped, operator-authored-adjacent data (#397's event
// timeline), never markup to trust.
func fmtControlPlaneJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

const controlPlaneTimeLayout = "2006-01-02 15:04:05 MST"

func fmtControlPlaneTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(controlPlaneTimeLayout)
}

func fmtControlPlaneTimePtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return fmtControlPlaneTime(*t)
}

// fmtControlPlaneDuration rounds to the second: sub-second precision is noise
// an operator reading elapsed/silent-for columns never needs.
func fmtControlPlaneDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// runsLink builds "/runs?status=...&source=..." for one of the Runs list's
// filter pills: status and source are set independently, and an empty value
// clears that one filter rather than clearing both. It is a plain link, not a
// form post, so the filtered view stays a bookmarkable URL and keeps working
// with JavaScript disabled.
func runsLink(status, source string) string {
	query := ""
	add := func(key, value string) {
		if value == "" {
			return
		}
		if query != "" {
			query += "&"
		}
		query += key + "=" + template.URLQueryEscaper(value)
	}
	add("status", status)
	add("source", source)
	if query == "" {
		return "/runs"
	}
	return "/runs?" + query
}

func parseControlPlaneTemplate(name string) *template.Template {
	return template.Must(template.New("layout.html").Funcs(controlPlaneFuncs).ParseFS(controlPlaneTemplateFiles,
		"controlplane_assets/templates/layout.html",
		"controlplane_assets/templates/"+name))
}

var (
	overviewTemplate  = parseControlPlaneTemplate("overview.html")
	runsTemplate      = parseControlPlaneTemplate("runs.html")
	runDetailTemplate = parseControlPlaneTemplate("run_detail.html")
	notFoundTemplate  = parseControlPlaneTemplate("not_found.html")
	loginTemplate     = parseControlPlaneTemplate("login.html")
)

// controlPlanePage is the layout's own dot: ObservedAt is when THIS response
// was rendered (what the page's live/stale indicator measures against), and
// Data is whatever the page's content block was written for - an
// overviewData, a runsData, a runDetailData, a bare run id, a loginPageData,
// or nil.
type controlPlanePage struct {
	ObservedAt time.Time
	Data       any
}

// renderControlPlaneTemplate is the one place a page's data becomes HTML. It
// sets the content type unconditionally; for a response whose status line a
// caller already wrote (the 404 and 401 pages), net/http's Header().Set after
// WriteHeader is a documented no-op, and the browser gets the same
// text/html it would have sniffed from the body anyway.
func renderControlPlaneTemplate(w http.ResponseWriter, tmpl *template.Template, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := controlPlanePage{ObservedAt: time.Now().UTC(), Data: data}
	if err := tmpl.ExecuteTemplate(w, "layout", page); err != nil {
		return fmt.Errorf("render control plane template: %w", err)
	}
	return nil
}
