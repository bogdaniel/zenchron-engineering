package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// failedProducerFixture drives the real supervisor until a producer whose run
// FAILED before any handoff has a live epoch_authorization request, and
// resolves nothing.
type failedProducerFixture struct {
	fixture    *fleetFixture
	supervisor *Supervisor
	graphID    string
	failedRun  string
	request    orchestration.EngineeringMessage
}

func driveToFailedProducerRequest(t *testing.T) *failedProducerFixture {
	t.Helper()
	fixture := newFleetFixture(t, 4)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	failing := issueRunOf(t, fixture, graphUnitC)
	fixture.worker.set(failing, fleetFails)
	for range 60 {
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
				return &failedProducerFixture{fixture: fixture, supervisor: supervisor, graphID: view.GraphID, failedRun: failing, request: message}
			}
		}
	}
	t.Fatal("a producer that failed before any handoff never got an epoch_authorization request")
	return nil
}

func (g *failedProducerFixture) resolve(t *testing.T, value string) {
	t.Helper()
	if _, err := g.supervisor.ResolveDecision(ControlRequest{
		DecisionID: g.request.ID, Operator: "operator@example.com",
		DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: value,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedProducerWithNoHandoffRaisesAGovernedEpochRequest(t *testing.T) {
	g := driveToFailedProducerRequest(t)
	if !strings.HasPrefix(g.request.Body, "failed-run:"+g.failedRun) {
		t.Fatalf("the request does not name the failed run: %q", g.request.Body)
	}
	if g.request.Subject != nil {
		t.Fatalf("a request about a run with no handoff is bound to a subject: %+v", g.request.Subject)
	}
	// Raising the question changes nothing: the unit stays failed, its
	// dependent stays blocked, and no epoch or authorization exists.
	view := g.fixture.graph(g.graphID)
	if c := view.unit(t, "c"); c.State != orchestration.UnitState(orchestration.ItemFailed) || c.RunID != g.failedRun {
		t.Fatalf("c is %s on %s before any decision", c.State, c.RunID)
	}
	if d := view.unit(t, "d"); d.State != orchestration.UnitBlocked || d.RunID != "" {
		t.Fatalf("d is %s on %q before any decision", d.State, d.RunID)
	}
	epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c")
	if err != nil || len(epochs) != 0 {
		t.Fatalf("epochs before a decision = %+v (%v)", epochs, err)
	}
}

func TestAnApprovedFailedProducerEpochRunsAgainAndUnblocksItsDependents(t *testing.T) {
	g := driveToFailedProducerRequest(t)
	g.resolve(t, orchestration.EpochAuthorizationApprove)
	// driveGraph would return at once: a failed unit and its blocked dependent
	// already count as settled.
	var settled WorkGraphView
	for range 80 {
		if _, err := g.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		g.fixture.clock.advance(61 * time.Second)
		if settled = g.fixture.graph(g.graphID); settled.unit(t, "d").State == orchestration.UnitState(orchestration.ItemCompleted) {
			break
		}
	}
	c, d := settled.unit(t, "c"), settled.unit(t, "d")
	if c.RunID == g.failedRun {
		t.Fatal("the unit still names the failed run after an approved epoch")
	}
	if c.State != orchestration.UnitState(orchestration.ItemCompleted) || d.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("after the approved epoch c=%s d=%s: %s", c.State, d.State, d.Reason)
	}
	epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c")
	if err != nil || len(epochs) != 1 || epochs[0].Epoch != 2 || epochs[0].RunID != c.RunID {
		t.Fatalf("epochs = %+v (%v), want exactly the epoch-2 run %s", epochs, err, c.RunID)
	}
	authorizations, err := g.fixture.store.RemediationAuthorizationsFor(g.graphID, "c")
	if err != nil || len(authorizations) != 1 || authorizations[0].AuthorizedBy != RemediationAuthorizedByDecision ||
		authorizations[0].FindingID != g.request.Body {
		t.Fatalf("authorizations = %+v (%v)", authorizations, err)
	}
	// The failed run is history, not rewritten.
	if run, ok := storedRun(t, g.fixture.phase8Fixture, g.failedRun); !ok || run.Disposition != Failed {
		t.Fatalf("the failed run is %+v", run)
	}
}

func TestARejectedFailedProducerEpochGrantsNothing(t *testing.T) {
	g := driveToFailedProducerRequest(t)
	g.resolve(t, orchestration.EpochAuthorizationReject)
	for range 20 {
		if _, err := g.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		g.fixture.clock.advance(61 * time.Second)
	}
	if epochs, err := g.fixture.store.WorkUnitRemediationEpochs(g.graphID, "c"); err != nil || len(epochs) != 0 {
		t.Fatalf("a rejected request produced epochs %+v (%v)", epochs, err)
	}
	if authorizations, err := g.fixture.store.RemediationAuthorizationsFor(g.graphID, "c"); err != nil || len(authorizations) != 0 {
		t.Fatalf("a rejected request produced authorizations %+v (%v)", authorizations, err)
	}
	view := g.fixture.graph(g.graphID)
	if c := view.unit(t, "c"); c.RunID != g.failedRun || c.State != orchestration.UnitState(orchestration.ItemFailed) {
		t.Fatalf("c moved after a rejection: %s on %s", c.State, c.RunID)
	}
}

func TestAFailedRunReferenceNamesTheHeldCandidateOnlyWhenThereIsOne(t *testing.T) {
	if got := failedRunReference("run-1", RunProjection{}); got != "failed-run:run-1" {
		t.Fatalf("reference without a candidate = %q", got)
	}
	held := RunProjection{CandidateRevision: "f257c60d", CandidateTree: "43144ce4"}
	if got := failedRunReference("run-1", held); got != "failed-run:run-1 candidate=f257c60d tree=43144ce4" {
		t.Fatalf("reference with a candidate = %q", got)
	}
}

// #693: the projection producer's remediation attempt reported "completed", had
// its handoff admitted, and then ran out of assurance budget. The run is failed
// WITH a handoff, and the question still has to be raised.
func TestAFailedProducerNeedsAnEpochRequestWhetherOrNotAHandoffWasAdmitted(t *testing.T) {
	cases := []struct {
		name string
		item OrchestrationItemView
		want bool
	}{
		{"failed, no handoff", OrchestrationItemView{State: orchestration.ItemFailed}, true},
		{"failed after its handoff was admitted", OrchestrationItemView{State: orchestration.ItemFailed, HandoffID: "handoff-1"}, true},
		{"failed but still executing", OrchestrationItemView{State: orchestration.ItemFailed, Executing: true}, false},
		{"completed", OrchestrationItemView{State: orchestration.ItemCompleted, HandoffID: "handoff-1"}, false},
		{"stopped", OrchestrationItemView{State: orchestration.ItemStopped}, false},
	}
	for _, c := range cases {
		if got := failedProducerNeedsEpochRequest(c.item); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
