// Package controlplane: the operator console.
//
// Web is a second PRESENTATION of exactly the boundary API already owns, not
// a second boundary. It holds the same *runtime.ReadStore, the same
// controller root, the same live-observation seam and the same bearer
// token API does, and cmd/zenchron-engineering/control_plane.go mounts both
// on one http.Server behind one listener. There is no second SQLite handle,
// no second token file and no unschematized /api/* surface: the page's own
// live refresh (web/static/console.js) polls API's existing schema'd
// /v1/runs/{id} and /v1/runs/{id}/events routes, the same contract any other
// client of S1 would use.
//
// A browser cannot attach an Authorization header to a plain navigation, so
// the HTML surface accepts the identical token via a cookie instead of the
// header API requires. The cookie is never minted from anything but the
// operator typing the token S1 already generated into the console's own
// login page, and that page sets it from JavaScript rather than a query
// parameter or a form GET, so the token never appears in a URL, a server
// log, or a template response - only in the Cookie and Authorization headers
// a request carries, exactly where the existing bearer credential already
// travels for every other client.
//
// Templates render runtime.RunSummary and runtime.StatusReport directly, but
// never their Reason or Error members: both are free-form diagnostic prose
// (runtime/fleet.go, runtime/controller.go), the exact category API's own
// doc comment already excludes, and rendering it here would relax a
// boundary S1 drew rather than build on it.
package controlplane

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// Web renders the console. Every field mirrors API's: same store, same
// controller root, same live observation seam, same clock seam, same token.
type Web struct {
	Store          *rt.ReadStore
	Token          string
	ControllerRoot string
	Observe        func() (rt.LiveControllerSnapshot, error)
	Now            func() time.Time
}

func (w *Web) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// webTokenCookie is the ONLY place the console's session lives. Its value is
// never anything but the one token API already issues: there is no
// server-side session store, no second secret, nothing to rotate separately
// from control-plane.token itself.
const webTokenCookie = "zenchron_control_plane_token"

// authenticated reports whether this request already presented the token via
// the console's cookie. The comparison is constant-time for the same reason
// API's is: a local, unprivileged process on the same host must not be able
// to recover the token one timing measurement at a time.
func (w *Web) authenticated(r *http.Request) bool {
	if w.Token == "" {
		return false
	}
	cookie, err := r.Cookie(webTokenCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(w.Token)) == 1
}

// Handler serves the console. Every route is GET; a clean-path check and a
// strict method check mirror API's, because this is the same boundary's
// second surface, not a boundary with its own, looser rules.
func (w *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.handleRoot)
	mux.HandleFunc("GET /overview", w.handleOverview)
	mux.HandleFunc("GET /runs", w.handleRuns)
	mux.HandleFunc("GET /runs/{id}", w.handleRunDetail)
	mux.Handle("GET /static/", http.FileServerFS(webStaticFiles))
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if path.Clean(r.URL.Path) != r.URL.Path {
			w.renderNotFound(rw)
			return
		}
		// Handler is consulted only to learn the matched pattern; dispatch
		// goes through mux.ServeHTTP itself (not the *http.Handler Handler
		// returns), because only ServeHTTP binds {id} path values onto r -
		// calling the returned handler directly would hand every route an r
		// whose PathValue("id") is always empty.
		_, pattern := mux.Handler(r)
		if pattern == "" {
			w.renderNotFound(rw)
			return
		}
		// Static assets carry no run, controller or fleet state - only this
		// console's own CSS and JS - so they are the one route that predates
		// the token: the login page itself needs them to render.
		if strings.HasPrefix(pattern, "GET /static/") {
			mux.ServeHTTP(rw, r)
			return
		}
		if !w.authenticated(r) {
			w.renderLogin(rw)
			return
		}
		mux.ServeHTTP(rw, r)
	})
}

func (w *Web) handleRoot(rw http.ResponseWriter, r *http.Request) {
	http.Redirect(rw, r, "/overview", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

// webCounts is the fleet's disposition breakdown, computed here from the
// same RunSummary rows the Runs list renders: Fleet reports Active and
// Executing because only those two are judged against the concurrency
// ceiling (runtime/fleet.go), not because the per-disposition counts an
// overview needs do not exist in its Runs slice.
type webCounts struct {
	Active, Waiting, Failed, Completed, Cancelled, Held int
}

func countWebRuns(runs []rt.RunSummary) webCounts {
	var c webCounts
	for _, run := range runs {
		switch run.Disposition {
		case rt.Active:
			c.Active++
		case rt.Waiting:
			c.Waiting++
		case rt.Failed:
			c.Failed++
		case rt.Completed:
			c.Completed++
		case rt.Cancelled:
			c.Cancelled++
		}
		if run.Held {
			c.Held++
		}
	}
	return c
}

type overviewData struct {
	ObservedAt    time.Time
	Controller    rt.ControllerStatus
	ControllerErr string
	Fleet         rt.Fleet
	Counts        webCounts
}

func (w *Web) handleOverview(rw http.ResponseWriter, r *http.Request) {
	now := w.now()
	data := overviewData{ObservedAt: now}
	status, err := w.Store.Controller(w.ControllerRoot, w.Observe, now)
	if err != nil {
		data.ControllerErr = err.Error()
	} else {
		data.Controller = status
	}
	fleet, err := w.Store.Fleet(now)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	data.Fleet = fleet
	data.Counts = countWebRuns(fleet.Runs)
	w.render(rw, overviewTemplate, data)
}

// ---------------------------------------------------------------------------
// Runs list
// ---------------------------------------------------------------------------

// runsFilter is read from query parameters, so a filtered view is a plain,
// bookmarkable URL that keeps working with JavaScript disabled.
type runsFilter struct {
	Status string
	Source string
}

func runsFilterFromQuery(values map[string][]string) runsFilter {
	get := func(key string) string {
		if v, ok := values[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	return runsFilter{Status: get("status"), Source: get("source")}
}

// matches reports whether one run satisfies the filter. "held" is not a
// Disposition: it asks RunSummary.Held directly, independent of which
// terminal disposition a held run settled into.
func (f runsFilter) matches(s rt.RunSummary) bool {
	if f.Status != "" {
		if f.Status == "held" {
			if !s.Held {
				return false
			}
		} else if string(s.Disposition) != f.Status {
			return false
		}
	}
	if f.Source != "" && !matchesSource(s, f.Source) {
		return false
	}
	return true
}

// matchesSource accepts either "owner/repo#123" or a bare issue number: an
// operator watching one issue rarely remembers which repository it was filed
// against and should not have to type it to filter on it.
func matchesSource(s rt.RunSummary, source string) bool {
	if repo, issue, ok := strings.Cut(source, "#"); ok {
		number, err := strconv.Atoi(strings.TrimSpace(issue))
		return err == nil && s.Repository == repo && s.Issue == number
	}
	if number, err := strconv.Atoi(source); err == nil {
		return s.Issue == number
	}
	return false
}

func filterWebRuns(runs []rt.RunSummary, filter runsFilter) []rt.RunSummary {
	out := make([]rt.RunSummary, 0, len(runs))
	for _, run := range runs {
		if filter.matches(run) {
			out = append(out, run)
		}
	}
	return out
}

type runsData struct {
	ObservedAt time.Time
	Fleet      rt.Fleet
	Runs       []rt.RunSummary
	Filter     runsFilter
}

func (w *Web) handleRuns(rw http.ResponseWriter, r *http.Request) {
	now := w.now()
	fleet, err := w.Store.Fleet(now)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	filter := runsFilterFromQuery(r.URL.Query())
	data := runsData{ObservedAt: now, Fleet: fleet, Runs: filterWebRuns(fleet.Runs, filter), Filter: filter}
	w.render(rw, runsTemplate, data)
}

// ---------------------------------------------------------------------------
// Run detail
// ---------------------------------------------------------------------------

// webEventPageLimit bounds one page of the causal timeline, the same way
// API's own /v1/runs/{id}/events route bounds its SQL page: this handler
// calls the identical ReadStore.EventsPage, never EventsAfter sliced by hand.
const webEventPageLimit = 200

type runDetailData struct {
	ObservedAt time.Time
	Status     rt.StatusReport
	Events     []rt.EngineeringEvent
	After      int64
	NextAfter  int64
	HasMore    bool
}

func (w *Web) handleRunDetail(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	exists, err := w.Store.HasRun(id)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	if !exists {
		w.renderRunNotFound(rw, id)
		return
	}
	now := w.now()
	status, err := w.Store.Status(id, now)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	events, hasMore, err := w.Store.EventsPage(id, after, webEventPageLimit)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	data := runDetailData{ObservedAt: now, Status: status, Events: events, After: after, HasMore: hasMore, NextAfter: after}
	if len(events) > 0 {
		data.NextAfter = events[len(events)-1].Sequence
	}
	w.render(rw, runDetailTemplate, data)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

type webPage struct {
	ObservedAt time.Time
	Data       any
}

func (w *Web) render(rw http.ResponseWriter, tmpl *template.Template, data any) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := webPage{ObservedAt: w.now(), Data: data}
	if err := tmpl.ExecuteTemplate(rw, "layout", page); err != nil {
		http.Error(rw, "render failed", http.StatusInternalServerError)
	}
}

func (w *Web) renderLogin(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusUnauthorized)
	if err := loginTemplate.ExecuteTemplate(rw, "layout", webPage{ObservedAt: w.now()}); err != nil {
		http.Error(rw, "render failed", http.StatusInternalServerError)
	}
}

func (w *Web) renderNotFound(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusNotFound)
	if err := notFoundTemplate.ExecuteTemplate(rw, "layout", webPage{ObservedAt: w.now()}); err != nil {
		http.Error(rw, "render failed", http.StatusInternalServerError)
	}
}

func (w *Web) renderRunNotFound(rw http.ResponseWriter, id string) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusNotFound)
	if err := runNotFoundTemplate.ExecuteTemplate(rw, "layout", webPage{ObservedAt: w.now(), Data: id}); err != nil {
		http.Error(rw, "render failed", http.StatusInternalServerError)
	}
}

// renderError never renders err's own text: like API's fixed read_failed
// code, an internal error here becomes one bounded, constant sentence rather
// than raw exception text that might repeat a path or a store-internal
// detail back to the page.
func (w *Web) renderError(rw http.ResponseWriter, err error) {
	_ = err
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusInternalServerError)
	if tmplErr := errorTemplate.ExecuteTemplate(rw, "layout", webPage{ObservedAt: w.now()}); tmplErr != nil {
		http.Error(rw, "render failed", http.StatusInternalServerError)
	}
}
