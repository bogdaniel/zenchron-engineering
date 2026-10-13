package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// workspaceDecision keeps a request beside its durable owning unit. Resolving
// it still goes through the controller; this projection grants no authority.
type workspaceDecision struct {
	Binding               string
	Unavailable           string
	ObjectivePlanID       string
	ObjectivePlanRevision int
	ObjectivePlanDigest   string
	GraphID               string
	GraphName             string
	UnitID                string
	Repository            string
	Decision              DecisionRef
}

type workspacePublication struct {
	Run         RunDetail
	Unavailable string
}

type workspaceData struct {
	ObservedAt    time.Time
	Controller    Controller
	Plans         []Plan
	Objectives    []objectiveListItem
	PlansPrevLink string
	PlansNextLink string
	Decisions     []workspaceDecision
	Publications  []workspacePublication
	Offset        int64
	PrevLink      string
	NextLink      string
}

type objectiveListItem struct {
	Plan
	Repository string
	Issue      int
}

func (w *Web) handleWorkspace(rw http.ResponseWriter, r *http.Request) {
	offset, ok := pageOffset(r, "offset")
	if !ok {
		w.renderNotFound(rw)
		return
	}
	now := w.now()
	data := workspaceData{ObservedAt: now, Controller: w.liveController(now), Offset: offset}
	switch r.URL.Path {
	case "/agents":
		w.render(rw, agentsTemplate, data)
	case "/settings":
		w.render(rw, settingsTemplate, data)
	case "/plans", "/objectives":
		plans, more, err := w.Store.PlanSummariesPage(int(offset), webPageSize)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		for _, plan := range plans {
			projected := planProjection(plan.PlanSummary)
			data.Plans = append(data.Plans, projected)
			data.Objectives = append(data.Objectives, objectiveListItem{Plan: projected, Repository: plan.Repository, Issue: plan.Issue})
		}
		data.PrevLink, data.NextLink = pageLinks(r.URL.Path, "offset", offset, more)
		if r.URL.Path == "/objectives" {
			w.render(rw, objectivesTemplate, data)
			return
		}
		w.render(rw, plansTemplate, data)
	case "/decisions":
		plansOffset, ok := pageOffset(r, "plans_offset")
		if !ok {
			w.renderNotFound(rw)
			return
		}
		plans, plansMore, err := w.Store.PlanSummariesPage(int(plansOffset), webPageSize)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		for _, plan := range plans {
			if plan.State == "awaiting_approval" || plan.Error != "" {
				data.Plans = append(data.Plans, planProjection(plan.PlanSummary))
			}
		}
		data.PlansPrevLink, data.PlansNextLink = pageLinks(r.URL.Path, "plans_offset", plansOffset, plansMore)
		graphs, more, err := w.Store.WorkGraphs(int(offset), webPageSize)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		for _, graph := range graphs {
			detail, status, _ := readWorkGraphDetail(w.Store, now, graph.ID)
			if status != http.StatusOK {
				w.renderError(rw, nil)
				return
			}
			decisions, publications, err := workspaceGraphActions(w.Store, detail, graph)
			if err != nil {
				w.renderError(rw, err)
				return
			}
			data.Decisions = append(data.Decisions, decisions...)
			data.Publications = append(data.Publications, publications...)
		}
		data.Decisions = bindWorkspaceDecisionActions(data.Decisions, data.Controller)
		data.PrevLink, data.NextLink = pageLinks(r.URL.Path, "offset", offset, more)
		w.render(rw, decisionsTemplate, data)
	}
}

func bindWorkspaceDecisionActions(decisions []workspaceDecision, controller Controller) []workspaceDecision {
	unavailable := controller.ActionsDisabled()
	for i := range decisions {
		decisions[i].Binding = ""
		if decisions[i].Unavailable != "" {
			continue
		}
		decisions[i].Unavailable = unavailable
		if decisions[i].Unavailable == "" {
			decisions[i].Binding = controller.ControllerBinding
		}
	}
	return decisions
}

func workspaceGraphActions(store *rt.ReadStore, detail WorkGraphDetail, graph orchestration.WorkGraph) ([]workspaceDecision, []workspacePublication, error) {
	var decisions []workspaceDecision
	var publications []workspacePublication
	unavailable := ""
	objectivePlanID := detail.ObjectivePlanID
	if graph.Revision != detail.Revision || graph.ObjectivePlan == nil && detail.ObjectivePlanID != "" || graph.ObjectivePlan != nil &&
		(graph.ObjectivePlan.ID != detail.ObjectivePlanID || graph.ObjectivePlan.Revision != detail.ObjectivePlanRevision || graph.ObjectivePlan.Digest != detail.ObjectivePlanDigest) {
		unavailable = "the execution graph changed while its decision context was read"
	}
	if detail.ObjectivePlanID != "" {
		view, err := store.PlanView(detail.ObjectivePlanID, detail.ObjectivePlanRevision)
		var refused *rt.PlanRefusedError
		if err != nil && !errors.As(err, &refused) {
			return nil, nil, err
		}
		if err != nil || view.Plan.Digest != detail.ObjectivePlanDigest {
			unavailable = "the execution graph's exact plan binding cannot be verified"
		}
	} else if graph.ObjectivePlan != nil {
		unavailable = "the execution graph's exact plan binding cannot be verified"
	}
	for _, unit := range detail.Units {
		if unit.Hold != nil {
			_, resolved, err := store.DecisionResolution(unit.Hold.Reference)
			if err != nil {
				return nil, nil, err
			}
			if !resolved {
				hold, found, err := store.WorkUnitHold(unit.Hold.Reference)
				if err != nil {
					return nil, nil, err
				}
				if !found {
					return nil, nil, fmt.Errorf("decision hold %s is unavailable", unit.Hold.Reference)
				}
				decisions = append(decisions, workspaceDecision{
					Unavailable: unavailable, ObjectivePlanID: objectivePlanID,
					ObjectivePlanRevision: detail.ObjectivePlanRevision, ObjectivePlanDigest: detail.ObjectivePlanDigest,
					GraphID: detail.ID, GraphName: detail.Name, Repository: detail.Repository, UnitID: unit.UnitID,
					Decision: DecisionRef{ID: hold.ID, Purpose: hold.Purpose, RequestedBy: hold.RequestedBy.Actor,
						RequestedAt: hold.RequestedAt, ExpectedOutcomeKind: orchestration.DecisionAllowDeny},
				})
			}
		}
		for _, decision := range unit.OpenDecisions {
			decisions = append(decisions, workspaceDecision{
				Unavailable: unavailable, ObjectivePlanID: objectivePlanID,
				ObjectivePlanRevision: detail.ObjectivePlanRevision, ObjectivePlanDigest: detail.ObjectivePlanDigest,
				GraphID: detail.ID, GraphName: detail.Name, Repository: detail.Repository,
				UnitID: unit.UnitID, Decision: decision,
			})
		}
		if unit.Run != nil && unit.Run.AuthorityRequest != nil && len(unit.Run.AuthorityRequest.Missing) > 0 {
			publications = append(publications, workspacePublication{Run: *unit.Run, Unavailable: unavailable})
		}
	}
	return decisions, publications, nil
}
