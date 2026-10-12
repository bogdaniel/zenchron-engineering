package controlplane

import "net/http"

// objectiveData composes sanitized owner projections; it persists no state
// and offers no authority beyond the existing governed action routes.
type objectiveData struct {
	Plan            PlanDetail
	Graph           *WorkGraphDetail
	Controller      Controller
	BindingMismatch bool
	Decisions       []workspaceDecision
	Publications    []workspacePublication
	Lifecycle       Plan
	NoPlan          bool
}

func (w *Web) handleObjective(rw http.ResponseWriter, r *http.Request) {
	plan, status, _ := readPlanDetail(w.Store, r)
	revision, _ := pageNumber(r, "revision", 0, 1<<31-1)
	if status == http.StatusNotFound && revision == 0 {
		identity, found, err := w.Store.PlanIdentity(r.PathValue("id"))
		if err != nil {
			w.renderError(rw, err)
			return
		}
		if found && identity.Revision == 0 {
			data := objectiveData{
				NoPlan:    true,
				Plan:      PlanDetail{ID: identity.PlanID, Repository: identity.Repository, Shown: "not recorded"},
				Lifecycle: Plan{Unavailable: true}, Controller: w.liveController(w.now()),
			}
			w.render(rw, objectiveTemplate, data)
			return
		}
	}
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		w.renderNotFound(rw)
		return
	}
	if status != http.StatusOK {
		w.renderError(rw, nil)
		return
	}
	data := objectiveData{Plan: plan, Controller: w.liveController(w.now())}
	summary, err := w.Store.PlanSummary(plan.ID)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	data.Lifecycle = planProjection(summary)
	graph, found, err := w.Store.ObjectiveGraph(plan.ID)
	if err != nil {
		w.renderError(rw, err)
		return
	}
	if found {
		detail, status, _ := readWorkGraphDetail(w.Store, w.now(), graph.ID)
		if status != http.StatusOK {
			w.renderError(rw, nil)
			return
		}
		if detail.ObjectivePlanID != plan.ID {
			w.renderError(rw, nil)
			return
		}
		data.Graph = &detail
		data.BindingMismatch = detail.ObjectivePlanRevision != plan.Revision || detail.ObjectivePlanDigest != plan.Digest
		data.Decisions, data.Publications, err = workspaceGraphActions(w.Store, detail)
		if err != nil {
			w.renderError(rw, err)
			return
		}
		data.Decisions = bindWorkspaceDecisionActions(data.Decisions, data.Controller)
		if data.BindingMismatch {
			for i := range data.Decisions {
				data.Decisions[i].Binding = ""
				data.Decisions[i].Unavailable = "inspect the execution graph's exact plan context before resolving this request"
			}
			for i := range data.Publications {
				data.Publications[i].Unavailable = "inspect the execution graph's exact plan context before publication"
			}
		}
	}
	w.render(rw, objectiveTemplate, data)
}
