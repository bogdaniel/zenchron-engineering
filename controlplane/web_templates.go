package controlplane

// The console's rendering: embedded templates and static assets, and the
// pure formatting functions the templates may call.
//
// Every page template is layout.html PLUS exactly one content file, parsed
// as its own *template.Template, so that "content" and "title" blocks
// defined by one page can never shadow another's.

import (
	"embed"
	"html/template"
	"io/fs"
	"time"
)

//go:embed assets/templates/*.html
var webTemplateFiles embed.FS

// webStaticFiles is the console's own CSS/JS, served verbatim. fs.Sub strips
// the "assets" prefix the embed directive keeps, so a request for
// "/static/console.css" resolves against "static/console.css" inside this
// tree, matching the "/static/" mux pattern it is stripped against.
//
//go:embed assets/static
var webAssetsRoot embed.FS

var webStaticFiles = mustSubFS(webAssetsRoot, "assets")

func mustSubFS(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// webFuncs are the only functions a template may call, and every one is a
// pure, total formatting function: no template here reaches back into the
// store, the filesystem, or anything else with a side effect.
var webFuncs = template.FuncMap{
	"fmtTime":     fmtWebTime,
	"fmtTimePtr":  fmtWebTimePtr,
	"fmtDuration": fmtWebDuration,
	"runsLink":    webRunsLink,
}

const webTimeLayout = "2006-01-02 15:04:05 MST"

func fmtWebTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(webTimeLayout)
}

func fmtWebTimePtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return fmtWebTime(*t)
}

// fmtWebDuration rounds to the second: sub-second precision is noise an
// operator reading an elapsed or silent-for column never needs.
func fmtWebDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// webRunsLink builds "/runs?status=...&source=..." for one of the Runs
// list's filter pills: status and source are set independently, and an
// empty value clears that one filter rather than clearing both. It is a
// plain link, not a form post, so the filtered view stays a bookmarkable URL
// and keeps working with JavaScript disabled.
func webRunsLink(status, source string) string {
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

func parseWebTemplate(name string) *template.Template {
	return template.Must(template.New("layout.html").Funcs(webFuncs).ParseFS(webTemplateFiles,
		"assets/templates/layout.html",
		"assets/templates/"+name))
}

var (
	overviewTemplate    = parseWebTemplate("overview.html")
	runsTemplate        = parseWebTemplate("runs.html")
	runDetailTemplate   = parseWebTemplate("run_detail.html")
	loginTemplate       = parseWebTemplate("login.html")
	notFoundTemplate    = parseWebTemplate("not_found.html")
	runNotFoundTemplate = parseWebTemplate("run_not_found.html")
	errorTemplate       = parseWebTemplate("error.html")
)
