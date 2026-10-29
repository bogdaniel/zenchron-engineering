package controlplane

import "net/http"

// PlanEvents exposes the same bounded journal metadata as run events. Plan
// payloads, objective prose, reasoning and operator notes remain private.
type PlanEvents struct {
	PlanID  string  `json:"plan_id"`
	Events  []Event `json:"events"`
	Next    int64   `json:"next_after"`
	HasMore bool    `json:"has_more"`
}

func (a *API) planEvents(w http.ResponseWriter, r *http.Request) {
	after, ok := pageNumber(r, "after", 0, 1<<63-1)
	limit, ok2 := pageNumber(r, "limit", 100, 500)
	if !ok || !ok2 || limit == 0 {
		fail(w, 400, "invalid_page")
		return
	}
	id := r.PathValue("id")
	exists, err := a.Store.HasPlanIdentity(id)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	if !exists {
		fail(w, 404, "not_found")
		return
	}
	events, more, err := a.Store.PlanEventsPage(id, after, int(limit))
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := PlanEvents{PlanID: id, Events: []Event{}, Next: after, HasMore: more}
	for _, e := range events {
		out.Events = append(out.Events, eventProjection(e))
		out.Next = e.Sequence
	}
	send(w, 200, out)
}
