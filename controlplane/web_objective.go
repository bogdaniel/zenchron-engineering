package controlplane

import "net/http"

// objectiveData composes sanitized owner projections; it persists no state
// and offers no authority beyond the existing governed action routes.
type objectiveData struct {
	Plan            PlanDetail
	Graph           *WorkGraphDetail
	Controller      Controller
	BindingMismatch bool
}

func (w *Web) handleObjective(rw http.ResponseWriter, r *http.Request) {
	plan, status, _ := readPlanDetail(w.Store, r)
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		w.renderNotFound(rw)
		return
	}
	if status != http.StatusOK {
		w.renderError(rw, nil)
		return
	}
	data := objectiveData{Plan: plan, Controller: w.liveController(w.now())}
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
		data.Graph = &detail
		data.BindingMismatch = graph.ObjectivePlan.Revision != plan.Revision || graph.ObjectivePlan.Digest != plan.Digest
	}
	w.render(rw, objectiveTemplate, data)
}
