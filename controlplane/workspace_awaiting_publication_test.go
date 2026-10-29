package controlplane

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// #481 rehearsal: the read store cannot derive an exact authority request, so a
// graph unit's run parked on human authority carried AuthorityRequest == nil and
// Action required listed no publication request at all. The durable wait reason
// is what surfaces it. Mutation: restore the AuthorityRequest-only condition in
// workspaceGraphActions and the parked run disappears from the list.
func TestWorkspaceGraphActionsListsARunParkedOnHumanAuthority(t *testing.T) {
	graph := orchestration.WorkGraph{Revision: 1}
	detail := WorkGraphDetail{Revision: 1, Units: []WorkGraphUnitDetail{
		{UnitID: "parked", Run: &RunDetail{ID: "run-parked", AwaitingHumanAuthority: true}},
		{UnitID: "other-wait", Run: &RunDetail{ID: "run-other"}},
		{UnitID: "no-run"},
	}}
	_, publications, err := workspaceGraphActions(nil, detail, graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 1 || publications[0].Run.ID != "run-parked" {
		t.Fatalf("publication requests = %+v, want only the run parked on authority", publications)
	}
}

// Only the closed wait reason becomes a signal; free-text reasons never do.
func TestRunDetailFlagsOnlyTheAuthorityWaitReason(t *testing.T) {
	for reason, want := range map[string]bool{"awaiting_authority": true, "goal_state_reached": false, "SECRET_PAYLOAD": false} {
		got := runDetailProjection(rt.StatusReport{Disposition: rt.Waiting, Reason: reason}).AwaitingHumanAuthority
		if got != want {
			t.Fatalf("reason %q flagged = %t, want %t", reason, got, want)
		}
	}
	if runDetailProjection(rt.StatusReport{Disposition: rt.Failed, Reason: "awaiting_authority"}).AwaitingHumanAuthority {
		t.Fatal("a run that is not waiting is not awaiting authority")
	}
}
