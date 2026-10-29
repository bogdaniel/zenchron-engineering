package runtime

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// firstRunAsks makes the first run the host ever executes (the graph's root)
// commit its candidate and ask a human decision, on its first invocation only.
type firstRunAsks struct {
	*fleetProvider
	document string
	mu       sync.Mutex
	asker    string
}

func (w *firstRunAsks) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	w.mu.Lock()
	if w.asker == "" && request.Purpose != InvocationHandoffRepair {
		w.asker = request.RunID
		if err := os.WriteFile(request.MessagePath, []byte(w.document), 0o600); err != nil {
			w.mu.Unlock()
			return ExecutionResult{}, err
		}
	}
	w.mu.Unlock()
	return w.fleetProvider.Execute(ctx, request)
}

// #480 invariant 10 on the generic WorkGraph path: a unit whose run committed a
// candidate AND asked a human decision in the same invocation is not completed
// work. Its dependent must not be activated against that candidate while the
// question is open, because the answer may change the candidate.
func TestAWorkUnitThatCommittedAndAskedIsNotCompletedWhileItsQuestionIsOpen(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	worker := &firstRunAsks{fleetProvider: fixture.worker, document: decisionWaitDocument(t)}
	fixture.deps.Provider = worker
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
		{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: graphUnitA},
		{ID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: graphUnitB, DependsOn: []string{"a"}},
	})
	aRun := ""
	asked := false
	for range 40 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if aRun == "" {
			aRun = fixture.graph(view.GraphID).unit(t, "a").RunID
		}
		open, err := fixture.store.OpenDecisionRequestsForRun(aRun)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 0 {
			continue
		}
		asked = true
		view = fixture.graph(view.GraphID)
		if b := view.unit(t, "b"); b.RunID != "" {
			t.Fatalf("b was activated (run %s) while a's decision %s is unanswered; a is %s", b.RunID, open[0].ID, view.unit(t, "a").State)
		}
		if a := view.unit(t, "a"); a.State == orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("a is completed while its decision %s is unanswered", open[0].ID)
		}
	}
	if !asked {
		view = fixture.graph(view.GraphID)
		t.Logf("a=%s b=%s", view.unit(t, "a").State, view.unit(t, "b").State)
		t.Fatal("the question never became an open decision; the hazard is not exercised")
	}
}
