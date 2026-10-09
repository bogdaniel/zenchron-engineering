package runtime

// GitHubRESTAdapter.SubmitReview (#233), kept apart from github_rest.go -
// which sits at its frozen file-size ceiling - rather than appended to it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	status, raw, err := a.do(ctx, repo, http.MethodPost, repoPath(repo)+"/pulls/"+strconv.Itoa(number)+"/reviews", nil, payload)
	if err != nil {
		return GitHubReview{}, err
	}
	if status < 200 || status > 299 {
		return GitHubReview{}, classifyReviewSubmissionStatus(status, raw)
	}
	var created struct {
		ID          int64    `json:"id"`
		User        *ghActor `json:"user"`
		State       string   `json:"state"`
		Body        string   `json:"body"`
		CommitID    string   `json:"commit_id"`
		SubmittedAt string   `json:"submitted_at"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return GitHubReview{}, fmt.Errorf("github review submission returned an unexpected payload")
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

// GitHubSelfApprovalRejectedError is the ONE error PublishReview is permitted
// to downgrade an APPROVE into a COMMENT_ONLY publication for (#233 B2):
// GitHub's own, definitive refusal of a self-approval. Every other outcome -
// a timeout, a 5xx, a rate limit, an auth failure, a response lost after the
// original request may have already landed, or any OTHER 422 - must be
// preserved for replay rather than silently read as "cannot approve".
type GitHubSelfApprovalRejectedError struct{ Detail string }

func (e *GitHubSelfApprovalRejectedError) Error() string {
	return "github_self_approval_rejected: " + e.Detail
}

// selfApprovalRejectionPhrases are substrings GitHub's own 422 response body
// uses for this specific refusal. Matched case-insensitively against the
// response body only - never against a locally constructed message - so a
// different 422 (a malformed payload, an already-dismissed review, a review
// of a non-existent commit) is never mistaken for this one case.
var selfApprovalRejectionPhrases = []string{"own pull request", "own commit"}

func isSelfApprovalRejection(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, phrase := range selfApprovalRejectionPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// classifyReviewSubmissionStatus classifies a non-2xx review-submission
// response by status AND body, unlike the shared call() path other GitHubAdapter
// writes use: a review's identity-sensitive APPROVE/REQUEST_CHANGES/COMMENT
// distinction needs the one narrow self-approval case distinguished from every
// other failure, which the body text - not the status alone - is what proves.
func classifyReviewSubmissionStatus(status int, body []byte) error {
	detail := fmt.Sprintf("review submission returned status %d", status)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &GitHubAuthError{Detail: "github rejected the credential with status " + strconv.Itoa(status)}
	case status == http.StatusTooManyRequests || status >= 500:
		return &GitHubTransientError{Status: status, Detail: detail}
	case status == http.StatusUnprocessableEntity && isSelfApprovalRejection(body):
		return &GitHubSelfApprovalRejectedError{Detail: boundedTo(string(body), 500)}
	default:
		return &GitHubAPIError{Status: status, Detail: detail + ": " + boundedTo(string(body), 500)}
	}
}
