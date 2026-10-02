// Package controlplane is the read-only local HTTP boundary. DTOs deliberately
// exclude payloads, transcripts, environment, goals, paths and diagnostic prose.
package controlplane

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

type Run struct {
	ID          string         `json:"run_id"`
	Phase       rt.Phase       `json:"phase"`
	Disposition rt.Disposition `json:"disposition"`
	Executing   bool           `json:"executing"`
}
type Runs struct {
	Runs    []Run `json:"runs"`
	Offset  int   `json:"offset"`
	HasMore bool  `json:"has_more"`
}
type Operation struct {
	State                  rt.OperationState `json:"state"`
	Attempt                int               `json:"attempt"`
	ProgressSource         string            `json:"progress_source"`
	HeartbeatAt            *time.Time        `json:"heartbeat_at"`
	LastProgressAt         *time.Time        `json:"last_progress_at"`
	SilentFor              time.Duration     `json:"silent_for"`
	InactivityLimit        time.Duration     `json:"inactivity_limit"`
	InactivityLimitUnknown bool              `json:"inactivity_limit_unknown"`
	InactivitySuspension   string            `json:"inactivity_suspension"`
}
type RunDetail struct {
	ID                  string         `json:"run_id"`
	Phase               rt.Phase       `json:"phase"`
	Disposition         rt.Disposition `json:"disposition"`
	Elapsed             time.Duration  `json:"elapsed"`
	ActiveElapsed       time.Duration  `json:"active_elapsed"`
	ExternalWaitElapsed time.Duration  `json:"external_wait_elapsed"`
	Operation           *Operation     `json:"operation"`
}
type Event struct {
	Sequence   int64     `json:"sequence"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
}
type Events struct {
	RunID   string  `json:"run_id"`
	Events  []Event `json:"events"`
	Next    int64   `json:"next_after"`
	HasMore bool    `json:"has_more"`
}
type Plan struct {
	ID               string         `json:"plan_id"`
	Revision         int            `json:"revision"`
	ApprovedRevision int            `json:"approved_revision"`
	State            string         `json:"state"`
	Stages           map[string]int `json:"stages"`
}
type Controller struct {
	DurableConsistency rt.DurableConsistency `json:"durable_consistency"`
	Serving            rt.ServingState       `json:"serving"`
	Projection         rt.ProjectionState    `json:"projection"`
	GenerationMatch    string                `json:"generation_match"`
	Role               rt.RoleObservation    `json:"role"`
	WorkAdmission      rt.WorkAdmissionState `json:"work_admission"`
}
type Error struct {
	Code string `json:"error"`
}

// API owns only a read capability. Observe must issue controller.snapshot only.
type API struct {
	Store          *rt.ReadStore
	Token          string
	ControllerRoot string
	Observe        func() (rt.LiveControllerSnapshot, error)
	Now            func() time.Time
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/controller", a.controller)
	mux.HandleFunc("GET /v1/runs", a.runs)
	mux.HandleFunc("GET /v1/runs/{id}", a.run)
	mux.HandleFunc("GET /v1/runs/{id}/events", a.events)
	mux.HandleFunc("GET /v1/plans/{id}", a.plan)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if a.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.Token)) != 1 {
			fail(w, 401, "unauthorized")
			return
		}
		if r.Method != http.MethodGet {
			fail(w, 405, "method_not_allowed")
			return
		}
		if path.Clean(r.URL.Path) != r.URL.Path {
			fail(w, 404, "not_found")
			return
		}
		_, pattern := mux.Handler(r)
		if pattern == "" {
			fail(w, 404, "not_found")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}
func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code string) { send(w, status, Error{code}) }
func pageNumber(r *http.Request, key string, fallback, max int64) (int64, bool) {
	v, present := r.URL.Query()[key]
	if !present {
		return fallback, true
	}
	if len(v) != 1 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[0], 10, 64)
	return n, err == nil && n >= 0 && n <= max
}
func (a *API) runs(w http.ResponseWriter, r *http.Request) {
	offset, ok := pageNumber(r, "offset", 0, 1<<31-1)
	limit, ok2 := pageNumber(r, "limit", 100, 500)
	if !ok || !ok2 || limit == 0 {
		fail(w, 400, "invalid_page")
		return
	}
	f, err := a.Store.Fleet(a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := Runs{Runs: []Run{}, Offset: int(offset)}
	end := min(int64(len(f.Runs)), offset+limit)
	for i := offset; i < end; i++ {
		s := f.Runs[i]
		out.Runs = append(out.Runs, Run{s.RunID, s.Phase, s.Disposition, s.Executing})
	}
	out.HasMore = end < int64(len(f.Runs))
	send(w, 200, out)
}
func (a *API) exists(w http.ResponseWriter, id string) bool {
	ok, err := a.Store.HasRun(id)
	if err != nil {
		fail(w, 500, "read_failed")
		return false
	}
	if !ok {
		fail(w, 404, "not_found")
	}
	return ok
}
func (a *API) run(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.exists(w, id) {
		return
	}
	s, err := a.Store.Status(id, a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := RunDetail{ID: s.RunID, Phase: s.Phase, Disposition: s.Disposition, Elapsed: s.Elapsed, ActiveElapsed: s.ActiveElapsed, ExternalWaitElapsed: s.ExternalWaitElapsed}
	if p := s.Operation; p != nil {
		out.Operation = &Operation{p.State, p.Attempt, p.ProgressSource, p.HeartbeatAt, p.LastProgressAt, p.SilentFor, p.InactivityLimit, p.InactivityLimitUnknown, p.InactivitySuspension}
	}
	send(w, 200, out)
}
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	after, ok := pageNumber(r, "after", 0, 1<<63-1)
	limit, ok2 := pageNumber(r, "limit", 100, 500)
	if !ok || !ok2 || limit == 0 {
		fail(w, 400, "invalid_page")
		return
	}
	id := r.PathValue("id")
	if !a.exists(w, id) {
		return
	}
	events, more, err := a.Store.EventsPage(id, after, int(limit))
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := Events{RunID: id, Events: []Event{}, Next: after, HasMore: more}
	for _, e := range events {
		out.Events = append(out.Events, Event{e.Sequence, e.Type, e.OccurredAt})
		out.Next = e.Sequence
	}
	send(w, 200, out)
}
func (a *API) plan(w http.ResponseWriter, r *http.Request) {
	f, err := a.Store.Fleet(a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	for _, p := range f.Plans {
		if p.PlanID == r.PathValue("id") {
			if p.Error != "" {
				fail(w, 500, "read_failed")
				return
			}
			stages := p.Stages
			if stages == nil {
				stages = map[string]int{}
			}
			send(w, 200, Plan{p.PlanID, p.Revision, p.ApprovedRevision, p.State, stages})
			return
		}
	}
	fail(w, 404, "not_found")
}
func controllerProjection(s rt.ControllerStatus) Controller {
	out := Controller{DurableConsistency: s.DurableConsistency, Serving: s.Serving, Projection: s.Projection.State, GenerationMatch: "unknown"}
	if s.Live.Reachable && s.Live.Snapshot != nil {
		out.Role = s.Live.Snapshot.Role
		out.WorkAdmission = s.Live.Snapshot.WorkAdmission
		if s.Durable.Generation != nil && s.DurableConsistency != rt.DurableUnstable {
			out.GenerationMatch = "mismatch"
			if *s.Durable.Generation == s.Live.Snapshot.Identity.Build {
				out.GenerationMatch = "match"
			}
		}
	}
	return out
}
func (a *API) controller(w http.ResponseWriter, r *http.Request) {
	s, err := a.Store.Controller(a.ControllerRoot, a.Observe, a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	send(w, 200, controllerProjection(s))
}
