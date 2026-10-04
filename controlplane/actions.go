package controlplane

// Governed operator actions (#398 S4a): `stop` a run and `plan-reject` one
// plan revision, and nothing else. docs/control-plane.md "Governed actions" is
// the frozen specification; this file is its only implementation.
//
// The control plane never writes the store or the journal. Every action is
// one request to the controller's own socket carrying the controller binding
// the operator saw; `serve` proves its own identity against it and executes
// under its controller role on that one connection. Everything here only
// NARROWS: it refuses earlier, it never authorizes.

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// Outcomes of one action.
const (
	OutcomeApplied        = "applied"
	OutcomeAlreadyApplied = "already_applied"
	OutcomeRefused        = "refused"
	OutcomeUnreachable    = "unreachable"
	OutcomeUnknown        = "unknown"
)

type ActionTarget struct {
	RunID    string `json:"run_id,omitempty"`
	PlanID   string `json:"plan_id,omitempty"`
	Revision int    `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// ActionResult is what happened, with State re-read from the durable store
// after the attempt - never the request echoed back.
type ActionResult struct {
	Action  string       `json:"action"`
	Target  ActionTarget `json:"target"`
	Outcome string       `json:"outcome"`
	Code    string       `json:"code,omitempty"`
	Detail  string       `json:"detail,omitempty"`
	State   any          `json:"state,omitempty"`
}

// actionMu serializes every action in this process, so a duplicate submit is
// pre-checked against the durable effect of the first.
// ponytail: one operator, one process; per-target locks only if that changes.
var actionMu sync.Mutex

const stateUnread = "durable state could not be re-read: re-read before retrying"

// actionDeadline outlives SendControl's own bound, so the lock is never
// released while a request it sent may still be applied.
const actionDeadline = 70 * time.Second

func (a *API) stop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	effect := func() (bool, any, error) {
		ok, err := a.Store.HasRun(id)
		if err != nil || !ok {
			return false, nil, err
		}
		s, err := a.Store.Status(id, a.now())
		if err != nil {
			return false, nil, err
		}
		d := runDetailProjection(s)
		return d.Disposition == rt.Cancelled, d, nil
	}
	precheck := func(state any) (int, string) {
		if state == nil {
			return 404, "run_not_found"
		}
		return 0, ""
	}
	a.act(w, r, ActionResult{Action: rt.ControlStop, Target: ActionTarget{RunID: id}}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlStop, RunID: id}, effect, precheck)
}

func (a *API) reject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		Revision          int    `json:"revision"`
		Digest            string `json:"digest"`
		Note              string `json:"note"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	if body.Revision < 1 || body.Digest == "" {
		fail(w, 400, "invalid_request")
		return
	}
	id := r.PathValue("id")
	var view rt.PlanView
	effect := func() (bool, any, error) {
		events, err := a.Store.PlanEvents(id)
		if err != nil {
			return false, nil, err
		}
		_, done := rt.DecisionEventFor(events, "reject", body.Revision, body.Digest)
		view, err = a.Store.PlanView(id, body.Revision)
		var missing *rt.PlanRefusedError
		if errors.As(err, &missing) {
			return done, nil, nil
		}
		if err != nil {
			return false, nil, err
		}
		return done, planDetailProjection(view), nil
	}
	// Exactly the revision awaiting a decision, at exactly the digest the
	// operator read. A stale value is refused, never refreshed.
	precheck := func(state any) (int, string) {
		switch {
		case state == nil:
			return 404, "plan_not_found"
		case !view.Snapshot.AwaitingDecision(body.Revision):
			return 409, "not_awaiting_decision"
		case view.Plan.Digest != body.Digest:
			return 409, "digest_mismatch"
		}
		return 0, ""
	}
	a.act(w, r, ActionResult{Action: rt.ControlPlanReject, Target: ActionTarget{PlanID: id, Revision: body.Revision, Digest: body.Digest}},
		body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlPlanReject, PlanID: id, Revision: body.Revision, Digest: body.Digest, Note: rt.BoundedNote(body.Note)},
		effect, precheck)
}

// act runs one action under the process lock: durable pre-check, narrowing
// refusals, ONE send, and - when the reply is lost - a re-read of durable state
// instead of a resend. effect reports whether the action's durable effect
// already exists, plus the current projection (nil when the target is absent).
func (a *API) act(w http.ResponseWriter, r *http.Request, res ActionResult, binding string, request rt.ControlRequest,
	effect func() (bool, any, error), precheck func(any) (int, string)) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(actionDeadline))
	actionMu.Lock()
	defer actionMu.Unlock()
	// reply ALWAYS reports the outcome. Once anything may have been sent, a
	// failed re-read must not hide what happened behind a bare read_failed.
	reply := func(status int) {
		if _, state, err := effect(); err == nil {
			res.State = state
		} else if res.Detail == "" {
			res.Detail = stateUnread
		} else {
			res.Detail += "; " + stateUnread
		}
		send(w, status, res)
	}
	refuse := func(status int, code string) {
		res.Outcome, res.Code = OutcomeRefused, code
		reply(status)
	}
	done, state, err := effect()
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	if done {
		res.Outcome = OutcomeAlreadyApplied
		reply(200)
		return
	}
	if status, code := precheck(state); status != 0 {
		refuse(status, code)
		return
	}
	if binding == "" {
		refuse(409, rt.ControlCodeControllerUnattested)
		return
	}
	// The same observation the page used to offer the action, taken again
	// now: serving, role, generation match and attestation. serve itself
	// enforces only the binding and the role.
	why := "no serving controller is reachable"
	if status, err := a.Store.Controller(a.ControllerRoot, a.Observe, a.now()); err == nil {
		why = controllerProjection(status).ActionsDisabled()
	}
	if why != "" {
		res.Detail = why
		refuse(409, "controller_not_actionable")
		return
	}
	request.ExpectedController = binding
	response, err := a.Send(request)
	switch {
	case errors.Is(err, rt.ErrControlReplyLost):
		res.Outcome, res.Detail = OutcomeUnknown, "sent; the reply was lost and the durable state does not show the effect: re-read before retrying"
		if done, _, readErr := effect(); readErr == nil && done {
			res.Outcome, res.Detail = OutcomeApplied, ""
			reply(200)
			return
		}
		reply(504)
	case err != nil:
		res.Outcome, res.Detail = OutcomeUnreachable, "the controller could not be reached; nothing was sent"
		reply(503)
	case !response.OK:
		code := response.Code
		if code == "" {
			code = "runtime_refused"
		}
		refuse(409, code)
	default:
		res.Outcome = OutcomeApplied
		reply(200)
	}
}

// decodeAction is the body boundary: JSON only, bounded, no unknown field,
// exactly one value.
func decodeAction(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "invalid_request")
		return false
	}
	return true
}

// mutationAllowed is the CSRF and DNS-rebinding boundary every POST passes
// before its route runs. The Bearer header was already required; a cookie is
// never accepted here. Order: Host, Origin, Sec-Fetch-Site, Content-Type.
func (a *API) mutationAllowed(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case a.Listen == "" || r.Host != a.Listen:
		fail(w, 403, "forbidden_host")
	case r.Header.Get("Origin") != "http://"+a.Listen:
		fail(w, 403, "forbidden_origin")
	case r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin":
		fail(w, 403, "forbidden_origin")
	default:
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			fail(w, 415, "unsupported_media_type")
			return false
		}
		return true
	}
	return false
}

// ActionsDisabled is why the console offers no action against this
// controller, or "" when it may. The server re-checks all of it; this only
// narrows what the page offers.
func (c Controller) ActionsDisabled() string {
	switch {
	case !c.LiveReachable || c.Serving != rt.Serving:
		return "no serving controller is reachable"
	case c.ControllerBinding == "":
		return "the serving controller is unattested"
	case c.Role != rt.RoleHeld:
		return "the serving controller does not hold the controller role"
	case c.GenerationMatch != "match":
		return "the serving controller is not the durable active generation"
	}
	return ""
}

// liveController is the page's controller observation; an unreadable one is
// the zero value, which disables every action.
func (w *Web) liveController(now time.Time) Controller {
	s, err := w.Store.Controller(w.ControllerRoot, w.Observe, now)
	if err != nil {
		return Controller{}
	}
	return controllerProjection(s)
}
