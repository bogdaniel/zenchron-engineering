package main

// The control plane's HTTP surface.
//
// Every handler reuses an existing runtime projection; none replays a store
// query or re-derives a status this process does not own computing:
//
//	GET /v1/controller              runtime.DescribeControllerStatus
//	GET /v1/runs                    runtime.FleetStatus (runs + plan summaries)
//	GET /v1/runs/{run_id}            one runtime.RunSummary from the same fleet
//	GET /v1/runs/{run_id}/events     the run's runtime.EngineeringEvent journal
//	GET /v1/plans/{plan_id}          runtime.PlanSnapshot via store.ReplayPlan
//
// Nothing here queries SQLite directly, and nothing here is reachable without
// the bearer token: authenticate wraps every route, with no exemption.
//
// The store behind every handler was opened by OpenSQLiteOperationStoreReadOnly,
// which creates nothing and migrates nothing, so this process cannot mutate the
// runtime database it serves even if a handler tried to.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const (
	controlPlaneDefaultEventLimit = 100
	controlPlaneMaxEventLimit     = 500
)

// controlPlaneServer holds exactly the read-only dependencies its handlers
// need: a store opened read-only, the state directory FleetStatus needs for
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
	mux.HandleFunc("GET /v1/controller", s.handleController)
	mux.HandleFunc("GET /v1/runs", s.handleRuns)
	mux.HandleFunc("GET /v1/runs/{run_id}", s.handleRunDetail)
	mux.HandleFunc("GET /v1/runs/{run_id}/events", s.handleRunEvents)
	mux.HandleFunc("GET /v1/plans/{plan_id}", s.handlePlanDetail)
	return s.authenticate(mux)
}

// authenticate is the ONE place every route is refused or admitted. There is
// no exempt path - not a health check, not a root index - because a read
// boundary that leaves one route unauthenticated has no boundary.
func (s *controlPlaneServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !controlPlaneAuthorized(s.token, r.Header.Get("Authorization")) {
			writeControlPlaneError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *controlPlaneServer) handleController(w http.ResponseWriter, r *http.Request) {
	status, err := runtime.DescribeControllerStatus(s.store, s.controllerRoot, s.observeLiveController, s.now())
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "controller_status_unavailable", err.Error())
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, status)
}

func (s *controlPlaneServer) handleRuns(w http.ResponseWriter, r *http.Request) {
	fleet, err := runtime.FleetStatus(s.store, s.stateDir, s.capacity, s.now())
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, fleet)
}

func (s *controlPlaneServer) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if _, ok, err := s.store.Run(runID); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "run_unavailable", err.Error())
		return
	} else if !ok {
		writeControlPlaneError(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("no run %q", runID))
		return
	}
	fleet, err := runtime.FleetStatus(s.store, s.stateDir, s.capacity, s.now())
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "fleet_unavailable", err.Error())
		return
	}
	for _, run := range fleet.Runs {
		if run.RunID == runID {
			writeControlPlaneJSON(w, http.StatusOK, run)
			return
		}
	}
	// Unreachable in practice - the run row was just confirmed to exist, and
	// FleetStatus reports an unreadable run WITH an Error field rather than
	// omitting it - but a disappearing run is reported honestly rather than
	// panicking on an index that turned out not to be there.
	writeControlPlaneError(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("no run %q", runID))
}

// controlPlaneRunEvents is the bounded, paginated event page. Every release
// of this endpoint returns a page, never the unbounded stream: Limit is
// always populated, clamped to controlPlaneMaxEventLimit, and Truncated says
// whether more events exist after NextAfter.
type controlPlaneRunEvents struct {
	RunID     string                     `json:"run_id"`
	After     int64                      `json:"after"`
	Limit     int                        `json:"limit"`
	Events    []runtime.EngineeringEvent `json:"events"`
	NextAfter *int64                     `json:"next_after,omitempty"`
	Truncated bool                       `json:"truncated"`
}

func (s *controlPlaneServer) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if _, ok, err := s.store.Run(runID); err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "run_unavailable", err.Error())
		return
	} else if !ok {
		writeControlPlaneError(w, http.StatusNotFound, "run_not_found", fmt.Sprintf("no run %q", runID))
		return
	}

	after, err := parseNonNegativeInt64(r.URL.Query().Get("after"))
	if err != nil {
		writeControlPlaneError(w, http.StatusBadRequest, "invalid_after", err.Error())
		return
	}
	limit, err := parseBoundedLimit(r.URL.Query().Get("limit"), controlPlaneDefaultEventLimit, controlPlaneMaxEventLimit)
	if err != nil {
		writeControlPlaneError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	events, err := s.store.EventsAfter(runID, after)
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "events_unavailable", err.Error())
		return
	}
	truncated := len(events) > limit
	if truncated {
		events = events[:limit]
	}
	response := controlPlaneRunEvents{RunID: runID, After: after, Limit: limit, Events: events, Truncated: truncated}
	if len(events) > 0 {
		next := events[len(events)-1].Sequence
		response.NextAfter = &next
	}
	writeControlPlaneJSON(w, http.StatusOK, response)
}

func (s *controlPlaneServer) handlePlanDetail(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("plan_id")
	plans, err := s.store.Plans()
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "plans_unavailable", err.Error())
		return
	}
	found := false
	for _, plan := range plans {
		if plan.ID == planID {
			found = true
			break
		}
	}
	if !found {
		writeControlPlaneError(w, http.StatusNotFound, "plan_not_found", fmt.Sprintf("no plan %q", planID))
		return
	}
	snapshot, err := s.store.ReplayPlan(planID)
	if err != nil {
		writeControlPlaneError(w, http.StatusInternalServerError, "plan_unavailable", err.Error())
		return
	}
	writeControlPlaneJSON(w, http.StatusOK, snapshot)
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

func parseBoundedLimit(raw string, def, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if value > max {
		value = max
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
