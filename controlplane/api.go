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

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// Run is one run as a list sees it. Every field here is the same bounded,
// non-secret category API's doc comment already promises: no path, no
// transcript, no free-form diagnostic prose. Unavailable replaces
// runtime.RunSummary's own Error string - ONE run that could not be replayed
// is reported as a fact about that run, never as the Go error text that
// explains why.
type Run struct {
	ID                string         `json:"run_id"`
	Repository        string         `json:"repository,omitempty"`
	Issue             int            `json:"issue,omitempty"`
	Agent             string         `json:"agent,omitempty"`
	ProviderKind      string         `json:"provider_kind,omitempty"`
	Model             string         `json:"model,omitempty"`
	TrustMode         rt.TrustMode   `json:"trust_mode,omitempty"`
	Phase             rt.Phase       `json:"phase"`
	Disposition       rt.Disposition `json:"disposition"`
	Operation         string         `json:"operation,omitempty"`
	Attempt           int            `json:"attempt,omitempty"`
	Executing         bool           `json:"executing"`
	Elapsed           time.Duration  `json:"elapsed,omitempty"`
	Branch            string         `json:"branch,omitempty"`
	CandidateRevision string         `json:"candidate_revision,omitempty"`
	Attempts          map[string]int `json:"attempts,omitempty"`
	Held              bool           `json:"held,omitempty"`
	PullRequest       int            `json:"pull_request,omitempty"`
	PRState           string         `json:"pull_request_state,omitempty"`
	CI                string         `json:"ci,omitempty"`
	Review            string         `json:"review,omitempty"`
	Unavailable       bool           `json:"unavailable,omitempty"`
}
type Runs struct {
	Runs    []Run `json:"runs"`
	Offset  int   `json:"offset"`
	HasMore bool  `json:"has_more"`
}
type Operation struct {
	Kind                   string            `json:"kind,omitempty"`
	State                  rt.OperationState `json:"state"`
	Attempt                int               `json:"attempt"`
	MaxAttempts            int               `json:"max_attempts,omitempty"`
	ProgressSource         string            `json:"progress_source"`
	StartedAt              *time.Time        `json:"started_at,omitempty"`
	HeartbeatAt            *time.Time        `json:"heartbeat_at"`
	LastProgressAt         *time.Time        `json:"last_progress_at"`
	SilentFor              time.Duration     `json:"silent_for"`
	InactivityLimit        time.Duration     `json:"inactivity_limit"`
	InactivityLimitUnknown bool              `json:"inactivity_limit_unknown"`
	InactivitySuspension   string            `json:"inactivity_suspension"`
	Deadline               *time.Time        `json:"deadline,omitempty"`
	DeadlineBound          rt.AttemptBound   `json:"deadline_bound,omitempty"`
}

// Source, Worker, Candidate and Ref are the identity facts a run detail
// shows beside its lifecycle. Worker deliberately has no Workspace member:
// the candidate clone's local path never crosses this boundary (#397 review
// 5397796709). A bounded presence fact stands in for it.
type Source struct {
	Repository    string `json:"repository,omitempty"`
	Issue         int    `json:"issue,omitempty"`
	URL           string `json:"url,omitempty"`
	IntentChanged bool   `json:"intent_changed,omitempty"`
}
type Worker struct {
	Agent          string       `json:"agent,omitempty"`
	ProviderKind   string       `json:"provider_kind,omitempty"`
	Model          string       `json:"model,omitempty"`
	TrustMode      rt.TrustMode `json:"trust_mode,omitempty"`
	WorkspaceBound bool         `json:"workspace_bound,omitempty"`
}
type Candidate struct {
	Branch   string `json:"branch,omitempty"`
	Revision string `json:"revision,omitempty"`
	Tree     string `json:"tree,omitempty"`
}
type Ref struct {
	ID       string `json:"id,omitempty"`
	Revision string `json:"revision,omitempty"`
}
type ControllerRef struct {
	BuildKind    string `json:"build_kind,omitempty"`
	BuildVersion string `json:"build_version,omitempty"`
	Changed      bool   `json:"changed,omitempty"`
}
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state,omitempty"`
	Merged bool   `json:"merged,omitempty"`
	Stale  bool   `json:"stale,omitempty"`
}
type Budgets struct {
	WallLimit                 time.Duration `json:"wall_limit,omitempty"`
	LifecycleDeadline         time.Duration `json:"lifecycle_deadline,omitempty"`
	MaxExecutionAttempts      int           `json:"max_execution_attempts,omitempty"`
	MaxExecutionContinuations int           `json:"max_execution_continuations,omitempty"`
	MaxRemediationAttempts    int           `json:"max_remediation_attempts,omitempty"`
	MaxAssuranceAttempts      int           `json:"max_assurance_attempts,omitempty"`
}

// HeldMaterial is the bounded, non-secret identity of held material (#203):
// kind, revision and why, never Location - the quarantine/workspace path.
type HeldMaterial struct {
	Kind                 string `json:"kind"`
	Revision             string `json:"revision,omitempty"`
	BlockedBy            string `json:"blocked_by,omitempty"`
	NextStep             string `json:"next_step,omitempty"`
	Successor            string `json:"successor,omitempty"`
	SuccessorUnavailable string `json:"successor_unavailable,omitempty"`
}
type Assurance struct {
	Passed       bool            `json:"passed"`
	FailureClass rt.FailureClass `json:"failure_class,omitempty"`
	Stale        bool            `json:"stale,omitempty"`
}
type PublicationAuthority struct {
	Status       domain.AuthorityStatus `json:"status,omitempty"`
	ActionType   string                 `json:"action_type,omitempty"`
	ActionTarget string                 `json:"action_target,omitempty"`
}
type AuthorityRequest struct {
	Status   domain.AuthorityStatus `json:"status,omitempty"`
	Requires []string               `json:"requires,omitempty"`
	Missing  []string               `json:"missing,omitempty"`
	Stale    []string               `json:"stale,omitempty"`
}

// ExecutionDiagnostic is classification and identity only, exactly like its
// runtime counterpart's own doc comment: Message, Code, ArtifactRef and every
// other free-form or path-shaped member stay out of this boundary.
type ExecutionDiagnostic struct {
	Stage                string          `json:"stage,omitempty"`
	FailureClass         rt.FailureClass `json:"failure_class,omitempty"`
	Bound                rt.AttemptBound `json:"bound,omitempty"`
	Successor            string          `json:"successor,omitempty"`
	SuccessorUnavailable string          `json:"successor_unavailable,omitempty"`
}
type RunDetail struct {
	ID                       string                `json:"run_id"`
	Repository               string                `json:"repository,omitempty"`
	Phase                    rt.Phase              `json:"phase"`
	Disposition              rt.Disposition        `json:"disposition"`
	Elapsed                  time.Duration         `json:"elapsed"`
	ActiveElapsed            time.Duration         `json:"active_elapsed"`
	ExternalWaitElapsed      time.Duration         `json:"external_wait_elapsed"`
	Source                   *Source               `json:"source,omitempty"`
	Worker                   *Worker               `json:"worker,omitempty"`
	Candidate                *Candidate            `json:"candidate,omitempty"`
	Base                     *Ref                  `json:"base,omitempty"`
	Contract                 *Ref                  `json:"contract,omitempty"`
	Controller               *ControllerRef        `json:"controller,omitempty"`
	Operation                *Operation            `json:"operation"`
	PullRequest              *PullRequest          `json:"pull_request,omitempty"`
	Attempts                 map[string]int        `json:"attempts,omitempty"`
	Budgets                  *Budgets              `json:"budgets,omitempty"`
	BudgetsUnverifiable      []string              `json:"budgets_unverifiable,omitempty"`
	HeldMaterial             *HeldMaterial         `json:"held_material,omitempty"`
	Assurance                *Assurance            `json:"assurance,omitempty"`
	PublicationAuthority     *PublicationAuthority `json:"publication_authority,omitempty"`
	AuthorityRequest         *AuthorityRequest     `json:"authority_request,omitempty"`
	ExecutionDiagnostic      *ExecutionDiagnostic  `json:"execution_diagnostic,omitempty"`
	CandidateDiscardRefusals int                   `json:"candidate_discard_refusals,omitempty"`
	CandidateDiscardRefused  string                `json:"candidate_discard_refused,omitempty"`
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
	Unavailable      bool           `json:"unavailable,omitempty"`
}
type Build struct {
	Kind           string `json:"kind,omitempty"`
	Version        string `json:"version,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
}
type Controller struct {
	DurableConsistency rt.DurableConsistency `json:"durable_consistency"`
	Serving            rt.ServingState       `json:"serving"`
	Projection         rt.ProjectionState    `json:"projection"`
	GenerationMatch    string                `json:"generation_match"`
	Role               rt.RoleObservation    `json:"role"`
	WorkAdmission      rt.WorkAdmissionState `json:"work_admission"`
	DurableGeneration  *Build                `json:"durable_generation,omitempty"`
	LiveReachable      bool                  `json:"live_reachable"`
	LiveBuild          *Build                `json:"live_build,omitempty"`
	Findings           []string              `json:"findings,omitempty"`
}
type Error struct {
	Code string `json:"error"`
}

// Fleet is the overview's fleet-wide projection: counts, capacity and the
// plan list, never the raw runtime.Fleet - which carries ControlEndpoint, a
// literal local socket path (#397 review 5397796709).
type FleetCounts struct {
	Active    int `json:"active"`
	Waiting   int `json:"waiting"`
	Failed    int `json:"failed"`
	Completed int `json:"completed"`
	Cancelled int `json:"cancelled"`
	Held      int `json:"held"`
}
type Fleet struct {
	Capacity          int         `json:"capacity"`
	Executing         int         `json:"executing"`
	Active            int         `json:"active"`
	SupervisorRunning bool        `json:"supervisor_running"`
	Counts            FleetCounts `json:"counts"`
	Runs              []Run       `json:"runs,omitempty"`
	Plans             []Plan      `json:"plans,omitempty"`
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

// runProjection is the ONE sanitized view of runtime.RunSummary: every field
// that reaches a template or a wire response - JSON or HTML - comes from
// here, never from handing the runtime struct itself to a renderer. It
// deliberately has no Workspace and turns a replay failure into Unavailable
// rather than repeating RunSummary.Error's Go error text (#397 review
// 5397796709).
func runProjection(s rt.RunSummary) Run {
	return Run{
		ID: s.RunID, Repository: s.Repository, Issue: s.Issue,
		Agent: s.Agent, ProviderKind: s.ProviderKind, Model: s.Model, TrustMode: s.TrustMode,
		Phase: s.Phase, Disposition: s.Disposition,
		Operation: s.Operation, Attempt: s.Attempt, Executing: s.Executing,
		Elapsed: s.Elapsed, Branch: s.Branch, CandidateRevision: s.CandidateRevision,
		Attempts: s.Attempts, Held: s.Held,
		PullRequest: s.PullRequest, PRState: s.PRState, CI: s.CI, Review: s.Review,
		Unavailable: s.Error != "",
	}
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
		out.Runs = append(out.Runs, runProjection(f.Runs[i]))
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

// operationProjection is the one sanitized view of runtime.OperationStatus.
func operationProjection(p *rt.OperationStatus) *Operation {
	if p == nil {
		return nil
	}
	return &Operation{
		Kind: p.Kind, State: p.State, Attempt: p.Attempt, MaxAttempts: p.MaxAttempts,
		ProgressSource: p.ProgressSource, StartedAt: p.StartedAt,
		HeartbeatAt: p.HeartbeatAt, LastProgressAt: p.LastProgressAt, SilentFor: p.SilentFor,
		InactivityLimit: p.InactivityLimit, InactivityLimitUnknown: p.InactivityLimitUnknown,
		InactivitySuspension: p.InactivitySuspension, Deadline: p.Deadline, DeadlineBound: p.DeadlineBound,
	}
}

// runDetailProjection is the ONE sanitized view of runtime.StatusReport.
// Every member that is a local path (Worker.Workspace), free-form diagnostic
// prose (ExecutionDiagnostic.Message and friends) or anything else S1's own
// doc comment already excludes is left out; everything else - classification,
// identity, counts and bounded enums - crosses, exactly once, here (#397
// review 5397796709).
func runDetailProjection(s rt.StatusReport) RunDetail {
	out := RunDetail{
		ID: s.RunID, Repository: s.Repository, Phase: s.Phase, Disposition: s.Disposition,
		Elapsed: s.Elapsed, ActiveElapsed: s.ActiveElapsed, ExternalWaitElapsed: s.ExternalWaitElapsed,
		Candidate: &Candidate{Branch: s.Candidate.Branch, Revision: s.Candidate.Revision, Tree: s.Candidate.Tree},
		Base:      &Ref{ID: s.Base.ID, Revision: s.Base.Revision},
		Contract:  &Ref{ID: s.Contract.ID, Revision: s.Contract.Revision},
		Controller: &ControllerRef{
			BuildKind: s.Controller.Build.Kind, BuildVersion: s.Controller.Build.Version, Changed: s.Controller.Changed,
		},
		Operation:                operationProjection(s.Operation),
		Attempts:                 s.Attempts,
		CandidateDiscardRefusals: s.CandidateDiscardRefusals, CandidateDiscardRefused: s.CandidateDiscardRefused,
	}
	if s.Source.Repository != "" || s.Source.URL != "" {
		out.Source = &Source{
			Repository: s.Source.Repository, Issue: s.Source.Issue, URL: s.Source.URL,
			IntentChanged: s.Source.IntentChanged,
		}
	}
	if w := s.Worker; w.Agent != "" || w.ProviderKind != "" || w.Model != "" || w.TrustMode != "" || w.Workspace != "" {
		out.Worker = &Worker{
			Agent: w.Agent, ProviderKind: w.ProviderKind, Model: w.Model, TrustMode: w.TrustMode,
			WorkspaceBound: w.Workspace != "",
		}
	}
	if p := s.PullRequest; p != nil {
		out.PullRequest = &PullRequest{Number: p.Number, State: p.State, Merged: p.Merged, Stale: p.Stale}
	}
	if b := s.Budgets; b != nil {
		out.Budgets = &Budgets{
			WallLimit: b.WallLimit, LifecycleDeadline: b.LifecycleDeadline,
			MaxExecutionAttempts: b.MaxExecutionAttempts, MaxExecutionContinuations: b.MaxExecutionContinuations,
			MaxRemediationAttempts: b.MaxRemediationAttempts, MaxAssuranceAttempts: b.MaxAssuranceAttempts,
		}
	} else {
		out.BudgetsUnverifiable = s.RunPolicy.Unverifiable
	}
	if h := s.HeldMaterial; h != nil {
		out.HeldMaterial = &HeldMaterial{
			Kind: h.Kind, Revision: h.Revision, BlockedBy: h.BlockedBy, NextStep: h.NextStep,
			Successor: h.Successor, SuccessorUnavailable: h.SuccessorUnavailable,
		}
	}
	if a := s.Assurance; a != nil {
		out.Assurance = &Assurance{Passed: a.Passed, FailureClass: a.FailureClass, Stale: a.Stale}
	}
	if p := s.PublicationAuthority; p != nil {
		out.PublicationAuthority = &PublicationAuthority{Status: p.Status, ActionType: p.Action.Type, ActionTarget: p.Action.Target}
	}
	if req := s.AuthorityRequest; req != nil {
		out.AuthorityRequest = &AuthorityRequest{Status: req.Status, Requires: req.Requires, Missing: req.Missing, Stale: req.Stale}
	}
	if d := s.ExecutionDiagnostic; d != nil {
		out.ExecutionDiagnostic = &ExecutionDiagnostic{
			Stage: d.Stage, FailureClass: d.FailureClass, Bound: d.Bound,
			Successor: d.Successor, SuccessorUnavailable: d.SuccessorUnavailable,
		}
	}
	return out
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
	send(w, 200, runDetailProjection(s))
}

// eventProjection is the one sanitized view of a journal event: sequence,
// type and timestamp. The payload body never crosses this boundary.
func eventProjection(e rt.EngineeringEvent) Event {
	return Event{Sequence: e.Sequence, Type: e.Type, OccurredAt: e.OccurredAt}
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
		out.Events = append(out.Events, eventProjection(e))
		out.Next = e.Sequence
	}
	send(w, 200, out)
}

// planProjection is the one sanitized view of runtime.PlanSummary.
// Unavailable replaces PlanSummary.Error's raw Go error text (#397 review
// 5397796709).
func planProjection(p rt.PlanSummary) Plan {
	stages := p.Stages
	if stages == nil {
		stages = map[string]int{}
	}
	return Plan{ID: p.PlanID, Revision: p.Revision, ApprovedRevision: p.ApprovedRevision, State: p.State, Stages: stages, Unavailable: p.Error != ""}
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
			send(w, 200, planProjection(p))
			return
		}
	}
	fail(w, 404, "not_found")
}

// controllerProjection is the one sanitized view of runtime.ControllerStatus.
// It never reports Durable.ArtifactDir or Projection.Path/Target - local
// filesystem paths - only build identity and the dimensional verdicts an
// operator judges health from.
func controllerProjection(s rt.ControllerStatus) Controller {
	out := Controller{DurableConsistency: s.DurableConsistency, Serving: s.Serving, Projection: s.Projection.State, GenerationMatch: "unknown", Findings: s.Findings}
	if g := s.Durable.Generation; g != nil {
		out.DurableGeneration = &Build{Kind: g.Kind, Version: g.Version, SourceRevision: g.SourceRevision}
	}
	if s.Live.Reachable && s.Live.Snapshot != nil {
		out.LiveReachable = true
		out.Role = s.Live.Snapshot.Role
		out.WorkAdmission = s.Live.Snapshot.WorkAdmission
		build := s.Live.Snapshot.Identity.Build
		out.LiveBuild = &Build{Kind: build.Kind, Version: build.Version, SourceRevision: build.SourceRevision}
		if s.Durable.Generation != nil && s.DurableConsistency != rt.DurableUnstable {
			out.GenerationMatch = "mismatch"
			if *s.Durable.Generation == build {
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

// fleetCounts computes the overview's disposition breakdown from the same
// sanitized Run rows the runs list renders.
func fleetCounts(runs []Run) FleetCounts {
	var c FleetCounts
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

// fleetProjection is the one sanitized view of runtime.Fleet. It never
// reports ControlEndpoint - a literal local socket path concatenated with its
// mechanism description (#397 review 5397796709).
func fleetProjection(f rt.Fleet) Fleet {
	out := Fleet{Capacity: f.Capacity, Executing: f.Executing, Active: f.Active, SupervisorRunning: f.SupervisorRunning}
	out.Runs = make([]Run, 0, len(f.Runs))
	for _, run := range f.Runs {
		out.Runs = append(out.Runs, runProjection(run))
	}
	out.Counts = fleetCounts(out.Runs)
	for _, p := range f.Plans {
		out.Plans = append(out.Plans, planProjection(p))
	}
	return out
}
