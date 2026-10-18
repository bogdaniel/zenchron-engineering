package benchmark

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

type fakeSource struct {
	graph  runtime.WorkGraphView
	status map[string]runtime.StatusReport
	events map[string][]runtime.EngineeringEvent
	plan   []runtime.EngineeringEvent
	// pageSize forces multi-page journals; zero means one page.
	pageSize int
	endless  bool
}

func (f fakeSource) WorkGraphStatus(string, time.Time) (runtime.WorkGraphView, error) {
	if f.graph.GraphID == "" {
		return runtime.WorkGraphView{}, errors.New("unknown work graph")
	}
	return f.graph, nil
}

func (f fakeSource) Status(id string, _ time.Time) (runtime.StatusReport, error) {
	s, ok := f.status[id]
	if !ok {
		return runtime.StatusReport{}, errors.New("unknown run")
	}
	return s, nil
}

func (f fakeSource) EventsPage(id string, after int64, limit int) ([]runtime.EngineeringEvent, bool, error) {
	if f.endless {
		return []runtime.EngineeringEvent{{Sequence: after + 1, Type: runtime.EventHandoffReported}}, true, nil
	}
	var rest []runtime.EngineeringEvent
	for _, e := range f.events[id] {
		if e.Sequence > after {
			rest = append(rest, e)
		}
	}
	size := limit
	if f.pageSize > 0 && f.pageSize < size {
		size = f.pageSize
	}
	if len(rest) > size {
		return rest[:size], true, nil
	}
	return rest, false, nil
}

func (f fakeSource) PlanEvents(string) ([]runtime.EngineeringEvent, error) { return f.plan, nil }

func opAfter(seq int64, opID, kind string, attempt int, started, ended time.Time) runtime.EngineeringEvent {
	payload, _ := json.Marshal(runtime.RunOperation{ID: opID, RunID: "r", Kind: kind, Attempt: attempt,
		State: runtime.Succeeded, StartedAt: &started})
	return runtime.EngineeringEvent{Sequence: seq, Type: runtime.EventOperationAfter, OccurredAt: ended, Payload: payload}
}

func terminal(seq int64, at time.Time) runtime.EngineeringEvent {
	return runtime.EngineeringEvent{Sequence: seq, Type: runtime.EventRunCompleted, OccurredAt: at}
}

func status(agent string, created time.Time, d runtime.Disposition) runtime.StatusReport {
	return runtime.StatusReport{Repository: "o/r", Worker: runtime.WorkerIdentity{Agent: agent, ProviderKind: "codex_cli", Model: "m"},
		Phase: "x", Disposition: d, CreatedAt: created}
}

func twoWorkerSource() fakeSource {
	return fakeSource{
		graph: runtime.WorkGraphView{GraphID: "g", Revision: 2, RevisionDigest: "d",
			ObjectivePlan: &domain.PlanRef{ID: "plan-1", Revision: 1, Digest: "pd"},
			Units:         []runtime.WorkGraphUnitView{{UnitID: "u1", RunID: "r1", Issue: 1}, {UnitID: "u2", RunID: "r2", Issue: 2}}},
		status: map[string]runtime.StatusReport{
			"r1": status("codex", minute(0), runtime.Completed),
			"r2": status("claude", minute(4), runtime.Completed),
		},
		events: map[string][]runtime.EngineeringEvent{
			"r1": {opAfter(1, "o1", "execute", 1, minute(1), minute(9)), terminal(2, minute(10))},
			"r2": {opAfter(1, "o2", "execute", 1, minute(5), minute(12)), {Sequence: 2, Type: runtime.EventReviewContinuationGranted},
				{Sequence: 3, Type: runtime.EventHandoffReported}, terminal(4, minute(14))},
		},
	}
}

func TestCollectComputesConcurrencyFromRealRunAndOperationIntervals(t *testing.T) {
	ev := Collect(twoWorkerSource(), Scope{GraphID: "g"}, minute(60))
	if len(ev.Problems) != 0 || len(ev.Runs) != 2 {
		t.Fatalf("problems=%v runs=%d", ev.Problems, len(ev.Runs))
	}
	if ev.RunSpans.Peak != 2 || ev.RunSpans.OverlapSeconds != 6*60 {
		t.Fatalf("run overlap [0,10] x [4,14] should be 6 minutes at peak 2: %+v", ev.RunSpans)
	}
	if ev.OperationSpan.Peak != 2 || ev.OperationSpan.OverlapSeconds != 4*60 {
		t.Fatalf("operation overlap [1,9] x [5,12] should be 4 minutes: %+v", ev.OperationSpan)
	}
	r2 := ev.Runs[1]
	if r2.Agent != "claude" || r2.ReviewContinuations != 1 || r2.HandoffsReported != 1 || !r2.Terminal {
		t.Fatalf("run facts not extracted: %+v", r2)
	}
	if r2.WallSeconds.Value == nil || *r2.WallSeconds.Value != 10*60 {
		t.Fatalf("wall seconds = %+v, want 600", r2.WallSeconds)
	}
}

func TestUnknownTokensAndCostStayUnknownNotZero(t *testing.T) {
	ev := Collect(twoWorkerSource(), Scope{GraphID: "g"}, minute(60))
	if ev.Tokens.IsKnown() || ev.Tokens.UnknownReason == "" {
		t.Fatalf("tokens must be unknown with a reason: %+v", ev.Tokens)
	}
	if ev.CostMicros.IsKnown() || ev.CostMicros.UnknownReason == "" {
		t.Fatalf("cost with no consumption events must be unknown: %+v", ev.CostMicros)
	}
	r, err := Build(Input{Arm: ArmPlanned, Runtime: &ev})
	if err != nil {
		t.Fatal(err)
	}
	if r.Measured.Cost.Known || r.Measured.Cost.AmountMicros != nil {
		t.Fatalf("result cost must stay unknown: %+v", r.Measured.Cost)
	}
	raw, _ := json.Marshal(r.Measured.Tokens)
	if !strings.Contains(string(raw), `"value":null`) {
		t.Fatalf("unknown renders as null, never 0: %s", raw)
	}
}

func consumption(known bool, micros int64) runtime.EngineeringEvent {
	p := runtime.PlanBudgetConsumedPayload{CostKnown: known}
	if known {
		p.CostMicros = &micros
	}
	raw, _ := json.Marshal(p)
	return runtime.EngineeringEvent{Type: runtime.EventPlanBudgetConsumed, Payload: raw}
}

func TestPlanCostIsKnownOnlyWhenEveryConsumptionReportedIt(t *testing.T) {
	src := twoWorkerSource()
	src.plan = []runtime.EngineeringEvent{consumption(true, 1500), consumption(true, 500)}
	ev := Collect(src, Scope{GraphID: "g"}, minute(60))
	if !ev.CostMicros.IsKnown() || *ev.CostMicros.Value != 2000 {
		t.Fatalf("all consumptions reported: want 2000, got %+v", ev.CostMicros)
	}
	src.plan = []runtime.EngineeringEvent{consumption(true, 1500), consumption(false, 0)}
	ev = Collect(src, Scope{GraphID: "g"}, minute(60))
	if ev.CostMicros.IsKnown() {
		t.Fatalf("a partial sum would understate cost; must be unknown: %+v", ev.CostMicros)
	}
}

func TestUnreadableRunBecomesAProblemNotASilentOmission(t *testing.T) {
	src := twoWorkerSource()
	delete(src.status, "r2")
	ev := Collect(src, Scope{GraphID: "g"}, minute(60))
	if len(ev.Problems) != 1 || !strings.Contains(ev.Problems[0], "r2") {
		t.Fatalf("problems = %v", ev.Problems)
	}
	if len(ev.Runs) != 1 {
		t.Fatalf("readable run must still be collected, got %d", len(ev.Runs))
	}
}

func TestUnreadableGraphIsAProblem(t *testing.T) {
	ev := Collect(fakeSource{}, Scope{GraphID: "missing"}, minute(1))
	if len(ev.Problems) != 1 || len(ev.Runs) != 0 {
		t.Fatalf("problems=%v runs=%d", ev.Problems, len(ev.Runs))
	}
}

func TestNonTerminalRunHasUnknownWallAndIsExcludedFromConcurrency(t *testing.T) {
	src := twoWorkerSource()
	src.events["r2"] = []runtime.EngineeringEvent{{Sequence: 1, Type: runtime.EventHandoffReported}}
	src.status["r2"] = status("claude", minute(4), runtime.Waiting)
	ev := Collect(src, Scope{GraphID: "g"}, minute(60))
	r2 := ev.Runs[1]
	if r2.Terminal || r2.WallSeconds.IsKnown() {
		t.Fatalf("a waiting run has no known wall time: %+v", r2.WallSeconds)
	}
	if ev.RunSpans.Spans != 1 || ev.RunSpans.Peak != 1 {
		t.Fatalf("non-terminal run must not fabricate an interval: %+v", ev.RunSpans)
	}
	if len(ev.Limitations) == 0 {
		t.Fatal("exclusion must be stated as a limitation")
	}
}

func TestJournalReadIsPagedAndBounded(t *testing.T) {
	src := twoWorkerSource()
	src.pageSize = 1
	ev := Collect(src, Scope{RunIDs: []string{"r2"}}, minute(60))
	if ev.Runs[0].Events != 4 || !ev.Runs[0].Terminal {
		t.Fatalf("paged read lost events: %+v", ev.Runs[0])
	}

	endless := Collect(fakeSource{status: map[string]runtime.StatusReport{"r": status("a", minute(0), runtime.Active)}, endless: true},
		Scope{RunIDs: []string{"r"}}, minute(1))
	if !endless.Runs[0].EventsTruncated || endless.Runs[0].Events > maxEventsPerRun+eventsPageSize {
		t.Fatalf("unbounded journal must be truncated: events=%d", endless.Runs[0].Events)
	}
	if len(endless.Problems) == 0 {
		t.Fatal("truncation must invalidate the result via a problem")
	}
}

func TestRunScopeIsBounded(t *testing.T) {
	ids := make([]string, maxRuns+5)
	for i := range ids {
		ids[i] = "r" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	ev := Collect(fakeSource{}, Scope{RunIDs: ids}, minute(1))
	found := false
	for _, p := range ev.Problems {
		found = found || strings.Contains(p, "at most")
	}
	if !found {
		t.Fatalf("oversized scope must be reported: %v", ev.Problems)
	}
}
