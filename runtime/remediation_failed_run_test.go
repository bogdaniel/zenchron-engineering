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


// The #693 production shape: the worker completed and its handoff was admitted,
// but cumulative assurance exhausted the run's wall allowance afterwards.
// A handoff does not override terminal failure or release downstream work.
func TestAFailedAssuranceRunWithAnEarlierAdmittedHandoffRequestsRecovery(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	// Keep the dependent from starting during the producer's initial success;
	// after assurance fails it must remain blocked by the failed producer.
	fixture.hold("d", "test:await-final-assurance", "do not consume an unverified producer")
	supervisor := fixture.supervisor()
	graph := fixture.adoptGraph(supervisor, "claude", 1, graphDiamond())
	producerRunID := issueRunOf(t, fixture, graphUnitC)

	var admittedID string
	for range 80 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		c := fixture.graph(graph.GraphID).unit(t, "c")
		if c.State == orchestration.UnitState(orchestration.ItemCompleted) &&
			c.Child != nil && c.Child.HandoffID != "" {
			admittedID = c.Child.HandoffID
			break
		}
	}
	if admittedID == "" {
		t.Fatal("producer never completed with its admitted worker handoff")
	}
	handoffs, err := fixture.store.RunHandoffs(producerRunID)
	if err != nil || len(handoffs) != 1 || handoffs[0].ID != admittedID {
		t.Fatalf("the initial handoff was not admitted: %+v (%v)", handoffs, err)
	}

	// Exercise the actual terminal lifecycle rather than inventing a new
	// failure classification. The already-admitted handoff is not deleted.
	lifecycleEvent(t, fixture.store, producerRunID, EventRunFailed,
		dispositionRecord{Reason: "run_assurance_budget_exhausted"})
	run, found, err := fixture.store.Run(producerRunID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%t err=%v", found, err)
	}
	run.Disposition, run.Reason = Failed, "run_assurance_budget_exhausted"
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	fixture.release("d")
	c := fixture.graph(graph.GraphID).unit(t, "c")
	d := fixture.graph(graph.GraphID).unit(t, "d")
	if c.State != orchestration.UnitState(orchestration.ItemFailed) ||
		c.Child == nil || c.Child.HandoffID != admittedID {
		t.Fatalf("terminal failure did not dominate the old handoff: %+v", c)
	}
	if d.State != orchestration.UnitBlocked || d.RunID != "" {
		t.Fatalf("dependent consumed a failed producer: %+v", d)
	}

	var request orchestration.EngineeringMessage
	for range 15 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		messages, err := fixture.store.ScopeMessages(run.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind == orchestration.KindDecisionRequest &&
				message.Purpose == orchestration.EpochAuthorizationPurpose {
				request = message
			}
		}
		if request.ID != "" {
			break
		}
	}
	if request.ID == "" {
		t.Fatal("terminal assurance failure after handoff did not request governed recovery")
	}
	if request.Subject != nil || request.Source.RunID != producerRunID ||
		!strings.HasPrefix(request.Body, "failed-run:"+producerRunID) {
		t.Fatalf("request must name the failed run, not authorize its old handoff: %+v", request)
	}
	if authorizations, err := fixture.store.RemediationAuthorizationsFor(graph.GraphID, "c"); err != nil || len(authorizations) != 0 {
		t.Fatalf("request silently granted an epoch: %+v (%v)", authorizations, err)
	}
	if c := fixture.graph(graph.GraphID).unit(t, "c"); c.State != orchestration.UnitState(orchestration.ItemFailed) {
		t.Fatalf("the request changed the failed producer without approval: %+v", c)
	}
}
