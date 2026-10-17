package main

// Product-scoped objective intake correctness (#471/#479/#611 hardening):
// RequireProductIfChosen (prevalidation) and EnsureProductGraphAssociation
// (retry recovery) driven through the REAL revisePlan dispatch - the exact
// function the browser's /v1/objectives handler calls - not a unit test of
// AssociateProductGraph in isolation. Deterministic planning means no real
// provider is ever invoked, exactly as TestEveryPlanProposalBindsAWorkGraph's
// own propose path.

import (
	"context"
	"testing"
	"time"

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
	plans, err := built.store.Plans()
	if err != nil {
		t.Fatal(err)
	}
	graphs, err := built.store.WorkGraphs()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 || len(graphs) != 0 {
		t.Fatalf("unknown product wrote state before being refused: %d plans, %d graphs", len(plans), len(graphs))
	}
}

// 2. Product exists but does not contain the graph's repository: refused by
// the authoritative AssociateWorkGraph boundary, and it stays refused on a
// retry of the same objective (not a one-time, inconsistent failure).
func TestObjectiveIntakeRefusesProductThatDoesNotOwnTheRepository(t *testing.T) {
	built, supervisor, _ := objectiveIntakeFixture(t, 41)
	wrongRepo := adoptTestProduct(t, built, "unrelated-product", []string{"acme/other-repo"})
	request := objectiveRequest(41, wrongRepo)
	if _, err := built.revisePlan(context.Background(), supervisor, request); err == nil {
		t.Fatal("a product that does not own the objective's repository should be refused")
	}
	// The plan/graph DO exist past this point (prevalidation only checked the
	// product existed, not its repositories) - but no association does.
	if owner, found, err := built.store.AssociatedProduct(firstGraphID(t, built)); err != nil || found {
		t.Fatalf("a repository-mismatched product must not end up associated: owner=%q found=%t err=%v", owner, found, err)
	}
	// Retrying the SAME objective fails the same way, deterministically -
	// never succeeds on a later attempt with nothing having changed.
	if _, err := built.revisePlan(context.Background(), supervisor, request); err == nil {
		t.Fatal("retrying with the same repository-mismatched product should still be refused")
	}
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
// association rather than report success while it is still absent.
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
