package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reviewRunFixture builds a real local Git checkout (base commit, then a
// second "PR head" commit), a producing run bound to PR #7 via its own
// github.pr_observed history, and a FakeGitHubAdapter observing that PR -
// everything RunIndependentReview needs, with no network call anywhere.
func reviewRunFixture(t *testing.T) (RunIndependentReviewInput, *FakeGitHubAdapter, *SQLiteOperationStore) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, source, "README.md", "# subject\n")
	baseCommit, _ := commitSource(t, source)
	writeSourceFile(t, source, "feature.go", "package feature\n")
	headCommit, _ := commitSource(t, source)

	store, err := OpenSQLiteOperationStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	run := newJournalRun("run-1")
	run.Repository = testRepo.String()
	run.AgentID = "codex"
	run.Candidate = Candidate{Revision: headCommit, Tree: "ignored"}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(GitHubPRObservedPayload{Number: 7, HeadRevision: headCommit, BaseRevision: baseCommit, State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: newEventID(run.ID), RunID: run.ID,
		Type: EventGitHubPRObserved, OccurredAt: time.Unix(1700000000, 0).UTC(), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, URL: "https://github.com/zenchron/fixture/pull/7", HeadSHA: headCommit, BaseSHA: baseCommit, BaseRef: "main", State: GitHubOpen}
	fake.ChecksByHead[headCommit] = GitHubCheckObservation{State: GitHubCheckSuccess}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}

	agents := map[string]ResolvedAgent{
		"codex":  {ID: "codex", Kind: AgentKindCodexCLI},
		"claude": {ID: "claude", Kind: AgentKindClaudeCode},
	}
	in := RunIndependentReviewInput{
		Repo: testRepo, PRNumber: 7,
		Reviewer: agents["claude"],
		ResolveAgent: func(id string) (ResolvedAgent, error) {
			agent, ok := agents[id]
			if !ok {
				return ResolvedAgent{}, &UnknownAgentError{ID: id}
			}
			return agent, nil
		},
		Store: store, GitHub: fake,
		StateDir: filepath.Join(root, "review-state"), ControllerID: "controller-1",
		Source: source, Clock: fixedClock{time.Unix(1700000100, 0).UTC()},
	}
	return in, fake, store
}

func TestRunIndependentReviewApprovesAndPublishesACleanPR(t *testing.T) {
	in, fake, _ := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	in.Provider = reviewStubProvider{document: string(document)}
	in.Publish = true

	out, err := RunIndependentReview(context.Background(), in)
	if err != nil {
		t.Fatalf("RunIndependentReview: %v", err)
	}
	if !out.Created || out.Decision.Verdict != "approve" {
		t.Fatalf("expected a newly created APPROVE decision, got %+v", out)
	}
	if out.Publication == nil || !out.Publication.Published {
		t.Fatalf("expected the decision to be published, got %+v", out.Publication)
	}
	submits := 0
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			submits++
		}
	}
	if submits != 1 {
		t.Fatalf("expected exactly one SubmitReview call, got %d", submits)
	}
}

func TestRunIndependentReviewRequestsChangesOnABlockingFinding(t *testing.T) {
	in, _, _ := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "missing cancellation handling"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Provider = reviewStubProvider{document: string(document)}

	out, err := RunIndependentReview(context.Background(), in)
	if err != nil {
		t.Fatalf("RunIndependentReview: %v", err)
	}
	if out.Decision.Verdict != "request_changes" || len(out.Decision.Findings) != 1 {
		t.Fatalf("expected a REQUEST_CHANGES decision with one finding, got %+v", out.Decision)
	}
	if out.Publication != nil {
		t.Fatal("expected no publication without Publish set")
	}
}

// Re-running the same review for the same exact head must not perform a
// second independent review: it must return the decision already reached.
func TestRunIndependentReviewIsIdempotentForTheSameHead(t *testing.T) {
	in, _, store := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	in.Provider = countingReviewProvider{document: string(document), calls: &calls}

	first, err := RunIndependentReview(context.Background(), in)
	if err != nil || !first.Created {
		t.Fatalf("first RunIndependentReview: %+v %v", first, err)
	}
	second, err := RunIndependentReview(context.Background(), in)
	if err != nil {
		t.Fatalf("second RunIndependentReview: %v", err)
	}
	if second.Created {
		t.Fatal("expected the second call to find the existing decision rather than create a new one")
	}
	if second.Decision.ID != first.Decision.ID {
		t.Fatalf("expected the same decision identity, got %q and %q", first.Decision.ID, second.Decision.ID)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one reviewer invocation across both calls, got %d", calls)
	}
	decisions, err := store.ReviewDecisionsForPullRequest(testRepo.String(), 7)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("expected exactly one durable decision, got %+v %v", decisions, err)
	}
}

// Mutation check: the same underlying vendor family under a different agent
// id must still be refused as the reviewer. Removing CheckReviewIndependence's
// call in RunIndependentReview must make this test fail.
func TestRunIndependentReviewRefusesAnIndependenceCollapse(t *testing.T) {
	in, _, _ := reviewRunFixture(t)
	in.Reviewer = ResolvedAgent{ID: "codex"} // same agent id as the producer
	in.Provider = reviewStubProvider{}
	if _, err := RunIndependentReview(context.Background(), in); err == nil {
		t.Fatal("expected a reviewer that is the producer to be refused")
	}
}

type countingReviewProvider struct {
	document string
	calls    *int
}

func (p countingReviewProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	*p.calls++
	return reviewStubProvider{document: p.document}.Execute(ctx, request)
}

// movingHeadAdapter answers PullRequest with the original head for the first
// call and a MOVED head afterward, modelling a PR that advances mid-review
// (#233 acceptance C) without needing two separate real pushes.
type movingHeadAdapter struct {
	*FakeGitHubAdapter
	calls   int
	movedTo string
}

func (m *movingHeadAdapter) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	m.calls++
	pr, err := m.FakeGitHubAdapter.PullRequest(ctx, repo, number)
	if err != nil {
		return pr, err
	}
	if m.calls > 1 {
		pr.HeadSHA = m.movedTo
	}
	return pr, nil
}

// Mutation check: if the PR head moves between RunIndependentReview's two
// reads of it (the initial fetch and BuildReviewPacket's own), the workspace
// materialized for the FIRST head must never be silently reviewed as though
// it were the second. Removing the HeadSHA comparison in RunIndependentReview
// must make this test fail by letting the stale workspace through.
func TestRunIndependentReviewRefusesWhenHeadMovesMidAssembly(t *testing.T) {
	in, fake, _ := reviewRunFixture(t)
	moving := &movingHeadAdapter{FakeGitHubAdapter: fake, movedTo: testOtherSHA}
	in.GitHub = moving
	in.Provider = reviewStubProvider{}

	if _, err := RunIndependentReview(context.Background(), in); err == nil {
		t.Fatal("expected a head that moved mid-assembly to be refused")
	}
}

// Mutation check: a restart between the durable decision committing and its
// GitHub publication must be able to COMPLETE the publication on retry, not
// merely discover the decision already exists and stop. Removing the publish
// attempt in RunIndependentReview's existing-decision path must make this
// test fail.
func TestRunIndependentReviewPublishesOnRetryAfterACrashBeforePublication(t *testing.T) {
	in, fake, _ := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	in.Provider = reviewStubProvider{document: string(document)}

	// First call: decision recorded, but NOT published - models the crash
	// between those two durable steps.
	first, err := RunIndependentReview(context.Background(), in)
	if err != nil || !first.Created || first.Publication != nil {
		t.Fatalf("expected a created, unpublished decision, got %+v %v", first, err)
	}
	// Retry with --publish: must complete publication without re-reviewing.
	in.Publish = true
	second, err := RunIndependentReview(context.Background(), in)
	if err != nil {
		t.Fatalf("retry RunIndependentReview: %v", err)
	}
	if second.Created {
		t.Fatal("expected the retry to find the existing decision, not create a new one")
	}
	if second.Publication == nil || !second.Publication.Published {
		t.Fatalf("expected the retry to complete publication, got %+v", second.Publication)
	}
	submits := 0
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			submits++
		}
	}
	if submits != 1 {
		t.Fatalf("expected exactly one SubmitReview call, got %d", submits)
	}
}
