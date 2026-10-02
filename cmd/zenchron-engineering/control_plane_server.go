package main

// The console's HTTP surface.
//
// Every handler reuses an existing runtime projection; nothing here replays a
// store query a projection does not already own:
//
//	GET  /overview                  runtime.DescribeControllerStatus + runtime.ConsoleFleet's counts
//	GET  /runs                      runtime.ConsoleFleet, filtered
//	GET  /runs/{run_id}              runtime.ConsoleRunDetail + the run's journal
//	GET  /login                      the sign-in form
//	POST /login                      verify the token, set the session cookie, redirect
//	GET  /api/overview              the same data as /overview, as JSON, for the page's own polling
//	GET  /api/runs                  the same data as /runs, as JSON
//	GET  /api/runs/{run_id}          the same data as /runs/{run_id}, as JSON
//	GET  /api/runs/{run_id}/events   the run's events after a cursor, as JSON
//
// The /api/* routes are this page's OWN polling mechanism, not a published
// contract: they exist so the small amount of JavaScript this console ships
// can refresh the live operation overlay without a full page reload. They are
// not schema'd or versioned the way #392's eventual public API will be.
//
// Nothing here is reachable without the bearer token or its session cookie,
// except "/login" itself - the one route that necessarily must be reachable
// unauthenticated, because it is how a request BECOMES authenticated. No
// other exemption exists: not a health check, not a static asset.
//
// This slice is READ ONLY. There is no mutating route beyond "/login"
// establishing this console's own session, and none of the control socket's
// command vocabulary (submit, drain, stop, plan-approve, ...) is exposed
// here: that is #392's later, explicitly separate scope.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// controlPlaneDefaultEventLimit bounds one page of the timeline's incremental
// poll: a run that has produced a burst of events since the last poll is
// still answered quickly, with the rest arriving on the next tick.
const controlPlaneDefaultEventLimit = 200

// controlPlaneServer holds exactly the read-only dependencies its handlers
// need: a store opened read-only, the state directory ConsoleFleet needs for
// workspace presence checks, the operator's configured run ceiling, and the
// bearer token every request must present.
type controlPlaneServer struct {
	store                 *runtime.SQLiteOperationStore
	stateDir              string
	capacity              int
	token                 string
	now                   func() time.Time
	controllerRoot        string
	observeLiveController func() (runtime.LiveControllerSnapshot, error)
}

func newControlPlaneServer(store *runtime.SQLiteOperationStore, stateDir string, capacity int, token string) *controlPlaneServer {
	return &controlPlaneServer{
		store:                 store,
		stateDir:              stateDir,
		capacity:              capacity,
		token:                 token,
		now:                   func() time.Time { return time.Now().UTC() },
		controllerRoot:        controllerRoot(),
		observeLiveController: observeLiveController(stateDir),
	}
}

func (s *controlPlaneServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("GET /overview", s.handleOverviewPage)
	mux.HandleFunc("GET /runs", s.handleRunsPage)
	mux.HandleFunc("GET /runs/{run_id}", s.handleRunDetailPage)
	mux.HandleFunc("GET /api/overview", s.handleOverviewAPI)
	mux.HandleFunc("GET /api/runs", s.handleRunsAPI)
	mux.HandleFunc("GET /api/runs/{run_id}", s.handleRunDetailAPI)
	mux.HandleFunc("GET /api/runs/{run_id}/events", s.handleRunEventsAPI)
	mux.Handle("GET /static/", http.FileServerFS(controlPlaneStaticFiles))
	return s.authenticate(mux)
}

// authenticate is the ONE place every route is refused or admitted. "/login"
// is the single exemption, because it is the credential-establishing request
// itself: a request without a valid Authorization header or session cookie
// reaching any OTHER path is always refused, never bootstrapped from
// something the URL or query string carried.
func (s *controlPlaneServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if controlPlaneTokenEqual(s.token, controlPlaneTokenPresented(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		s.writeUnauthorized(w, r)
	})
}

func (s *controlPlaneServer) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeControlPlaneError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token or console cookie is required")
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	if err := renderControlPlaneTemplate(w, loginTemplate, loginPageData{Next: safeLoginRedirect(r.URL.RequestURI())}); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
	}
}

func (s *controlPlaneServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/overview", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

// loginPageData is "/login"'s own content data: Next is where a successful
// sign-in returns to, and Error is set only when a submitted token was
// wrong.
type loginPageData struct {
	Next  string
	Error string
}

func (s *controlPlaneServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := safeLoginRedirect(r.URL.Query().Get("next"))
	if err := renderControlPlaneTemplate(w, loginTemplate, loginPageData{Next: next}); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
	}
}

// handleLoginSubmit is the ONLY place the token travels as a form field
// rather than a header or cookie: a POST body is not logged by this server,
// not kept in browser history, and not sent as a Referer the way a query
// parameter would be.
func (s *controlPlaneServer) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeControlPlaneError(w, http.StatusBadRequest, "invalid_form", err.Error())
		return
	}
	next := safeLoginRedirect(r.PostForm.Get("next"))
	if !controlPlaneTokenEqual(s.token, r.PostForm.Get("token")) {
		w.WriteHeader(http.StatusUnauthorized)
		if err := renderControlPlaneTemplate(w, loginTemplate, loginPageData{Next: next, Error: "that token was not accepted"}); err != nil {
			writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
		}
		return
	}
	setControlPlaneCookie(w, s.token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

type controlPlaneCounts struct {
	Active    int
	Waiting   int
	Failed    int
	Completed int
	Cancelled int
	Held      int
}

func countRuns(runs []runtime.RunDetail) controlPlaneCounts {
	var counts controlPlaneCounts
	for _, run := range runs {
		switch run.Summary.Disposition {
		case runtime.Active:
			counts.Active++
		case runtime.Waiting:
			counts.Waiting++
		case runtime.Failed:
			counts.Failed++
		case runtime.Completed:
			counts.Completed++
		case runtime.Cancelled:
			counts.Cancelled++
		}
		if run.HeldMaterial != nil {
			counts.Held++
		}
	}
	return counts
}

type overviewData struct {
	ObservedAt    time.Time
	Controller    runtime.ControllerStatus
	ControllerErr string
	Fleet         runtime.ConsoleFleetView
	Counts        controlPlaneCounts
}

func (s *controlPlaneServer) overview() (overviewData, error) {
	now := s.now()
	data := overviewData{ObservedAt: now}
	status, err := runtime.DescribeControllerStatus(s.store, s.controllerRoot, s.observeLiveController, now)
	if err != nil {
		data.ControllerErr = err.Error()
	} else {
		data.Controller = status
	}
	fleet, err := runtime.ConsoleFleet(s.store, s.stateDir, s.capacity, now)
	if err != nil {
		return data, err
	}
	data.Fleet = fleet
	data.Counts = countRuns(fleet.Runs)
	return data, nil
}

func (s *controlPlaneServer) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.overview()
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	if err := renderControlPlaneTemplate(w, overviewTemplate, data); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
	}
}

func (s *controlPlaneServer) handleOverviewAPI(w http.ResponseWriter, r *http.Request) {
	data, err := s.overview()
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, data)
}

// ---------------------------------------------------------------------------
// Runs list
// ---------------------------------------------------------------------------

// runsFilter is the Runs list's own filter state, read from query parameters
// so a filtered view is a plain, bookmarkable URL.
type runsFilter struct {
	Status string
	Source string
}

func runsFilterFromQuery(values url.Values) runsFilter {
	return runsFilter{
		Status: strings.TrimSpace(values.Get("status")),
		Source: strings.TrimSpace(values.Get("source")),
	}
}

// matches reports whether one run satisfies the filter. "held" is not a
// Disposition; it asks about HeldMaterial directly, independent of which
// terminal disposition produced it.
func (f runsFilter) matches(run runtime.RunDetail) bool {
	if f.Status != "" {
		if f.Status == "held" {
			if run.HeldMaterial == nil {
				return false
			}
		} else if string(run.Summary.Disposition) != f.Status {
			return false
		}
	}
	if f.Source != "" && !matchesSource(run.Summary, f.Source) {
		return false
	}
	return true
}

// matchesSource accepts either "owner/repo#123" or a bare issue number,
// because an operator watching one issue rarely remembers which repository it
// was in and should not have to type it.
func matchesSource(summary runtime.RunSummary, source string) bool {
	if repo, issue, ok := strings.Cut(source, "#"); ok {
		number, err := strconv.Atoi(strings.TrimSpace(issue))
		return err == nil && summary.Repository == repo && summary.Issue == number
	}
	if number, err := strconv.Atoi(source); err == nil {
		return summary.Issue == number
	}
	return false
}

func filterRuns(runs []runtime.RunDetail, filter runsFilter) []runtime.RunDetail {
	filtered := make([]runtime.RunDetail, 0, len(runs))
	for _, run := range runs {
		if filter.matches(run) {
			filtered = append(filtered, run)
		}
	}
	return filtered
}

type runsData struct {
	ObservedAt time.Time
	Fleet      runtime.ConsoleFleetView
	Runs       []runtime.RunDetail
	Filter     runsFilter
}

func (s *controlPlaneServer) runs(filter runsFilter) (runsData, error) {
	now := s.now()
	fleet, err := runtime.ConsoleFleet(s.store, s.stateDir, s.capacity, now)
	if err != nil {
		return runsData{}, err
	}
	return runsData{ObservedAt: now, Fleet: fleet, Runs: filterRuns(fleet.Runs, filter), Filter: filter}, nil
}

func (s *controlPlaneServer) handleRunsPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.runs(runsFilterFromQuery(r.URL.Query()))
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	if err := renderControlPlaneTemplate(w, runsTemplate, data); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
	}
}

func (s *controlPlaneServer) handleRunsAPI(w http.ResponseWriter, r *http.Request) {
	data, err := s.runs(runsFilterFromQuery(r.URL.Query()))
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, data)
}

// ---------------------------------------------------------------------------
// Run detail
// ---------------------------------------------------------------------------

type runDetailData struct {
	ObservedAt time.Time
	Detail     runtime.RunDetail
	Events     []eventView
}

func (s *controlPlaneServer) runDetail(runID string) (runDetailData, bool, error) {
	now := s.now()
	detail, found, err := runtime.ConsoleRunDetail(s.store, s.stateDir, runID, now)
	if err != nil || !found {
		return runDetailData{}, found, err
	}
	events, err := s.store.Events(runID)
	if err != nil {
		return runDetailData{}, true, err
	}
	return runDetailData{ObservedAt: now, Detail: detail, Events: eventViews(events)}, true, nil
}

func (s *controlPlaneServer) handleRunDetailPage(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	data, found, err := s.runDetail(runID)
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "run_unavailable", err.Error())
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		if err := renderControlPlaneTemplate(w, notFoundTemplate, runID); err != nil {
			writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
		}
		return
	}
	if err := renderControlPlaneTemplate(w, runDetailTemplate, data); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "render_failed", err.Error())
	}
}

func (s *controlPlaneServer) handleRunDetailAPI(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	data, found, err := s.runDetail(runID)
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "run_unavailable", err.Error())
		return
	}
	if !found {
		writeControlPlaneError(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("no run %q", runID))
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, data)
}

type runEventsPage struct {
	RunID     string      `json:"run_id"`
	After     int64       `json:"after"`
	Events    []eventView `json:"events"`
	NextAfter *int64      `json:"next_after,omitempty"`
	Truncated bool        `json:"truncated"`
}

func (s *controlPlaneServer) handleRunEventsAPI(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if _, found, err := s.store.Run(runID); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "run_unavailable", err.Error())
		return
	} else if !found {
		writeControlPlaneError(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("no run %q", runID))
		return
	}
	after, err := parseNonNegativeInt64(r.URL.Query().Get("after"))
	if err != nil {
		writeControlPlaneError(w, http.StatusBadRequest, "invalid_after", err.Error())
		return
	}
	events, err := s.store.EventsAfter(runID, after)
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "events_unavailable", err.Error())
		return
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	truncated := len(events) > controlPlaneDefaultEventLimit
	if truncated {
		events = events[:controlPlaneDefaultEventLimit]
	}
	page := runEventsPage{RunID: runID, After: after, Events: eventViews(events), Truncated: truncated}
	if len(events) > 0 {
		next := events[len(events)-1].Sequence
		page.NextAfter = &next
	}
	writeControlPlaneJSON(w, http.StatusOK, page)
}

func parseNonNegativeInt64(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("after must be a non-negative integer")
	}
	return value, nil
}

type controlPlaneErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeControlPlaneError(w http.ResponseWriter, status int, code, message string) {
	writeControlPlaneJSON(w, status, controlPlaneErrorBody{Error: code, Message: message})
}

func writeControlPlaneJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
