package controlplane

import (
	"errors"
	"net/http"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// PlanContext reuses the canonical reducer's pending predicate. Receipt is a
// historical journal fact, never a new workflow state or an authorization.
type PlanContext struct {
	Plan    PlanDetail       `json:"plan"`
	Pending bool             `json:"pending"`
	Receipt *DecisionReceipt `json:"receipt,omitempty"`
}

type DecisionReceipt struct {
	Status     string    `json:"status"`
	Revision   int       `json:"revision"`
	Digest     string    `json:"digest"`
	Sequence   int64     `json:"sequence"`
	OccurredAt time.Time `json:"occurred_at"`
}

type PlanIndexRow struct {
	rt.PlanIdentity
	Pending bool   `json:"pending"`
	Shown   string `json:"shown"`
}

type PlanIndex struct {
	Plans   []PlanIndexRow `json:"plans"`
	Offset  int64          `json:"offset"`
	HasMore bool           `json:"has_more"`
}

func (a *API) plans(w http.ResponseWriter, r *http.Request) {
	offset, ok := pageNumber(r, "offset", 0, 1<<31-1)
	limit, ok2 := pageNumber(r, "limit", 50, 50)
	query := r.URL.Query().Get("q")
	if !ok || !ok2 || limit < 1 || len(query) > 200 || len(r.URL.Query()["q"]) > 1 {
		fail(w, 400, "invalid_page")
		return
	}
	identities, more, err := a.Store.PlanIdentitiesPage(int(offset), int(limit), query)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	index := PlanIndex{Plans: []PlanIndexRow{}, Offset: offset, HasMore: more}
	// ponytail: at most 50 canonical reductions per page; journal lengths are
	// unbounded. Add a journal-derived read summary if measured cost requires it.
	for _, identity := range identities {
		row := PlanIndexRow{PlanIdentity: identity, Shown: "attempt_only"}
		if identity.Revision > 0 {
			view, err := a.Store.PlanView(identity.PlanID, identity.Revision)
			if err != nil {
				fail(w, 500, "read_failed")
				return
			}
			row.Pending = view.Snapshot.AwaitingDecision(identity.Revision)
			row.Shown = planDetailProjection(view).Shown
		}
		index.Plans = append(index.Plans, row)
	}
	send(w, 200, index)
}

func (a *API) planContext(w http.ResponseWriter, r *http.Request) {
	revision, ok := pageNumber(r, "revision", 0, 1<<31-1)
	if !ok {
		fail(w, 400, "invalid_page")
		return
	}
	view, err := a.Store.PlanView(r.PathValue("id"), int(revision))
	if err != nil {
		var missing *rt.PlanRefusedError
		if errors.As(err, &missing) {
			fail(w, 404, "not_found")
			return
		}
		fail(w, 500, "read_failed")
		return
	}
	events, err := a.Store.PlanEvents(view.Plan.ID)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	// Refuse a torn read rather than combining an old pending projection with
	// a newly written receipt. No retries, and no mutation on a GET.
	if len(events) > 0 && events[len(events)-1].EventHash != view.Snapshot.Cursor.LastEventHash {
		fail(w, 409, "snapshot_changed")
		return
	}
	context := PlanContext{Plan: planDetailProjection(view), Pending: view.Snapshot.AwaitingDecision(view.Plan.Revision)}
	for _, event := range events {
		verb := "approve"
		if event.Type == rt.EventPlanRejected {
			verb = "reject"
		}
		decision, found := rt.DecisionEventFor([]rt.EngineeringEvent{event}, verb, view.Plan.Revision, view.Plan.Digest)
		if found {
			context.Receipt = &DecisionReceipt{Status: string(decision.Status), Revision: decision.Revision,
				Digest: decision.Digest, Sequence: event.Sequence, OccurredAt: event.OccurredAt}
		}
	}
	send(w, 200, context)
}
