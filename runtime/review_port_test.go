package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

func reviewPortFixture(t *testing.T) (*SupervisorReviewPort, *FakeGitHubAdapter, RunIndependentReviewInput) {
	t.Helper()
	in, fake, store := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	provider := reviewStubProvider{document: string(document)}
	port := &SupervisorReviewPort{
		Store: store, GitHub: fake, ResolveAgent: in.ResolveAgent,
		ProviderFor: func(ResolvedAgent) (ExecutionProvider, error) { return provider, nil },
		StateDir:    in.StateDir, Source: in.Source, ControllerID: in.ControllerID, Clock: in.Clock,
	}
	return port, fake, in
}

func TestReviewPortNeedsReviewBeforeAnyDecisionExists(t *testing.T) {
	port, _, in := reviewPortFixture(t)
	needs, err := port.NeedsReview(context.Background(), in.Repo, in.PRNumber)
	if err != nil || !needs {
		t.Fatalf("expected NeedsReview=true before any decision exists, got %v %v", needs, err)
	}
	if _, found, err := port.LatestDecision(in.Repo, in.PRNumber); err != nil || found {
		t.Fatalf("expected no decision yet, found=%v err=%v", found, err)
	}
}

func TestReviewPortRequestReviewThenNoLongerNeedsReview(t *testing.T) {
	port, _, in := reviewPortFixture(t)
	decision, err := port.RequestReview(context.Background(), in.Repo, in.PRNumber, "claude")
	if err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if decision.Verdict != "approve" {
		t.Fatalf("expected an approve decision, got %q", decision.Verdict)
	}
	needs, err := port.NeedsReview(context.Background(), in.Repo, in.PRNumber)
	if err != nil || needs {
		t.Fatalf("expected NeedsReview=false right after a fresh decision, got %v %v", needs, err)
	}
	latest, found, err := port.LatestDecision(in.Repo, in.PRNumber)
	if err != nil || !found || latest.ID != decision.ID {
		t.Fatalf("expected LatestDecision to answer the just-created decision, got %+v found=%v err=%v", latest, found, err)
	}
}

// Mutation check: once the PR head moves past a completed decision, the port
// must report staleness and a fresh review requirement. Removing
// Decision.StaleAgainst's comparison (or IsStale's use of it) must make this
// test fail.
func TestReviewPortDetectsStalenessAfterTheHeadMoves(t *testing.T) {
	port, fake, in := reviewPortFixture(t)
	if _, err := port.RequestReview(context.Background(), in.Repo, in.PRNumber, "claude"); err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	moved := fake.PullRequests[in.PRNumber]
	moved.HeadSHA = testOtherSHA
	fake.PullRequests[in.PRNumber] = moved

	stale, err := port.IsStale(context.Background(), in.Repo, in.PRNumber)
	if err != nil || !stale {
		t.Fatalf("expected IsStale=true after the head moved, got %v %v", stale, err)
	}
	needs, err := port.NeedsReview(context.Background(), in.Repo, in.PRNumber)
	if err != nil || !needs {
		t.Fatalf("expected NeedsReview=true after the head moved, got %v %v", needs, err)
	}
}
