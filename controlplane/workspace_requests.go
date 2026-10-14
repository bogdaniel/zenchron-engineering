package controlplane

import (
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"net/http"
	"time"
)

// HumanRequest is a bounded presentation of existing graph-owned requests.
// It does not decide eligibility; the serving controller rechecks every action.
type HumanRequest struct {
	GraphRevision int          `json:"graph_revision"`
	Kind          string       `json:"kind"`
	ID            string       `json:"id"`
	GraphID       string       `json:"graph_id"`
	GraphName     string       `json:"graph_name"`
	UnitID        string       `json:"unit_id,omitempty"`
	Repository    string       `json:"repository"`
	PlanID        string       `json:"objective_plan_id,omitempty"`
	PlanRevision  int          `json:"objective_plan_revision,omitempty"`
	PlanDigest    string       `json:"objective_plan_digest,omitempty"`
	Unavailable   string       `json:"unavailable,omitempty"`
	Decision      *DecisionRef `json:"decision,omitempty"`
	Run           *RunDetail   `json:"run,omitempty"`
}
type HumanRequests struct {
	Requests      []HumanRequest `json:"requests"`
	Offset        int64          `json:"offset"`
	HasMore       bool           `json:"has_more"`
	GraphsOffset  int64          `json:"graphs_offset"`
	GraphsHasMore bool           `json:"graphs_has_more"`
}

func (a *API) workspaceRequests(w http.ResponseWriter, r *http.Request) {
	offset, ok := pageNumber(r, "offset", 0, 1<<31-1)
	graphsOffset, ok2 := pageNumber(r, "graphs_offset", 0, 1<<31-1)
	limit, ok3 := pageNumber(r, "limit", 50, 50)
	if !ok || !ok2 || !ok3 || limit < 1 {
		fail(w, 400, "invalid_page")
		return
	}
	graphs, more, err := a.Store.WorkGraphs(int(graphsOffset), 50)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	live, err := a.Store.Controller(a.ControllerRoot, a.Observe, a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	rows := []HumanRequest{}
	// ponytail: reduce at most 50 existing graph projections per page; units are
	// unbounded. Index journal-owned requests if measured read latency grows.
	for _, graph := range graphs {
		detail, status, _ := readWorkGraphDetail(a.Store, a.now(), graph.ID)
		if status != http.StatusOK {
			fail(w, 500, "read_failed")
			return
		}
		decisions, publications, err := workspaceGraphActions(a.Store, detail, graph)
		if err != nil {
			fail(w, 500, "read_failed")
			return
		}
		for _, d := range bindWorkspaceDecisionActions(decisions, controllerProjection(live)) {
			decision := d.Decision
			rows = append(rows, HumanRequest{Kind: "decision", GraphRevision: detail.Revision, ID: decision.ID, GraphID: d.GraphID, GraphName: d.GraphName, UnitID: d.UnitID,
				Repository: d.Repository, PlanID: d.ObjectivePlanID, PlanRevision: d.ObjectivePlanRevision, PlanDigest: d.ObjectivePlanDigest,
				Unavailable: d.Unavailable, Decision: &decision})
		}
		for _, p := range publications {
			run := p.Run
			unavailable := p.Unavailable
			if unavailable == "" {
				unavailable = controllerProjection(live).ActionsDisabled()
			}
			rows = append(rows, HumanRequest{Kind: "publication", GraphRevision: detail.Revision, ID: run.ID, GraphID: detail.ID, GraphName: detail.Name, Repository: detail.Repository,
				PlanID: detail.ObjectivePlanID, PlanRevision: detail.ObjectivePlanRevision, PlanDigest: detail.ObjectivePlanDigest, Unavailable: unavailable, Run: &run})
		}
	}
	end := min(int64(len(rows)), offset+limit)
	selected := []HumanRequest{}
	if offset < end {
		selected = rows[offset:end]
	}
	send(w, 200, HumanRequests{Requests: selected, Offset: offset, HasMore: end < int64(len(rows)), GraphsOffset: graphsOffset, GraphsHasMore: more})
}

// Resolution reads a durable receipt without exposing actor identity or notes.
func (a *API) decisionResolution(w http.ResponseWriter, r *http.Request) {
	value, found, err := a.Store.DecisionResolution(r.PathValue("id"))
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	if !found {
		fail(w, 404, "not_found")
		return
	}
	send(w, 200, struct {
		RequestID  string                        `json:"request_id"`
		Outcome    orchestration.DecisionOutcome `json:"outcome"`
		ResolvedAt time.Time                     `json:"resolved_at"`
	}{value.RequestID, value.Outcome, value.ResolvedAt})
}
