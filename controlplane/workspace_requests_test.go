package controlplane

import (
	"encoding/json"
	"fmt"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"testing"
	"time"
)

func TestHumanRequestsPageBothRequestAndGraphWindows(t *testing.T) {
	f := newActionFixture(t)
	for i := range 61 {
		at := time.Unix(int64(100+i), 0).UTC()
		graph, err := (orchestration.WorkGraphProposal{Name: fmt.Sprintf("graph-%03d", i), Revision: 1, Units: []orchestration.WorkUnit{{ID: "first", Purpose: "First bounded request", Role: domain.RoleImplementer, Issue: i*2 + 1}, {ID: "second", Purpose: "Second bounded request", Role: domain.RoleTester, Issue: i*2 + 2}}}).Compose("example/repo", "claude", "operator", at)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.store.AdoptWorkGraphRevision(graph); err != nil {
			t.Fatal(err)
		}
		for _, unit := range graph.Units {
			id, err := orchestration.WorkUnitHoldID(graph.ID, unit.ID)
			if err != nil {
				t.Fatal(err)
			}
			hold := orchestration.WorkUnitHold{SchemaVersion: orchestration.WorkUnitHoldSchemaVersion, ID: id, GraphID: graph.ID, UnitID: unit.ID, Purpose: "Choose bounded work", RequestedAt: at, RequestedBy: orchestration.DecisionResolutionAuthority{Actor: "operator", AuthorityKind: orchestration.AuthorityKindOperator, Provenance: "local_control_endpoint"}}
			if _, _, err := f.store.PlaceWorkUnitHold(hold); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen := map[string]bool{}
	for _, page := range []struct {
		query            string
		count            int
		more, graphsMore bool
	}{{"?limit=50", 50, true, true}, {"?limit=50&offset=50", 50, false, true}, {"?limit=50&graphs_offset=50", 22, false, false}} {
		var got HumanRequests
		if err := json.Unmarshal(wire(t, f.api, "/v1/workspace/requests"+page.query, "GET", f.api.Token, "human-requests", 200), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Requests) != page.count || got.HasMore != page.more || got.GraphsHasMore != page.graphsMore {
			t.Fatalf("page lost bounds: %+v", got)
		}
		for _, row := range got.Requests {
			if seen[row.ID] {
				t.Fatalf("repeated request %s", row.ID)
			}
			seen[row.ID] = true
			if row.Decision == nil || row.Decision.ExpectedOutcomeKind != orchestration.DecisionAllowDeny || row.GraphRevision != 1 {
				t.Fatalf("request grammar/context lost: %+v", row)
			}
		}
	}
	if len(seen) != 122 {
		t.Fatalf("lost requests: %d", len(seen))
	}
	for _, q := range []string{"?offset=-1", "?offset=0&offset=50", "?graphs_offset=-1", "?limit=51"} {
		wire(t, f.api, "/v1/workspace/requests"+q, "GET", f.api.Token, "error", 400)
	}
	wire(t, f.api, "/v1/workspace/requests", "GET", "", "error", 401)
	wire(t, f.api, "/v1/decisions/missing/resolution", "GET", f.api.Token, "error", 404)
	f.api.Observe = nil
	var unavailable HumanRequests
	if err := json.Unmarshal(wire(t, f.api, "/v1/workspace/requests", "GET", f.api.Token, "human-requests", 200), &unavailable); err != nil {
		t.Fatal(err)
	}
	if unavailable.Requests[0].Unavailable == "" {
		t.Fatal("unobserved controller looked actionable")
	}
	f.api.Store.Close()
	wire(t, f.api, "/v1/workspace/requests", "GET", f.api.Token, "error", 500)
}
