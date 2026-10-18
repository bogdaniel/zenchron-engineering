package runtime

// #480 acceptance, refused path: the same dynamic team as the governed
// journey, but the independent reviewer BLOCKS the integrator's candidate. No
// amount of further reconciliation may publish it, and the durable record must
// say that the exact head under review was blocked.

import (
	"context"
	"testing"
	"time"
)

func TestABlockedReviewOfTheIntegratorsCandidateNeverReleasesPublication(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	reviewerProvider := &recordingReviewer{FakeReviewerProvider: NewFakeReviewerProvider(ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "review:unsafe-composition", Detail: "the composed change breaks the contract"}}})}
	worker := &journeyWorker{reviewer: reviewerProvider, asks: map[string]string{}, requests: map[string]ExecutionRequest{}, attempts: map[string]int{}}
	fleet.deps.Provider = worker
	fleet.runtime = fleet.newRuntime(fleet.deps)

	plan := newNamedPlanRunFixture(t, fleet.phase8Fixture, "plan-refused", journeyStages(), fleet.deps.ConfigDigest, defaultPlanFixtureEnvelope)
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	plan.approve(t)
	reviewerDeps := fleet.deps
	reviewerDeps.Provider = reviewerProvider
	reviewerDeps.Agent = ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}
	plan.engines["claude"] = fleet.newRuntime(reviewerDeps)

	supervisor := fleet.supervisor()
	for range 60 {
		plan.reconcile(t)
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
		if _, calls := reviewerProvider.reached(); calls > 0 {
			break
		}
	}
	if _, calls := reviewerProvider.reached(); calls == 0 {
		t.Fatal("the reviewer never reached its worker; the refusal is not exercised")
	}
	integrate := fleet.graph(graphID).unit(t, "integrate")
	if integrate.RunID == "" || integrate.Output == nil {
		t.Fatalf("the integrator never produced a candidate to refuse: %+v", integrate)
	}

	// Keep reconciling long past the verdict: nothing may release the head.
	for range 40 {
		plan.reconcile(t)
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := fleet.runtime.Reconcile(context.Background(), integrate.RunID); err != nil {
			t.Fatal(err)
		}
		fleet.clock.advance(61 * time.Second)
		if state := fleet.state(integrate.RunID); state.projection.PullRequest != nil {
			t.Fatalf("candidate %s was published although its review blocked it", state.projection.CandidateRevision)
		}
	}
	snapshot, err := fleet.store.ReplayPlan(plan.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review := snapshot.Stages["review"].Review; review != nil && review.Verdict == StageReviewAccepted {
		t.Fatalf("a blocking reviewer produced an accepting verdict: %+v", review)
	}
	if final := fleet.state(integrate.RunID); !final.publicationDeferred {
		t.Fatal("publication is not deferred for a candidate whose review blocked it")
	}
}
