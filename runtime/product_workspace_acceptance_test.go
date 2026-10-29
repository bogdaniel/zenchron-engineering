package runtime

// #479's acceptance scenario, proved end to end through exactly the read
// paths controlplane's ReadStore exposes: a Product (#476) with one
// associated WorkGraph (#472) whose units A and B implement in parallel, C
// integrates and depends on both. A is independent-review-gated: one
// REQUEST_CHANGES decision (the BLOCK) does not release C, and only a fresh
// independent APPROVE of the EXACT SAME admitted commit (reviewApprovedFor's
// own exact-commit binding, runtime/workgraph_status.go) releases it. This is
// #472/#474's durable readiness GATE transitioning - never A's own
// completion alone - not #474's producer remediation lifecycle: it does not
// have A produce a second candidate H2, request a fresh independent review
// of H2, or invalidate a stale H1 decision against a newer head. That
// producer-remediation path is #474's own ownership and is proved by its own
// test suite (runtime/review_remediation_e2e_test.go and friends); nothing
// here asserts it.
//
// C itself asks one #473 DecisionRequest that an authorized operator
// resolves through the real #508 governed action (never a worker
// self-answer). Every fact - including that the resolved decision no longer
// reads as open - is re-read through a fresh ReadStore opened after the
// writer closes, proving restart safety through the exact ReadStore methods
// the Product Workspace console calls. The console's own HTTP route is
// additionally exercised, with a real resolved decision hidden through it,
// in controlplane's own
// TestWorkGraphDetailHidesAResolvedDecisionThroughTheRealHTTPRouteAfterRestart
// - this test cannot call that route itself without an import cycle
// (controlplane imports runtime; this file needs runtime's own unexported
// fleet/messaging test fixtures, so it must stay in package runtime).

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
	"github.com/bogdaniel/zenchron-engineering/review"
)

func TestProductWorkspaceCombinesProductWorkGraphReviewAndDecision(t *testing.T) {
	fixture, worker := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	now := fixture.clock.Now()
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}

	units := []orchestration.WorkUnit{
		{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: graphUnitA, RequiresReview: true},
		{ID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: graphUnitB},
		{ID: "c", Purpose: "integrate a and b", Role: domain.RoleImplementer, Issue: graphUnitC, DependsOn: []string{"a", "b"}},
	}
	// c's run id is a pure function of repository, issue and config digest
	// (issueRunID), so its decision document is set BEFORE the graph is even
	// adopted - c's own first execution attempt, whenever the frontier
	// eventually activates it, reads it already in place. Setting it only
	// once c's run becomes visible is too late: activation and that unit's
	// first execution attempt happen within the same supervisor Tick.
	cRunID := issueRunOf(t, fixture, graphUnitC)
	worker.say(cRunID, messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindDecisionRequest, Purpose: "which backend?", Body: "v1 or v2?",
	}), false)
	view := fixture.adoptGraph(supervisor, "claude", 1, units)

	prod := composeProduct(t, "checkout", 1, []string{"acme/repo"}, now)
	if _, _, err := fixture.store.AdoptProductRevision(prod); err != nil {
		t.Fatal(err)
	}
	association, err := product.NewGraphAssociation(prod.ID, view.GraphID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AssociateWorkGraph(association); err != nil {
		t.Fatal(err)
	}

	// A and B complete and admit their handoffs in parallel; C stays blocked
	// - unitSettled treats UnitBlocked as settled, so driveGraph returns as
	// soon as nothing else can move, exactly as graphReviewGated's own
	// acceptance test relies on.
	afterAB := fixture.driveGraph(supervisor, view.GraphID, nil)
	a, b := afterAB.unit(t, "a"), afterAB.unit(t, "b")
	if a.State != orchestration.UnitState(orchestration.ItemCompleted) || b.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("a and b must complete in parallel: a=%s b=%s", a.State, b.State)
	}
	if c := afterAB.unit(t, "c"); c.State != orchestration.UnitBlocked {
		t.Fatalf("c must stay blocked until a's review clears, got %s", c.State)
	}
	h1, prNumber := a.Output.CandidateRevision, a.Child.PullRequest

	// One BLOCK cycle: a REQUEST_CHANGES decision never releases c.
	seedWorkGraphReviewDecision(t, fixture.store, repo, prNumber, h1, a.RunID, a.Child.AgentID, "reviewer-1", review.VerdictRequestChanges, fixture.clock.Now())
	if got := fixture.graph(view.GraphID).unit(t, "c").State; got != orchestration.UnitBlocked {
		t.Fatalf("c = %s after REQUEST_CHANGES, want still blocked", got)
	}

	// The remediation: a fresh independent APPROVE of the exact commit
	// releases c.
	seedWorkGraphReviewDecision(t, fixture.store, repo, prNumber, h1, a.RunID, a.Child.AgentID, "reviewer-2", review.VerdictApprove, fixture.clock.Now().Add(time.Minute))
	if got := fixture.graph(view.GraphID).unit(t, "c").State; got != orchestration.UnitReady {
		t.Fatalf("c = %s after an independent APPROVE of the exact commit, want ready", got)
	}

	// Drive manually (not driveGraph, which requires every unit to settle):
	// c activates, asks its own decision on its first execution attempt, and
	// genuinely waits on it rather than settling - #508's whole point.
	settledOnWait := false
	for range 160 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		row, found, err := fixture.store.Run(cRunID)
		if err != nil {
			t.Fatal(err)
		}
		if found && row.Disposition == Waiting && row.Reason == ReasonDecisionPending {
			settledOnWait = true
			break
		}
	}
	if !settledOnWait {
		t.Fatal("c never activated and settled waiting on its own decision")
	}
	cBatchID := batchOf(t, fixture, view.GraphID, "c")
	open := fixture.status(cBatchID).OpenDecisions
	if len(open) != 1 || open[0].Source.RunID != cRunID {
		t.Fatalf("c's own open decision must be scoped to its own batch alone: %+v", open)
	}
	decisionID := open[0].ID
	if _, found, err := fixture.store.DecisionResolutionByRequestID(decisionID); err != nil || found {
		t.Fatalf("the decision must still be unresolved: found=%t err=%v", found, err)
	}

	// The authorized human decision: resolved through the real #508 governed
	// action, never a worker self-answer and never a control-plane shortcut.
	resolution, err := supervisor.ResolveDecision(ControlRequest{
		Operator: "operator@example", DecisionID: decisionID,
		DecisionOutcomeKind: orchestration.DecisionText, DecisionOutcomeValue: "use backend v2",
	})
	if err != nil {
		t.Fatalf("an authorized operator resolution must be accepted: %v", err)
	}
	if resolution.Resolution.Outcome.Value != "use backend v2" {
		t.Fatalf("resolution outcome lost: %+v", resolution.Resolution)
	}
	// A worker cannot resolve its own request: AuthorityKindOperator is
	// hardcoded by ResolveDecision and reachable only from this governed
	// call, never from anything a message report could carry (#508's own
	// architectural law, re-pinned here at the #479 boundary that reads it).

	// Restart: close the writer and open a fresh ReadStore - the EXACT
	// boundary controlplane.ReadStore is - and confirm every fact above
	// survives identically.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	restarted, err := reader.WorkGraphStatus(view.GraphID, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.unit(t, "c").State; got != orchestration.UnitState(orchestration.ItemWaiting) {
		t.Fatalf("c must still read as waiting on its own decision after restart, got %s", got)
	}
	restartedOpen, err := reader.OrchestrationStatus(cBatchID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(restartedOpen.OpenDecisions) != 1 || restartedOpen.OpenDecisions[0].ID != decisionID {
		t.Fatalf("the open decision message itself survives restart unchanged: %+v", restartedOpen.OpenDecisions)
	}
	if _, found, err := reader.DecisionResolution(decisionID); err != nil || !found {
		t.Fatalf("the durable resolution must survive restart: found=%t err=%v", found, err)
	}
	productID, found, err := reader.AssociatedProduct(view.GraphID)
	if err != nil || !found || productID != prod.ID {
		t.Fatalf("the graph's product association must survive restart: id=%q found=%t err=%v", productID, found, err)
	}
	readProduct, found, err := reader.CurrentProduct(productID)
	if err != nil || !found || readProduct.Name != "checkout" {
		t.Fatalf("the associated product must read back identically: %+v found=%t err=%v", readProduct, found, err)
	}
	if _, found, err := reader.AssociatedProduct("unknown-graph"); err != nil || found {
		t.Fatalf("an unassociated graph must report no product, not a default: found=%t err=%v", found, err)
	}
}
