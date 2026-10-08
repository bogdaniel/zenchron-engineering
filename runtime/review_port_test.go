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
	outcome, err := port.RequestReview(context.Background(), in.Repo, in.PRNumber, "claude")
	if err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if outcome.Decision.Verdict != "approve" {
		t.Fatalf("expected an approve decision, got %q", outcome.Decision.Verdict)
	}
	if outcome.Published {
		t.Fatalf("expected Published=false when Publish was not requested, got %+v", outcome)
	}
	needs, err := port.NeedsReview(context.Background(), in.Repo, in.PRNumber)
	if err != nil || needs {
		t.Fatalf("expected NeedsReview=false right after a fresh decision, got %v %v", needs, err)
	}
	latest, found, err := port.LatestDecision(in.Repo, in.PRNumber)
	if err != nil || !found || latest.ID != outcome.Decision.ID {
		t.Fatalf("expected LatestDecision to answer the just-created decision, got %+v found=%v err=%v", latest, found, err)
	}
}

// #233 B5: #474 must be able to read publication state directly off
// RequestReview's outcome, since it is the durable decision - never the
// published GitHub review - that is the authorized, producer-routable
// signal. Removing ReviewOutcome.Published/PublishedVerdict's population in
// SupervisorReviewPort.RequestReview must make this test fail.
func TestReviewPortRequestReviewReportsPublicationState(t *testing.T) {
	port, _, in := reviewPortFixture(t)
	port.Publish = true
	outcome, err := port.RequestReview(context.Background(), in.Repo, in.PRNumber, "claude")
	if err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if !outcome.Published || outcome.PublishedVerdict != outcome.Decision.Verdict {
		t.Fatalf("expected a reported, published outcome, got %+v", outcome)
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
