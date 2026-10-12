package controlplane

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// workspaceDecision keeps a request beside its durable owning unit. Resolving
// it still goes through the controller; this projection grants no authority.
type workspaceDecision struct {
	Binding         string
	ObjectivePlanID string
	GraphID         string
	GraphName       string
	UnitID          string
	Repository      string
	Decision        DecisionRef
}

type workspaceData struct {
	ObservedAt    time.Time
	Controller    Controller
	Plans         []Plan
	PlansPrevLink string
	PlansNextLink string
	Decisions     []workspaceDecision
	Publications  []RunDetail
	Offset        int64
	PrevLink      string
	NextLink      string
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
		fleet, err := w.Store.Fleet(now)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		end := min(int64(len(fleet.Plans)), offset+webPageSize)
		for i := offset; i < end; i++ {
			data.Plans = append(data.Plans, planProjection(fleet.Plans[i]))
		}
		data.PrevLink, data.NextLink = pageLinks(r.URL.Path, "offset", offset, end < int64(len(fleet.Plans)))
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
		fleet, err := w.Store.Fleet(now)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		var pendingPlans []Plan
		for _, plan := range fleet.Plans {
			if plan.State == "awaiting_approval" || plan.Revision > plan.ApprovedRevision {
				pendingPlans = append(pendingPlans, planProjection(plan))
			}
		}
		plansEnd := min(int64(len(pendingPlans)), plansOffset+webPageSize)
		data.Plans = pendingPlans[plansOffset:plansEnd]
		data.PlansPrevLink, data.PlansNextLink = pageLinks(r.URL.Path, "plans_offset", plansOffset, plansEnd < int64(len(pendingPlans)))
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
			objectivePlanID := ""
			if graph.ObjectivePlan != nil {
				objectivePlanID = graph.ObjectivePlan.ID
			}
			decisions, publications, err := workspaceGraphActions(w.Store, detail, objectivePlanID)
			if err != nil {
				w.renderError(rw, err)
				return
			}
			data.Decisions = append(data.Decisions, decisions...)
			data.Publications = append(data.Publications, publications...)
		}
		for i := range data.Decisions {
			data.Decisions[i].Binding = data.Controller.ControllerBinding
		}
		data.PrevLink, data.NextLink = pageLinks(r.URL.Path, "offset", offset, more)
		w.render(rw, decisionsTemplate, data)
	}
}

func workspaceGraphActions(store *rt.ReadStore, detail WorkGraphDetail, objectivePlanID string) ([]workspaceDecision, []RunDetail, error) {
	var decisions []workspaceDecision
	var publications []RunDetail
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
					ObjectivePlanID: objectivePlanID, GraphID: detail.ID, GraphName: detail.Name, Repository: detail.Repository, UnitID: unit.UnitID,
					Decision: DecisionRef{ID: hold.ID, Purpose: hold.Purpose, RequestedBy: hold.RequestedBy.Actor,
						RequestedAt: hold.RequestedAt, ExpectedOutcomeKind: orchestration.DecisionAllowDeny},
				})
			}
		}
		for _, decision := range unit.OpenDecisions {
			decisions = append(decisions, workspaceDecision{
				ObjectivePlanID: objectivePlanID, GraphID: detail.ID, GraphName: detail.Name, Repository: detail.Repository,
				UnitID: unit.UnitID, Decision: decision,
			})
		}
		if unit.Run != nil && unit.Run.AuthorityRequest != nil && len(unit.Run.AuthorityRequest.Missing) > 0 {
			publications = append(publications, *unit.Run)
		}
	}
	return decisions, publications, nil
}
