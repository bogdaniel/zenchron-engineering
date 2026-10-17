package main

// Product-scoped objective intake correctness (#471/#479/#611 hardening):
// RequireProductIfChosen (prevalidation, including repository membership)
// and revisePlan's resend path (which replays the canonical, idempotent
// BindObjectivePlan/AssociateProductGraph rather than a bespoke recovery
// function) driven through the REAL revisePlan dispatch - the exact
// function the browser's /v1/objectives handler calls - not a unit test of
// AssociateProductGraph in isolation. Deterministic planning means no real
// provider is ever invoked, exactly as TestEveryPlanProposalBindsAWorkGraph's
// own propose path.

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func objectiveIntakeFixture(t *testing.T, issue int) (*composition, *runtime.Supervisor, *runtime.FakeGitHubAdapter) {
	t.Helper()
	dir, configPath := planWorkspaceIn(t, t.TempDir())
	t.Chdir(dir)
	forge := planForge(t, issue)
	built, err := newComposition(autonomyFlags{Config: configPath}, autonomyOverrides{
		GitHub: forge, Provider: &refusingPlanner{}, ControllerBuild: &runtime.ControllerBuild{Kind: runtime.ControllerUnattested},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	supervisor, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return built, supervisor, forge
}

func approveTestPlan(t *testing.T, plans runtime.PlanService, planID string, revision int, digest string) {
	t.Helper()
	view, err := plans.ViewRevision(planID, revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plans.Approve(planID, revision, digest, view.AssignmentsDigest, "operator", "test approval"); err != nil {
		t.Fatal(err)
	}
}

func adoptTestProduct(t *testing.T, built *composition, name string, repositories []string) string {
	t.Helper()
	id, err := product.ProductID(name)
	if err != nil {
		t.Fatal(err)
	}
	p := product.Product{SchemaVersion: product.ProductSchemaVersion, ID: id, Name: name, Revision: 1,
		Repositories: repositories, CreatedAt: time.Now().UTC()}
	if _, _, err := built.store.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	return id
}

func objectiveRequest(issue int, productID string) runtime.ControlRequest {
	return runtime.ControlRequest{
		Command: runtime.ControlPlanRevise, Repository: "zenchron/seeded", Issue: issue,
		Deterministic: true, ProductID: productID, ExpectedController: "test-controller-binding",
	}
}

// 1. Unknown product submitted through objective intake: refused before any
// provider invocation or plan/graph write, not merely before association.
func TestObjectiveIntakeRefusesUnknownProductBeforeWritingAnything(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, "product-never-adopted")); err == nil {
		t.Fatal("an unknown product should refuse the objective")
	}
	assertNoPlanOrGraphWritten(t, built, "unknown product")
}

func assertNoPlanOrGraphWritten(t *testing.T, built *composition, label string) {
	t.Helper()
	plans, err := built.store.Plans()
	if err != nil {
		t.Fatal(err)
	}
	graphs, err := built.store.WorkGraphs()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 || len(graphs) != 0 {
		t.Fatalf("%s wrote state before being refused: %d plans, %d graphs", label, len(plans), len(graphs))
	}
}

// 2. Product exists but does not contain the graph's repository: refused
// before any plan or graph write at all (prevalidation now checks
// repository membership, not merely product existence), and it stays
// refused on a retry of the same objective - not a one-time, inconsistent
// failure, and never a later success with nothing having changed.
func TestObjectiveIntakeRefusesProductThatDoesNotOwnTheRepository(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	wrongRepo := adoptTestProduct(t, built, "unrelated-product", []string{"acme/other-repo"})
	request := objectiveRequest(41, wrongRepo)
	if _, err := built.revisePlan(context.Background(), supervisor, request); err == nil {
		t.Fatal("a product that does not own the objective's repository should be refused")
	}
	assertNoPlanOrGraphWritten(t, built, "repository-mismatched product")
	// Retrying the SAME objective fails the same way, deterministically,
	// and still writes nothing.
	if _, err := built.revisePlan(context.Background(), supervisor, request); err == nil {
		t.Fatal("retrying with the same repository-mismatched product should still be refused")
	}
	assertNoPlanOrGraphWritten(t, built, "retried repository-mismatched product")
}

// 3. Conflicting product ownership: a graph already associated with one
// product cannot be silently reassigned to another by a later resend of the
// same objective naming a different product.
func TestObjectiveIntakeRefusesAConflictingProductOnRetry(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	productB := adoptTestProduct(t, built, "checkout", []string{"zenchron/seeded"})
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA)); err != nil {
		t.Fatal(err)
	}
	graphID := firstGraphID(t, built)
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productB)); err == nil {
		t.Fatal("a resend naming a different, already-conflicting product should be refused")
	}
	owner, found, err := built.store.AssociatedProduct(graphID)
	if err != nil || !found || owner != productA {
		t.Fatalf("ownership must not move: owner=%q found=%t err=%v, want %q", owner, found, err, productA)
	}
}

// 3b. A FIRST-TIME product association is refused once the objective has
// been approved - not merely once a stage has actually started. Product
// association determines which product's configuration and knowledge
// context a WorkGraph's units may read; attaching that after approval
// would change the meaning of work that was approved without it. Nothing
// is partially recorded by the refusal.
func TestObjectiveIntakeRefusesFirstTimeProductAssociationAfterApproval(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	view, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, ""))
	if err != nil {
		t.Fatal(err)
	}
	approveTestPlan(t, plans, view.Plan.ID, view.Plan.Revision, view.Plan.Digest)

	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA)); err == nil {
		t.Fatal("a first-time product association after approval should be refused")
	}
	if _, found, err := built.store.AssociatedProduct(view.WorkGraphID); err != nil || found {
		t.Fatalf("a refused association must not be partially recorded: found=%t err=%v", found, err)
	}
}

// 3c. A resend naming the product a graph is ALREADY associated with stays
// idempotent even after approval - this is ordinary resend territory, not
// a new assignment, and must not be confused with 3b's first-time case.
func TestObjectiveIntakeAllowsSameProductResendAfterApproval(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	view, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA))
	if err != nil {
		t.Fatal(err)
	}
	approveTestPlan(t, plans, view.Plan.ID, view.Plan.Revision, view.Plan.Digest)

	resent, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA))
	if err != nil {
		t.Fatalf("a resend naming the already-associated product after approval should stay idempotent: %v", err)
	}
	if resent.WorkGraphID != view.WorkGraphID {
		t.Fatalf("resend changed the graph: got %s, want %s", resent.WorkGraphID, view.WorkGraphID)
	}
	owner, found, err := built.store.AssociatedProduct(view.WorkGraphID)
	if err != nil || !found || owner != productA {
		t.Fatalf("association must remain: owner=%q found=%t err=%v, want %q", owner, found, err, productA)
	}
}

// 4. A valid association survives the store reopening - a restart, through a
// second independent handle on the same SQLite file.
func TestObjectiveIntakeAssociationSurvivesStoreReopening(t *testing.T) {
	stateDir := t.TempDir()
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	supervisor, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA)); err != nil {
		t.Fatal(err)
	}
	graphID := firstGraphID(t, built)

	restarted, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	owner, found, err := restarted.AssociatedProduct(graphID)
	if err != nil || !found || owner != productA {
		t.Fatalf("association after reopening = %q (found=%t err=%v), want %q", owner, found, err, productA)
	}
}

// 5. An earlier attempt's product association did not complete (whatever the
// cause - a crash, a transient failure, or simply not having been asked for
// yet): the plan and graph already exist with no association. A retry of
// the SAME objective, now naming the product, must complete the missing
// association rather than report success while it is still absent. This
// replays BindObjectivePlan (idempotent - the graph already exists) then
// AssociateProductGraph; see test 8 below for the graph ALSO being missing.
func TestObjectiveIntakeRecoversAMissingAssociationOnRetry(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	// First attempt: no product chosen. Plan and graph are written; nothing
	// is associated - the exact durable shape a failed/incomplete first
	// association attempt would also leave behind.
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, "")); err != nil {
		t.Fatal(err)
	}
	graphID := firstGraphID(t, built)
	if _, found, err := built.store.AssociatedProduct(graphID); err != nil || found {
		t.Fatalf("precondition: graph must start unassociated, found=%t err=%v", found, err)
	}
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})

	// Retry: the SAME objective (no explicit PlanID - an ordinary resend),
	// now naming the product. This must take the dedup path (the plan
	// already exists) and still complete the association, not report
	// success while silently leaving it missing.
	view, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA))
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkGraphID != graphID {
		t.Fatalf("retry produced a different graph: %s, want %s", view.WorkGraphID, graphID)
	}
	owner, found, err := built.store.AssociatedProduct(graphID)
	if err != nil || !found || owner != productA {
		t.Fatalf("recovered association = %q (found=%t err=%v), want %q", owner, found, err, productA)
	}
}

// 6. Two products sharing a repository, each owning a different objective's
// graph in the SAME store: ownership never crosses.
func TestObjectiveIntakeTwoProductsSharingARepositoryStayIsolated(t *testing.T) {
	built, supervisor, forge := objectiveIntakeFixture(t, 41)
	forge.Issues[42] = runtime.GitHubIssue{
		Number: 42, URL: "https://github.com/zenchron/seeded/issues/42",
		Title: "a second, independent objective", Body: "Independent of issue 41.",
		State: runtime.GitHubOpen, UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Author: runtime.GitHubActor{Login: "operator", ID: 7},
	}
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})
	productB := adoptTestProduct(t, built, "checkout", []string{"zenchron/seeded"})
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA)); err != nil {
		t.Fatal(err)
	}
	graphA := firstGraphID(t, built)
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(42, productB)); err != nil {
		t.Fatal(err)
	}
	graphs, err := built.store.WorkGraphs()
	if err != nil {
		t.Fatal(err)
	}
	var graphB string
	for _, g := range graphs {
		if g.ID != graphA {
			graphB = g.ID
		}
	}
	if graphB == "" {
		t.Fatal("second objective never compiled its own graph")
	}
	ownerA, foundA, err := built.store.AssociatedProduct(graphA)
	if err != nil || !foundA || ownerA != productA {
		t.Fatalf("graph A owner = %q (found=%t err=%v), want %q", ownerA, foundA, err, productA)
	}
	ownerB, foundB, err := built.store.AssociatedProduct(graphB)
	if err != nil || !foundB || ownerB != productB {
		t.Fatalf("graph B owner = %q (found=%t err=%v), want %q", ownerB, foundB, err, productB)
	}
}

// 7. Existing unscoped objective intake (no product chosen) continues to
// work exactly as before this change: propose succeeds, and no association
// is an error, not a silently-invented one.
func TestObjectiveIntakeWithNoProductStillWorks(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	view, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, ""))
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkGraphID == "" {
		t.Fatal("an unscoped objective must still compile its WorkGraph")
	}
	if _, found, err := built.store.AssociatedProduct(view.WorkGraphID); err != nil || found {
		t.Fatalf("an unscoped objective must have no product association: found=%t err=%v", found, err)
	}
}

// 8. A plan can exist with NO objective graph at all - a crash between
// Propose and BindObjectivePlan, the ONE write-ordering the public propose
// paths never expose (they always bind together). A retry must not report
// success while silently leaving the objective ungraphed: the plan
// reconciler's legacy fallback starts a flat, un-graphed run the moment it
// finds no objective graph (objective_workgraph.go's startObjectiveStage),
// which for a product-scoped objective would mean unbound work having
// started outside the WorkGraph/product machinery entirely. Recovery must
// actually compile and bind the missing graph, not merely report the plan.
func TestObjectiveIntakeRecoversAPlanWithNoGraphOnRetry(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})

	planID := proposePlanOnly(t, built, supervisor, 41)
	plans, err := built.store.Plans()
	if err != nil {
		t.Fatal(err)
	}
	graphs, err := built.store.WorkGraphs()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(graphs) != 0 {
		t.Fatalf("precondition: want exactly one plan and zero graphs, got %d plans, %d graphs", len(plans), len(graphs))
	}

	view, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA))
	if err != nil {
		t.Fatal(err)
	}
	if view.Plan.ID != planID {
		t.Fatalf("recovery produced a different plan: %s, want %s", view.Plan.ID, planID)
	}
	if view.WorkGraphID == "" {
		t.Fatal("recovery must actually compile and bind the missing graph, not report success without one")
	}
	if graphs, err := built.store.WorkGraphs(); err != nil || len(graphs) != 1 {
		t.Fatalf("recovery must bind exactly one graph: %d graphs, err=%v", len(graphs), err)
	}
	owner, found, err := built.store.AssociatedProduct(view.WorkGraphID)
	if err != nil || !found || owner != productA {
		t.Fatalf("recovered graph's product association = %q (found=%t err=%v), want %q", owner, found, err, productA)
	}
}

// proposePlanOnly writes a plan through the real deterministic propose path
// but deliberately stops BEFORE BindObjectivePlan - modeling a crash between
// the two writes, a state the public revisePlan/CLI entry points never
// produce on their own (they always bind together).
func proposePlanOnly(t *testing.T, built *composition, supervisor *runtime.Supervisor, issue int) string {
	t.Helper()
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	repository, _, defaultBranch, err := built.planSubject(supervisor, objectiveRequest(issue, ""))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := runtime.ParseGitHubRepo(repository)
	if err != nil {
		t.Fatal(err)
	}
	target := runtime.RepositoryTarget{Identity: repo.String(), Remote: repo.CloneURL(), DefaultBranch: defaultBranch}
	engine, err := built.engineFor(target, built.agent)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	intent, err := engine.CompilePlanIntent(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	planID, err := engine.PlanID(issue)
	if err != nil {
		t.Fatal(err)
	}
	input := runtime.ProposeInput{
		PlanID: planID, Objective: intent.Objective, Subject: intent.Subject,
		Repository: target.Identity, Contract: intent.Contract, Model: intent.Model, Facts: intent.Facts,
		Issue: issue, ObservedBase: intent.Base.Revision,
	}
	if _, err := plans.Propose(ctx, input); err != nil {
		t.Fatal(err)
	}
	return planID
}

// 9. Recovery is serialized with the plan reconciler under the SAME plan
// lock a new proposal's write uses - a reconciler tick must never be able
// to interleave with the recovery read/bind/associate sequence. Proved by
// holding the lock in one goroutine and confirming the resend path
// genuinely blocks on it rather than running unlocked. See
// runtime/objective_workgraph_test.go's
// TestBindObjectivePlanRefusesAnIdempotentReplayOnceAStageHasStarted for the
// companion proof: why resumeObjectiveGraph must skip BindObjectivePlan
// entirely once a matching graph already exists, rather than ever calling
// it on a resend regardless of execution state.
func TestObjectiveIntakeResendIsSerializedWithThePlanLock(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, "")); err != nil {
		t.Fatal(err)
	}
	productA := adoptTestProduct(t, built, "storefront", []string{"zenchron/seeded"})

	entered := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- supervisor.WithPlanLock(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := built.revisePlan(context.Background(), supervisor, objectiveRequest(41, productA)); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-done:
		t.Fatal("resend completed while the plan lock was held elsewhere - it is not serialized with the reconciler")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resend never completed after the plan lock was released")
	}
}

// adoptObjectiveGraphFixture adopts a real WorkGraph whose ObjectivePlan
// reference is exactly the given plan identity - the one thing
// orchestration.WorkGraphProposal.Compose (a flat, non-objective graph) never
// sets, and exactly what resumeObjectiveGraph's revision/digest match checks.
func adoptObjectiveGraphFixture(t *testing.T, built *composition, name, repository, agentID, planID string, revision int, digest string) orchestration.WorkGraph {
	t.Helper()
	id, err := orchestration.WorkGraphID(repository, agentID, name)
	if err != nil {
		t.Fatal(err)
	}
	graph := orchestration.WorkGraph{SchemaVersion: orchestration.WorkGraphSchemaVersion, ID: id, Name: name,
		Repository: repository, AgentID: agentID, Revision: 1, CreatedAt: time.Now().UTC(),
		ObjectivePlan: &domain.PlanRef{ID: planID, Revision: revision, Digest: digest},
		Units:         []orchestration.WorkUnit{{ID: "a", Purpose: "do the work", Role: "implementer", Issue: 41}}}
	if err := graph.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := built.store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

// TestResumeObjectiveGraphReusesAnExistingGraphWithoutRebuilding is the
// direct, low-cost proof companion to
// TestBindObjectivePlanRefusesAnIdempotentReplayOnceAStageHasStarted
// (runtime/objective_workgraph_test.go): resumeObjectiveGraph must return an
// already-bound, exactly-matching graph id directly, never calling
// BindObjectivePlan at all. The PlanView here names a plan id that was never
// actually proposed to the plan store - BindObjectivePlan would error
// immediately if resumeObjectiveGraph called it despite the graph already
// matching, which is exactly how this catches a regression back to "always
// rebuild".
func TestResumeObjectiveGraphReusesAnExistingGraphWithoutRebuilding(t *testing.T) {
	built, _, _ := objectiveIntakeFixture(t, 41)
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	graph := adoptObjectiveGraphFixture(t, built, "objective-plan-never-proposed", "zenchron/seeded", "codex", "plan-never-proposed", 1, "digest-abc")
	fresh := runtime.PlanView{WorkGraphID: graph.ID, Plan: domain.EngineeringPlan{ID: "plan-never-proposed", Revision: 1, Digest: "digest-abc"}}
	graphID, err := built.resumeObjectiveGraph(plans, fresh, 41, autonomyFlags{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if graphID != graph.ID {
		t.Fatalf("resumeObjectiveGraph rebuilt instead of reusing: got %s, want %s", graphID, graph.ID)
	}
}

// TestResumeObjectiveGraphRefusesAConflictingRequestedAgent proves the one
// check preserved alongside "reuse, don't rebuild": a resend naming a
// DIFFERENT agent than the one the existing graph was actually bound under
// is refused, not silently ignored - the same way a resend naming a
// different product is refused rather than silently reassigned.
func TestResumeObjectiveGraphRefusesAConflictingRequestedAgent(t *testing.T) {
	built, _, _ := objectiveIntakeFixture(t, 41)
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	graph := adoptObjectiveGraphFixture(t, built, "objective-plan-never-proposed", "zenchron/seeded", "codex", "plan-never-proposed", 1, "digest-abc")
	fresh := runtime.PlanView{WorkGraphID: graph.ID, Plan: domain.EngineeringPlan{ID: "plan-never-proposed", Revision: 1, Digest: "digest-abc"}}

	if _, err := built.resumeObjectiveGraph(plans, fresh, 41, autonomyFlags{}, "claude"); err == nil {
		t.Fatal("a resend naming a different agent than the graph's own should be refused")
	}
	graphID, err := built.resumeObjectiveGraph(plans, fresh, 41, autonomyFlags{}, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if graphID != graph.ID {
		t.Fatalf("matching agent should reuse the graph: got %s, want %s", graphID, graph.ID)
	}
	graphID, err = built.resumeObjectiveGraph(plans, fresh, 41, autonomyFlags{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if graphID != graph.ID {
		t.Fatalf("no requested agent should reuse the graph without checking: got %s, want %s", graphID, graph.ID)
	}
}

// TestResumeObjectiveGraphNeverReusesAGraphBoundToADifferentRevision proves
// the exact-binding check: objectiveGraphFor (which computed WorkGraphID)
// matches by plan ID alone, never revision, so a graph bound to a DIFFERENT
// revision/digest than the one actually being represented must never be
// silently reused as-is - it must fall through to BindObjectivePlan
// instead, which correctly refuses a plan revision this synthetic test
// never actually proposed. The call failing (rather than succeeding with
// the stale id) is itself the proof that blind reuse was skipped.
func TestResumeObjectiveGraphNeverReusesAGraphBoundToADifferentRevision(t *testing.T) {
	built, _, _ := objectiveIntakeFixture(t, 41)
	plans, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	graph := adoptObjectiveGraphFixture(t, built, "objective-plan-x", "zenchron/seeded", "codex", "plan-x", 1, "digest-rev1")
	// fresh represents revision 2; the graph is still bound to revision 1 -
	// the exact shape an explicit revision's bind that crashed before
	// completing would leave behind.
	fresh := runtime.PlanView{WorkGraphID: graph.ID, Plan: domain.EngineeringPlan{ID: "plan-x", Revision: 2, Digest: "digest-rev2"}}

	if _, err := built.resumeObjectiveGraph(plans, fresh, 41, autonomyFlags{}, ""); err == nil {
		t.Fatal("a graph bound to a stale revision must never be silently reused")
	}
}

func firstGraphID(t *testing.T, built *composition) string {
	t.Helper()
	graphs, err := built.store.WorkGraphs()
	if err != nil {
		t.Fatal(err)
	}
	if len(graphs) != 1 {
		t.Fatalf("expected exactly one compiled graph, got %d", len(graphs))
	}
	return graphs[0].ID
}
