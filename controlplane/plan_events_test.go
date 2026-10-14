package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestPlanEventsAreBoundedPrivateAndIncludeAttempts(t *testing.T) {
	f := newActionFixture(t)
	if _, err := f.store.ClaimPlanAttempt("attempt", "example/repo", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for i := range 103 {
		payload, err := json.Marshal(rt.PlanAttemptRefusedPayload{AttemptID: rt.PendingAttemptID, Revision: 1,
			Origin: domain.ProposalOriginInitial, Errors: []string{"SECRET_REFUSAL"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.AppendPlanEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion,
			ID: fmt.Sprintf("attempt-%d", i), PlanID: "attempt", Type: rt.EventPlanAttemptRefused,
			OccurredAt: time.Now().UTC(), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	cursor, count := int64(0), 0
	for {
		var page PlanEvents
		path := fmt.Sprintf("/v1/plans/attempt/events?after=%d&limit=50", cursor)
		if err := json.Unmarshal(wire(t, f.api, path, "GET", f.api.Token, "plan-events", 200), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 50 || page.PlanID != "attempt" || (page.HasMore && page.Next <= cursor) {
			t.Fatalf("invalid page: %+v", page)
		}
		for _, event := range page.Events {
			if event.Sequence <= cursor {
				t.Fatal("cursor repeated an event")
			}
			cursor = event.Sequence
			count++
		}
		if !page.HasMore {
			break
		}
	}
	if count != 103 {
		t.Fatalf("events lost: %d", count)
	}
	wire(t, f.api, "/v1/plans/p/events", "GET", f.api.Token, "plan-events", 200)
	wire(t, f.api, "/v1/plans/missing/events", "GET", f.api.Token, "error", 404)
	wire(t, f.api, "/v1/plans/attempt/events", "GET", "", "error", 401)
	for _, suffix := range []string{"?after=-1", "?limit=0", "?limit=501", "?after=0&after=1"} {
		wire(t, f.api, "/v1/plans/attempt/events"+suffix, "GET", f.api.Token, "error", 400)
	}
	if _, _, err := f.api.Store.PlanEventsPage("attempt", -1, 1); err == nil {
		t.Fatal("negative cursor accepted")
	}
	if _, _, err := f.api.Store.PlanEventsPage("attempt", 0, 501); err == nil {
		t.Fatal("unbounded page accepted")
	}
	f.api.Store.Close()
	wire(t, f.api, "/v1/plans/attempt/events", "GET", f.api.Token, "error", 500)
}
