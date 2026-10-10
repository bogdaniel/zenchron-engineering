package controlplane

import (
	"net/http"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// WorkGraphSummary is one WorkGraph (#472) as a list sees it.
type WorkGraphSummary struct {
	ID         string `json:"graph_id"`
	Repository string `json:"repository"`
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
	Revision   int    `json:"revision"`
}

// WorkGraphDetail is one WorkGraph's current revision through its units -
// the Product Workspace's engineering-oriented drill-down. Product is the
// #476 product this graph is associated with, when any; a graph adopted
// before any association exists has none, never a fabricated one.
type WorkGraphDetail struct {
	ID         string                `json:"graph_id"`
	Repository string                `json:"repository"`
	AgentID    string                `json:"agent_id"`
	Name       string                `json:"name"`
	Revision   int                   `json:"revision"`
	Frontier   []string              `json:"frontier,omitempty"`
	Counts     map[string]int        `json:"counts"`
	Units      []WorkGraphUnitDetail `json:"units"`
	Product    *ProductSummary       `json:"product,omitempty"`
}

// WorkGraphUnitDetail is one unit, its own already-projected facts
// (rt.WorkGraphUnitView), its child run's full detail when it has one (the
// same runDetailProjection /v1/runs/{id} already renders - never
// re-derived), and the unit's own open #473 decisions.
type WorkGraphUnitDetail struct {
	UnitID         string   `json:"unit_id"`
	Purpose        string   `json:"purpose"`
	Role           string   `json:"role"`
	ExecutionKind  string   `json:"execution_kind,omitempty"`
	Issue          int      `json:"issue"`
	DependsOn      []string `json:"depends_on,omitempty"`
	State          string   `json:"state"`
	Reason         string   `json:"reason,omitempty"`
	RequiresReview bool     `json:"requires_review,omitempty"`
	ReviewApproved bool     `json:"review_approved,omitempty"`
	// Hold is a #508 operator gate on this unit before its first activation.
	// A unit with no row here is simply not held.
	Hold          *DecisionWaitRef `json:"hold,omitempty"`
	Run           *RunDetail       `json:"run,omitempty"`
	OpenDecisions []DecisionRef    `json:"open_decisions,omitempty"`
}

// DecisionWaitRef is a #508 WorkUnitHold's readiness-hold projection.
type DecisionWaitRef struct {
	Reference string `json:"reference,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// DecisionRef is one open #473 DecisionRequest: a question only a human or
// designated authority may answer. It has no resolution in this console
// (#398's governed action allowlist does not include decision-resolve) -
// docs/control-plane.md and the console page say where to resolve it.
type DecisionRef struct {
	ID          string    `json:"id"`
	Purpose     string    `json:"purpose,omitempty"`
	Body        string    `json:"body,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

// workGraphUnitDetailProjection projects one unit. runDetail and openDecisions
// are nil-safe lookups the caller supplies, so this stays pure and
// unit-testable without a store.
func workGraphUnitDetailProjection(unit rt.WorkGraphUnitView, run *RunDetail, decisions []DecisionRef) WorkGraphUnitDetail {
	out := WorkGraphUnitDetail{
		UnitID: unit.UnitID, Purpose: unit.Purpose, Role: string(unit.Role),
		ExecutionKind: string(unit.ExecutionKind), Issue: unit.Issue, DependsOn: unit.DependsOn,
		State: string(unit.State), Reason: unit.Reason,
		RequiresReview: unit.RequiresReview, ReviewApproved: unit.ReviewApproved,
		Run: run, OpenDecisions: decisions,
	}
	if hold := unit.AwaitingDecision; hold != nil {
		out.Hold = &DecisionWaitRef{Reference: hold.Reference, Detail: hold.Detail}
	}
	return out
}

// decisionRefProjection projects one open #473 message as a DecisionRef.
func decisionRefProjection(m orchestration.EngineeringMessage) DecisionRef {
	return DecisionRef{ID: m.ID, Purpose: m.Purpose, Body: m.Body, RequestedBy: m.Source.Unit, RequestedAt: m.AdmittedAt}
}

// workGraphDetailProjection assembles one graph's detail. runOf and
// decisionsOf are the only store-touching inputs; everything else is pure,
// mirroring planDetailProjection's shape.
func workGraphDetailProjection(view rt.WorkGraphView, product *ProductSummary,
	runOf func(runID string) (*RunDetail, error), decisionsOf func(batchID string) ([]orchestration.EngineeringMessage, error)) (WorkGraphDetail, error) {
	out := WorkGraphDetail{
		ID: view.GraphID, Repository: view.Repository, AgentID: view.AgentID, Name: view.Name,
		Revision: view.Revision, Frontier: view.Frontier, Product: product,
		Counts: map[string]int{
			"total": view.Counts.Total, "ready": view.Counts.Ready, "blocked": view.Counts.Blocked,
			"invalidated": view.Counts.Invalidated, "unknown": view.Counts.Unknown,
			"awaiting_decision": view.Counts.AwaitingDecision,
			"running":           view.Counts.Activated.Running,
			"handoff_pending":   view.Counts.Activated.HandoffPending,
			"completed":         view.Counts.Activated.Completed,
			"failed":            view.Counts.Activated.Failed,
			"stopped":           view.Counts.Activated.Stopped,
		},
	}
	for _, unit := range view.Units {
		var run *RunDetail
		var decisions []DecisionRef
		if unit.RunID != "" {
			r, err := runOf(unit.RunID)
			if err != nil {
				return WorkGraphDetail{}, err
			}
			run = r
		}
		if unit.BatchID != "" {
			messages, err := decisionsOf(unit.BatchID)
			if err != nil {
				return WorkGraphDetail{}, err
			}
			for _, m := range messages {
				decisions = append(decisions, decisionRefProjection(m))
			}
		}
		out.Units = append(out.Units, workGraphUnitDetailProjection(unit, run, decisions))
	}
	return out, nil
}

// unresolvedOpenDecisions reads a batch's open #473 messages through the
// store, then drops any that a #508 DecisionResolution already answers:
// OrchestrationStatus's own OpenDecisions is message-liveness-only and does
// not itself know about a resolution.
func unresolvedOpenDecisions(store *rt.ReadStore, now time.Time) func(batchID string) ([]orchestration.EngineeringMessage, error) {
	return func(batchID string) ([]orchestration.EngineeringMessage, error) {
		view, err := store.OrchestrationStatus(batchID, now)
		if err != nil {
			return nil, err
		}
		var open []orchestration.EngineeringMessage
		for _, message := range view.OpenDecisions {
			_, resolved, err := store.DecisionResolution(message.ID)
			if err != nil {
				return nil, err
			}
			if !resolved {
				open = append(open, message)
			}
		}
		return open, nil
	}
}

// readWorkGraphDetail is shared by the JSON route and the page, exactly as
// readPlanDetail is for plans.
func readWorkGraphDetail(store *rt.ReadStore, now time.Time, graphID string) (WorkGraphDetail, int, string) {
	view, err := store.WorkGraphStatus(graphID, now)
	if err != nil {
		return WorkGraphDetail{}, 404, "not_found"
	}
	var product *ProductSummary
	if productID, found, err := store.AssociatedProduct(graphID); err != nil {
		return WorkGraphDetail{}, 500, "read_failed"
	} else if found {
		p, found, err := store.CurrentProduct(productID)
		if err != nil {
			return WorkGraphDetail{}, 500, "read_failed"
		}
		if found {
			product = &ProductSummary{ID: p.ID, Name: p.Name, Revision: p.Revision, Repositories: p.Repositories}
		}
	}
	runOf := func(runID string) (*RunDetail, error) {
		status, err := store.Status(runID, now)
		if err != nil {
			return nil, err
		}
		detail := runDetailProjection(status)
		return &detail, nil
	}
	detail, err := workGraphDetailProjection(view, product, runOf, unresolvedOpenDecisions(store, now))
	if err != nil {
		return WorkGraphDetail{}, 500, "read_failed"
	}
	return detail, 200, ""
}

func (a *API) workGraphs(w http.ResponseWriter, r *http.Request) {
	graphs, err := a.Store.WorkGraphs()
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := make([]WorkGraphSummary, 0, len(graphs))
	for _, g := range graphs {
		out = append(out, WorkGraphSummary{ID: g.ID, Repository: g.Repository, AgentID: g.AgentID, Name: g.Name, Revision: g.Revision})
	}
	send(w, 200, out)
}

func (a *API) workGraph(w http.ResponseWriter, r *http.Request) {
	detail, status, code := readWorkGraphDetail(a.Store, a.now(), r.PathValue("id"))
	if status != 200 {
		fail(w, status, code)
		return
	}
	send(w, 200, detail)
}
