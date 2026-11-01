package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// spendingAssurance is the real verifier's shape for ONE run: its first
// verification spends more wall time than the run's whole assurance allowance
// and fails. Every other run is verified normally.
type spendingAssurance struct {
	mu     sync.Mutex
	clock  *steppingClock
	target string
	spent  bool
	inner  *lockedAssurance
}

func (a *spendingAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return a.inner.ProducedEvidenceClasses()
}

func (a *spendingAssurance) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	a.mu.Lock()
	first := request.RunID == a.target && !a.spent
	if first {
		a.spent = true
	}
	a.mu.Unlock()
	if !first {
		return a.inner.Assure(ctx, request)
	}
	a.clock.advance(2 * time.Hour)
	return AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", FailureClass: FailureVerification}, nil
}

// #693, exactly: the worker reports completion and its handoff is admitted,
// exact-candidate assurance then fails and the run's assurance allowance is
// spent, so the unit projects FAILED - with a handoff. The dependent stays
// blocked, the question is raised, and the old handoff satisfies nothing.
func TestAProducerThatFailsAssuranceAfterItsHandoffWasAdmittedRaisesTheEpochRequest(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	failing := issueRunOf(t, fixture, graphUnitC)
	fixture.deps.Budgets.AssuranceWallLimit = time.Hour
	fixture.deps.Assurance = &spendingAssurance{clock: fixture.clock, target: failing, inner: &lockedAssurance{inner: &FakeAssuranceProvider{}}}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())

	var request *orchestration.EngineeringMessage
	for range 120 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		run, ok := storedRun(t, fixture.phase8Fixture, failing)
		if !ok || run.Orchestration == nil {
			continue
		}
		messages, err := fixture.store.ScopeMessages(run.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				request = &message
			}
		}
		if request != nil {
			break
		}
	}
	if request == nil {
		t.Fatal("a producer that failed assurance after its handoff was admitted never got an epoch_authorization request")
	}

	// The precondition that made #728 miss it: the failed run HAS an admitted handoff.
	tx, err := fixture.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	run, _ := storedRun(t, fixture.phase8Fixture, failing)
	item := projectOrchestrationItem(tx, fixture.stateDir, orchestration.BatchItem{Issue: graphUnitC, RunID: failing}, fixture.clock.Now())
	_ = tx.Rollback()
	if item.State != orchestration.ItemFailed || item.HandoffID == "" || run.Reason != ReasonAssuranceBudgetExhausted {
		t.Fatalf("not the #693 shape: state=%s handoff=%q reason=%s", item.State, item.HandoffID, run.Reason)
	}
	if request.Subject != nil || request.Source.RunID != failing {
		t.Fatalf("the request is about the failed RUN, not the old handoff: subject=%+v source=%+v", request.Subject, request.Source)
	}
	// The old handoff does not release the dependent.
	if d := fixture.graph(view.GraphID).unit(t, "d"); d.State != orchestration.UnitBlocked || d.RunID != "" {
		t.Fatalf("d is %s on %q: a failed run's handoff satisfied a dependency", d.State, d.RunID)
	}

	g := &failedProducerFixture{fixture: fixture, supervisor: supervisor, graphID: view.GraphID, failedRun: failing, request: *request}
	g.resolve(t, orchestration.EpochAuthorizationApprove)
	for range 80 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if fixture.graph(view.GraphID).unit(t, "d").State == orchestration.UnitState(orchestration.ItemCompleted) {
			break
		}
	}
	settled := fixture.graph(view.GraphID)
	c, d := settled.unit(t, "c"), settled.unit(t, "d")
	if c.RunID == failing || c.State != orchestration.UnitState(orchestration.ItemCompleted) || d.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("after approval c=%s on %s, d=%s", c.State, c.RunID, d.State)
	}
	if prior, ok := storedRun(t, fixture.phase8Fixture, failing); !ok || prior.Disposition != Failed || prior.Reason != ReasonAssuranceBudgetExhausted {
		t.Fatalf("the failed run was rewritten: %+v", prior)
	}
}
