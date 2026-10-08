package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestFakeGitHubAdapterSubmitReviewIsObservableAfterward(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: testHeadSHA}

	submitted, err := fake.SubmitReview(ctx, testRepo, 1, GitHubReviewSubmission{
		CommitSHA: testHeadSHA, Event: GitHubReviewApproved, Body: mustPublication(t, "looks good"),
	})
	if err != nil || submitted.State != GitHubReviewApproved || submitted.CommitSHA != testHeadSHA {
		t.Fatalf("SubmitReview: %+v %v", submitted, err)
	}
	after, err := fake.Reviews(ctx, testRepo, 1, testHeadSHA)
	if err != nil || len(after.Reviews) != 1 || after.Reviews[0].ID != submitted.ID {
		t.Fatalf("Reviews after SubmitReview: %+v %v", after, err)
	}
}

func TestFakeGitHubAdapterSubmitReviewRefusesAnUnrecognizedEvent(t *testing.T) {
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: testHeadSHA}
	_, err := fake.SubmitReview(context.Background(), testRepo, 1, GitHubReviewSubmission{
		CommitSHA: testHeadSHA, Event: GitHubReviewDismissed, Body: mustPublication(t, "x"),
	})
	if err == nil {
		t.Fatal("expected GitHubReviewDismissed to be refused: it is observed-only and never submittable")
	}
}

func TestGitHubRESTAdapterSubmitReviewShapesItsRequest(t *testing.T) {
	ctx := context.Background()
	responses := restResponses()
	responses["POST /repos/zenchron/fixture/pulls/5/reviews"] = `{"id":9,"user":{"login":"reviewer","id":3},"state":"APPROVED","commit_id":"` + testHeadSHA + `","submitted_at":"2026-01-02T03:04:05Z"}`
	doer := &fakeGitHubDoer{responses: responses}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: staticCredential{secret: testToken}}

	submitted, err := adapter.SubmitReview(ctx, testRepo, 5, GitHubReviewSubmission{
		CommitSHA: testHeadSHA, Event: GitHubReviewApproved, Body: mustPublication(t, "looks good"),
	})
	if err != nil || submitted.State != GitHubReviewApproved || submitted.CommitSHA != testHeadSHA {
		t.Fatalf("SubmitReview: %+v %v", submitted, err)
	}
	if len(doer.requests) != 1 {
		t.Fatalf("expected exactly one request, got %d: %+v", len(doer.requests), doer.requests)
	}
	got := doer.requests[0]
	wantURL := "https://api.github.com/repos/zenchron/fixture/pulls/5/reviews"
	if got.Method != "POST" || got.URL != wantURL {
		t.Fatalf("request: %s %s, want POST %s", got.Method, got.URL, wantURL)
	}
	if !strings.Contains(got.Body, `"commit_id":"`+testHeadSHA+`"`) || !strings.Contains(got.Body, `"event":"APPROVE"`) {
		t.Fatalf("request body: %s", got.Body)
	}
}

func TestGitHubRESTAdapterSubmitReviewRefusesAnUnsafeSHA(t *testing.T) {
	adapter := GitHubRESTAdapter{HTTP: &fakeGitHubDoer{responses: map[string]string{}}, Credentials: staticCredential{secret: testToken}}
	_, err := adapter.SubmitReview(context.Background(), testRepo, 5, GitHubReviewSubmission{
		CommitSHA: "not-a-sha; rm -rf", Event: GitHubReviewApproved, Body: mustPublication(t, "x"),
	})
	if err == nil {
		t.Fatal("expected an unsafe commit SHA to be refused before any request is made")
	}
}
