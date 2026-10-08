package runtime

// GitHubRESTAdapter.SubmitReview (#233), kept apart from github_rest.go -
// which sits at its frozen file-size ceiling - rather than appended to it.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// reviewEvent maps the closed GitHubReviewState vocabulary to the wire value
// GitHub's "create a review" endpoint accepts. GitHubReviewDismissed is
// observed-only - nothing here ever creates a review in that state - so it,
// and anything outside the closed vocabulary, refuses rather than guessing.
func reviewEvent(state GitHubReviewState) (string, error) {
	switch state {
	case GitHubReviewApproved:
		return "APPROVE", nil
	case GitHubReviewChangesRequested:
		return "REQUEST_CHANGES", nil
	case GitHubReviewCommented:
		return "COMMENT", nil
	default:
		return "", fmt.Errorf("review disposition %q cannot be submitted to GitHub", state)
	}
}

func (a GitHubRESTAdapter) SubmitReview(ctx context.Context, repo GitHubRepo, number int, submission GitHubReviewSubmission) (GitHubReview, error) {
	if number <= 0 {
		return GitHubReview{}, fmt.Errorf("pull request number must be positive")
	}
	if err := safeSHA(submission.CommitSHA); err != nil {
		return GitHubReview{}, err
	}
	event, err := reviewEvent(submission.Event)
	if err != nil {
		return GitHubReview{}, err
	}
	if submission.Body.Body() == "" && len(submission.Comments) == 0 {
		return GitHubReview{}, fmt.Errorf("a review requires an explicitly cleared publication body or at least one inline comment")
	}
	type reviewComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Body string `json:"body"`
	}
	comments := make([]reviewComment, 0, len(submission.Comments))
	for _, c := range submission.Comments {
		if c.Path == "" || c.Line <= 0 || c.Body.Body() == "" {
			return GitHubReview{}, fmt.Errorf("an inline review comment requires a path, a positive line and an explicitly cleared publication body")
		}
		comments = append(comments, reviewComment{Path: c.Path, Line: c.Line, Body: c.Body.Body()})
	}
	payload := struct {
		CommitID string          `json:"commit_id"`
		Body     string          `json:"body,omitempty"`
		Event    string          `json:"event"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{CommitID: submission.CommitSHA, Body: submission.Body.Body(), Event: event, Comments: comments}
	var created struct {
		ID          int64    `json:"id"`
		User        *ghActor `json:"user"`
		State       string   `json:"state"`
		Body        string   `json:"body"`
		CommitID    string   `json:"commit_id"`
		SubmittedAt string   `json:"submitted_at"`
	}
	if err := a.call(ctx, repo, http.MethodPost, repoPath(repo)+"/pulls/"+strconv.Itoa(number)+"/reviews", nil, payload, &created); err != nil {
		return GitHubReview{}, err
	}
	review := GitHubReview{
		ID: created.ID, Author: created.User.normalize(), State: normalizeReview(created.State),
		Body: UntrustedText(created.Body), CommitSHA: created.CommitID,
	}
	if at, ok := parseTime(created.SubmittedAt); ok {
		review.SubmittedAt = at
	}
	return review, nil
}
