package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestWorkGraphDetailProjectionCarriesHoldsRunsAndOpenDecisions pins the one
// projection both the JSON route and the page use: a unit with no run gets
// neither a Run nor a decisions lookup, a held unit carries its own wait
// reference, an activated unit's open decisions come from its own batch
// alone, and a run with no cost field is never shown as zero.
func TestWorkGraphDetailProjectionCarriesHoldsRunsAndOpenDecisions(t *testing.T) {
	requestedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	view := rt.WorkGraphView{
		GraphID: "graph-1", Repository: "acme/repo", AgentID: "claude", Name: "checkout",
		Revision: 2, Frontier: []string{"b"},
		Counts: orchestration.WorkGraphCounts{Total: 2, Ready: 1, Activated: orchestration.Counts{Completed: 1}},
		Units: []rt.WorkGraphUnitView{
			{
				UnitID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: 101,
				State: orchestration.UnitState(orchestration.ItemCompleted), RequiresReview: true, ReviewApproved: true,
				RunID: "run-a", BatchID: "batch-a",
			},
			{
				UnitID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: 102,
				DependsOn: []string{"a"}, State: orchestration.UnitBlocked, Reason: "held by an operator",
				AwaitingDecision: &orchestration.DecisionWait{Reference: "hold-1", Detail: "approve the backend choice"},
			},
		},
	}
	runA := &RunDetail{ID: "run-a", Repository: "acme/repo"}
	decisions := map[string][]orchestration.EngineeringMessage{
		"batch-a": {{
			ID: "decision-1", Kind: orchestration.KindDecisionRequest, Purpose: "which backend?",
			Source: orchestration.MessageSource{Unit: "a"}, AdmittedAt: requestedAt,
		}},
	}
	runOf := func(runID string) (*RunDetail, error) {
		if runID != "run-a" {
			t.Fatalf("runOf called for unexpected run %q", runID)
		}
		return runA, nil
	}
	decisionsOf := func(batchID string) ([]orchestration.EngineeringMessage, error) { return decisions[batchID], nil }

	detail, err := workGraphDetailProjection(view, nil, runOf, decisionsOf)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]WorkGraphUnitDetail{}
	for _, u := range detail.Units {
		byID[u.UnitID] = u
	}
	a, b := byID["a"], byID["b"]
	if a.Run == nil || a.Run.ID != "run-a" {
		t.Fatalf("a's run must be the one runOf returned: %+v", a.Run)
	}
	if len(a.OpenDecisions) != 1 || a.OpenDecisions[0].ID != "decision-1" || a.OpenDecisions[0].RequestedBy != "a" {
		t.Fatalf("a's open decisions must come from its own batch alone: %+v", a.OpenDecisions)
	}
	if b.Run != nil {
		t.Fatalf("b has no run yet, got %+v", b.Run)
	}
	if b.Hold == nil || b.Hold.Reference != "hold-1" || b.Hold.Detail != "approve the backend choice" {
		t.Fatalf("b's operator hold must be carried through: %+v", b.Hold)
	}
	if len(b.OpenDecisions) != 0 {
		t.Fatalf("b has no batch yet, so no decision lookup must have been attempted: %+v", b.OpenDecisions)
	}

	out := renderTemplate(t, workGraphDetailTemplate, workGraphDetailData{Graph: detail})
	for _, want := range []string{
		"checkout", `href="/runs/run-a"`, "which backend?", "approve the backend choice",
		"autonomy workgraph resolve", "unknown - not reported for this run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered work graph detail lacks %q", want)
		}
	}
}

// TestWorkGraphDetailProjectionPropagatesStoreErrors confirms a failed run or
// decision lookup fails the whole projection rather than silently omitting
// that unit's facts.
func TestWorkGraphDetailProjectionPropagatesStoreErrors(t *testing.T) {
	view := rt.WorkGraphView{GraphID: "g", Units: []rt.WorkGraphUnitView{{UnitID: "a", RunID: "run-a"}}}
	failingRun := func(string) (*RunDetail, error) { return nil, errors.New("read failed") }
	noDecisions := func(string) ([]orchestration.EngineeringMessage, error) { return nil, nil }
	if _, err := workGraphDetailProjection(view, nil, failingRun, noDecisions); err == nil {
		t.Fatal("a failed run lookup must fail the projection, not silently omit it")
	}
}
