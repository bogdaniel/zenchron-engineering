package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReviewPublicationDeps is the narrow write surface PublishReview needs.
type ReviewPublicationDeps struct {
	Store  *SQLiteOperationStore
	GitHub GitHubAdapter
}

// PublishReview publishes a durable review.Decision to GitHub exactly once
// (#233 requirements 11, 14, 15). It is always safe to call again: a decision
// already recorded as published is a no-op, and a publication failure never
// touches the durable decision it failed to publish - RecordReviewPublication
// writes a wholly separate row.
//
// repo and number are the decision's own subject restated by the caller as a
// GitHubRepo/int rather than re-parsed here, because this file has no business
// re-deriving identity Decision.Subject already states authoritatively.
func PublishReview(ctx context.Context, deps ReviewPublicationDeps, repo GitHubRepo, decision review.Decision) (review.Publication, error) {
	if deps.Store == nil || deps.GitHub == nil {
		return review.Publication{}, fmt.Errorf("publishing a review requires a store and a forge adapter")
	}
	if err := decision.Validate(); err != nil {
		return review.Publication{}, err
	}
	if existing, found, err := deps.Store.ReviewPublication(decision.ID); err != nil {
		return review.Publication{}, err
	} else if found && existing.Published {
		return existing, nil
	}
	// A current PR read guards the one case a durable "not yet published" row
	// cannot: the PR moved past this decision's exact head since it was
	// reached, in which case publishing now would attach this verdict to a
	// head it never inspected.
	current, err := deps.GitHub.PullRequest(ctx, repo, decision.Subject.PRNumber)
	if err != nil {
		return review.Publication{}, fmt.Errorf("observing the current pull request head before publishing: %w", err)
	}
	if decision.StaleAgainst(current.HeadSHA) {
		return review.Publication{}, fmt.Errorf(
			"review decision %s was reached for head %s, and the pull request has since moved to %s; "+
				"a stale decision is never published as if it applied to the new head", decision.ID, short12(decision.Subject.HeadSHA), short12(current.HeadSHA))
	}
	if observed, ok, err := observeOwnPublishedReview(ctx, deps, repo, decision); err != nil {
		return review.Publication{}, err
	} else if ok {
		// GitHub already carries this exact decision's review - an earlier
		// attempt landed before its local record did. Reconciling from the
		// observation makes this crash-safe without submitting a second one.
		inserted, err := deps.Store.RecordReviewPublication(observed)
		if err != nil {
			return review.Publication{}, err
		}
		if inserted {
			return observed, nil
		}
		existing, _, err := deps.Store.ReviewPublication(decision.ID)
		return existing, err
	}
	event, wantApprove := reviewStateFor(decision.Verdict)
	body, err := NewPublication(reviewPublicationBody(decision))
	if err != nil {
		return review.Publication{}, err
	}
	submission := GitHubReviewSubmission{CommitSHA: decision.Subject.HeadSHA, Event: event, Body: body}
	created, err := deps.GitHub.SubmitReview(ctx, repo, decision.Subject.PRNumber, submission)
	publishedVerdict := decision.Verdict
	if err != nil && wantApprove {
		// #233 requirement 13: when the configured identity cannot submit an
		// approval (GitHub refuses self-approval of one's own pull request),
		// fall back to an explicit COMMENT_ONLY rather than pretending GitHub
		// approved, or silently failing a decision that IS otherwise durable.
		submission.Event = GitHubReviewCommented
		created, err = deps.GitHub.SubmitReview(ctx, repo, decision.Subject.PRNumber, submission)
		publishedVerdict = review.VerdictCommentOnly
	}
	if err != nil {
		return review.Publication{}, fmt.Errorf("publishing review %s: %w", decision.ID, err)
	}
	publication := review.Publication{
		DecisionID: decision.ID, Published: true, GitHubReviewID: created.ID,
		PublishedVerdict: publishedVerdict, PublishedAt: created.SubmittedAt,
	}
	if _, err := deps.Store.RecordReviewPublication(publication); err != nil {
		return review.Publication{}, err
	}
	return publication, nil
}

// observeOwnPublishedReview asks GitHub itself whether this exact decision
// was already published, for the crash window between SubmitReview
// succeeding and RecordReviewPublication committing. It requires a forge that
// can answer "who am I" (ForgeViewer); an adapter that cannot is not asked -
// which is also why a configuration lacking it must rely on
// RecordReviewPublication's insert-once guarantee alone, exactly as feedback
// admission already depends on ForgeViewer for its own self-identity guard.
func observeOwnPublishedReview(ctx context.Context, deps ReviewPublicationDeps, repo GitHubRepo, decision review.Decision) (review.Publication, bool, error) {
	viewer, ok := deps.GitHub.(ForgeViewer)
	if !ok {
		return review.Publication{}, false, nil
	}
	self, err := viewer.Viewer(ctx, repo)
	if err != nil {
		return review.Publication{}, false, nil
	}
	observation, err := deps.GitHub.Reviews(ctx, repo, decision.Subject.PRNumber, decision.Subject.HeadSHA)
	if err != nil {
		return review.Publication{}, false, err
	}
	for _, observed := range observation.Reviews {
		if observed.Author.Login != "" && strings.EqualFold(observed.Author.Login, self.Login) {
			return review.Publication{
				DecisionID: decision.ID, Published: true, GitHubReviewID: observed.ID,
				PublishedVerdict: verdictFor(observed.State), PublishedAt: observed.SubmittedAt,
			}, true, nil
		}
	}
	return review.Publication{}, false, nil
}

// reviewStateFor maps a durable decision to the GitHub event to submit, and
// reports whether that event is the identity-sensitive APPROVE.
func reviewStateFor(verdict review.Verdict) (GitHubReviewState, bool) {
	switch verdict {
	case review.VerdictApprove:
		return GitHubReviewApproved, true
	case review.VerdictRequestChanges:
		return GitHubReviewChangesRequested, false
	default:
		return GitHubReviewCommented, false
	}
}

// verdictFor is reviewStateFor's inverse, for reconciling an observed review
// back into the vocabulary a Publication records.
func verdictFor(state GitHubReviewState) review.Verdict {
	switch state {
	case GitHubReviewApproved:
		return review.VerdictApprove
	case GitHubReviewChangesRequested:
		return review.VerdictRequestChanges
	default:
		return review.VerdictCommentOnly
	}
}

// reviewPublicationBody renders the human-facing review body. Findings are the
// runtime's OWN bounded signatures - never repository or PR text - so nothing
// a candidate or a PR comment wrote can reach this excerpt before
// NewPublication's own secret-shape backstop even runs.
func reviewPublicationBody(decision review.Decision) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Independent review of %s by agent %s (%s verdict).\n\n", short12(decision.Subject.HeadSHA), decision.ReviewerAgentID, decision.Verdict)
	if decision.Reason != "" {
		fmt.Fprintf(&out, "%s\n\n", decision.Reason)
	}
	if len(decision.Findings) == 0 {
		out.WriteString("No findings.\n")
		return out.String()
	}
	out.WriteString("Findings:\n")
	for _, finding := range decision.Findings {
		fmt.Fprintf(&out, "- [%s] %s", finding.Severity, finding.Signature)
		if finding.Detail != "" {
			fmt.Fprintf(&out, ": %s", finding.Detail)
		}
		out.WriteString("\n")
	}
	return out.String()
}
