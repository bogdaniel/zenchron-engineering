package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

type WorkspaceAgent struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	TrustMode rt.TrustMode `json:"trust_mode"`
	Model     string       `json:"model,omitempty"`
	Default   bool         `json:"default"`
	Eligible  bool         `json:"eligible"`
}

type WorkspaceOptions struct {
	Repositories     []string                  `json:"repositories"`
	Agents           []WorkspaceAgent          `json:"agents"`
	Profiles         []string                  `json:"profiles"`
	Templates        []string                  `json:"templates"`
	InstructionPacks []string                  `json:"instruction_packs"`
	ContextPolicies  []string                  `json:"context_policies"`
	Budget           domain.PlanBudgetEnvelope `json:"budget"`
}

type PlanPreview struct {
	PlanDetail
	StageBlockers     map[string]string `json:"stage_blockers"`
	Objective         string            `json:"objective"`
	StageObjectives   map[string]string `json:"stage_objectives"`
	StageRationales   map[string]string `json:"stage_rationales"`
	ControllerBinding string            `json:"controller_binding"`
	AssignmentsDigest string            `json:"assignments_digest"`
	WorkGraphID       string            `json:"work_graph_id,omitempty"`
	Unbound           []string          `json:"unbound,omitempty"`
}

// PlanPreviewProjection adds only the resolved worker identities and approval
// binding. Context contents, instructions and local paths stay private.
func PlanPreviewProjection(view rt.PlanView) PlanPreview {
	out := PlanPreview{StageBlockers: map[string]string{}, Objective: boundedWorkspaceText(view.Plan.Objective), StageObjectives: map[string]string{}, StageRationales: map[string]string{}, PlanDetail: planDetailProjection(view), AssignmentsDigest: view.AssignmentsDigest, WorkGraphID: view.WorkGraphID, Unbound: view.Unbound}
	for _, blocked := range view.Blocked {
		out.StageBlockers[blocked.StageID] = boundedWorkspaceText(string(blocked.Kind) + ": " + blocked.Reason)
	}
	for _, stage := range view.Plan.Stages {
		out.StageObjectives[stage.ID] = boundedWorkspaceText(stage.Objective)
		out.StageRationales[stage.ID] = boundedWorkspaceText(stage.Rationale)
	}
	for i := range out.Stages {
		for _, assignment := range view.Assigned {
			if out.Stages[i].ID != assignment.StageID {
				continue
			}
			out.Stages[i].AssignmentID = assignment.ID
			out.Stages[i].Agent = assignment.Agent.ID
			out.Stages[i].ProviderKind = assignment.Agent.ProviderKind
			out.Stages[i].TrustMode = string(assignment.Agent.TrustMode)
			out.Stages[i].BoundProfile = fmt.Sprintf("%s@%d", assignment.Profile.ID, assignment.Profile.Version)
		}
	}
	return out
}

func (a *API) workspaceOptions(w http.ResponseWriter, r *http.Request) {
	a.controllerRead(w, rt.ControlRequest{Command: rt.ControlWorkspaceOptions}, &WorkspaceOptions{})
}
func (a *API) planPreview(w http.ResponseWriter, r *http.Request) {
	a.controllerRead(w, rt.ControlRequest{Command: rt.ControlPlanView, PlanID: r.PathValue("id")}, &PlanPreview{})
}
func (a *API) controllerRead(w http.ResponseWriter, request rt.ControlRequest, value any) {
	if a.Send == nil {
		fail(w, 503, "controller_unreachable")
		return
	}
	status, err := a.Store.Controller(a.ControllerRoot, a.Observe, a.now())
	if err != nil || !controllerProjection(status).LiveReachable {
		fail(w, 503, "controller_unreachable")
		return
	}
	// These controller commands only observe configuration or durable work;
	// the local socket is their boundary, not mutation authority.
	response, err := a.Send(request)
	if err != nil {
		fail(w, 503, "controller_unreachable")
		return
	}
	if !response.OK {
		fail(w, 409, "runtime_refused")
		return
	}
	if err := json.Unmarshal(response.Payload, value); err != nil {
		fail(w, 502, "invalid_controller_response")
		return
	}
	send(w, 200, value)
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		Revision          int    `json:"revision"`
		Digest            string `json:"digest"`
		AssignmentsDigest string `json:"assignments_digest"`
		Note              string `json:"note"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	if body.Revision < 1 || body.Digest == "" || body.AssignmentsDigest == "" {
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
		done := false
		for _, event := range events {
			if event.Type != rt.EventPlanApproved {
				continue
			}
			var payload rt.PlanDecisionPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return false, nil, err
			}
			if payload.Revision == body.Revision && payload.Digest == body.Digest && payload.AssignmentsDigest == body.AssignmentsDigest {
				done = true
			}
		}
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
	precheck := func(state any) (int, string) {
		if state == nil {
			return 404, "plan_not_found"
		}
		if !view.Snapshot.AwaitingDecision(body.Revision) {
			return 409, "not_awaiting_decision"
		}
		if view.Plan.Digest != body.Digest {
			return 409, "digest_mismatch"
		}
		return 0, ""
	}
	a.act(w, r, ActionResult{Action: rt.ControlPlanApprove, Target: ActionTarget{PlanID: id, Revision: body.Revision, Digest: body.Digest}}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlPlanApprove, PlanID: id, Revision: body.Revision, Digest: body.Digest, AssignmentsDigest: body.AssignmentsDigest, Note: rt.BoundedNote(body.Note)}, effect, precheck)
}

func (a *API) resolveDecision(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		OutcomeKind       string `json:"outcome_kind"`
		OutcomeValue      string `json:"outcome_value"`
		Note              string `json:"note"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	outcome := orchestration.DecisionOutcome{Kind: body.OutcomeKind, Value: body.OutcomeValue}
	if err := outcome.Validate(); err != nil {
		fail(w, 400, "invalid_request")
		return
	}
	id := r.PathValue("id")
	effect := func() (bool, any, error) {
		stored, found, err := a.Store.DecisionResolution(id)
		if err != nil || !found {
			return false, nil, err
		}
		// A conflicting answer must reach runtime's conflict refusal, never be
		// reported as an idempotent replay of the submitted answer.
		return stored.Outcome == outcome && stored.Reason == rt.BoundedNote(body.Note), struct {
			ID      string                        `json:"id"`
			Outcome orchestration.DecisionOutcome `json:"outcome"`
		}{stored.ID, stored.Outcome}, nil
	}
	a.act(w, r, ActionResult{Action: rt.ControlResolveDecision, Target: ActionTarget{DecisionID: id}}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlResolveDecision, DecisionID: id, DecisionOutcomeKind: body.OutcomeKind, DecisionOutcomeValue: body.OutcomeValue, Note: rt.BoundedNote(body.Note)}, effect, func(any) (int, string) { return 0, "" })
}

// propose never retries a provider invocation. The controller rechecks the
// exact revision under its plan lock immediately before the durable write.
func (a *API) propose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		Repository        string `json:"repository"`
		Issue             int    `json:"issue"`
		Agent             string `json:"agent"`
		Profile           string `json:"profile"`
		Template          string `json:"template"`
		Deterministic     bool   `json:"deterministic"`
		Revision          int    `json:"revision"`
		Digest            string `json:"digest"`
		Note              string `json:"note"`
		ProductID         string `json:"product_id"`
		AcceptanceFeature string `json:"acceptance_feature"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	if body.AcceptanceFeature != "" && !domain.ValidAcceptanceFeature(body.AcceptanceFeature) {
		fail(w, 400, "invalid_request")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		if _, err := rt.ParseGitHubRepo(body.Repository); err != nil || body.Issue < 1 {
			fail(w, 400, "invalid_request")
			return
		}
	} else if body.Revision < 1 || body.Digest == "" {
		fail(w, 400, "invalid_request")
		return
	}
	var result *PlanPreview
	effect := func() (bool, any, error) {
		if result != nil {
			return true, result, nil
		}
		if id == "" {
			return false, nil, nil
		}
		view, err := a.Store.PlanView(id, 0)
		if err != nil {
			return false, nil, err
		}
		return false, planDetailProjection(view), nil
	}
	precheck := func(state any) (int, string) {
		if id == "" {
			return 0, ""
		}
		detail, ok := state.(PlanDetail)
		if !ok {
			return 404, "plan_not_found"
		}
		if detail.Revision != body.Revision || detail.Digest != body.Digest {
			return 409, "revision_mismatch"
		}
		return 0, ""
	}
	// act's Send contract is shared by every mutation; the response is sanitized
	// here rather than returning raw PlanView context and instruction payloads.
	a.actResponse(w, r, ActionResult{Action: rt.ControlPlanRevise, Target: ActionTarget{PlanID: id, Revision: body.Revision, Digest: body.Digest}}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlPlanRevise, PlanID: id, Repository: body.Repository, Issue: body.Issue, Agent: body.Agent, Profile: body.Profile, Template: body.Template, Deterministic: body.Deterministic, Revision: body.Revision, Digest: body.Digest, Note: rt.BoundedNote(body.Note), ProductID: body.ProductID, AcceptanceFeature: body.AcceptanceFeature}, effect, precheck, func(response rt.ControlResponse) error {
			var view rt.PlanView
			if err := json.Unmarshal(response.Payload, &view); err != nil {
				return err
			}
			preview := PlanPreviewProjection(view)
			result = &preview
			return nil
		})
}

func (a *API) authorityPreview(w http.ResponseWriter, r *http.Request) {
	var request *rt.AuthorityRequest
	a.controllerRead(w, rt.ControlRequest{Command: rt.ControlAuthorityView, RunID: r.PathValue("id")}, &request)
}

func (a *API) authorize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		RequestID         string `json:"request_id"`
		Digest            string `json:"digest"`
		Decision          string `json:"decision"`
		Note              string `json:"note"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	if body.RequestID == "" || body.Digest == "" || (body.Decision != "approve" && body.Decision != "reject") {
		fail(w, 400, "invalid_request")
		return
	}
	id := r.PathValue("id")
	effect := func() (bool, any, error) {
		found, err := a.Store.HasRun(id)
		if err != nil || !found {
			return false, nil, err
		}
		receipts, err := readAuthorityReceipts(a.Store, id, body.RequestID, body.Digest)
		if err != nil {
			return false, nil, err
		}
		done := false
		for _, receipt := range receipts {
			if receipt.Decision == body.Decision {
				done = true
			}
		}
		status, err := a.Store.Status(id, a.now())
		if err != nil {
			return false, nil, err
		}
		return done, runDetailProjection(status), nil
	}
	a.act(w, r, ActionResult{Action: rt.ControlAuthorize, Target: ActionTarget{RunID: id, RequestID: body.RequestID, Digest: body.Digest}}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlAuthorize, RunID: id, AuthorityRequestID: body.RequestID, AuthorityDigest: body.Digest, AuthorityDecision: body.Decision, Note: rt.BoundedNote(body.Note)}, effect, func(state any) (int, string) {
			if state == nil {
				return 404, "run_not_found"
			}
			return 0, ""
		})
}

func boundedWorkspaceText(text string) string {
	runes := []rune(text)
	if len(runes) > 8192 {
		return string(runes[:8192]) + "…"
	}
	return text
}
