package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReviewPublicationDeps is the narrow write surface PublishReview needs.
type ReviewPublicationDeps struct {
	Store  *SQLiteOperationStore
	GitHub GitHubAdapter
	// Clock is the claim's time source; nil reads the real wall clock
	// (clockNow's default), which is what every caller outside a test wants.
	Clock Clock
}

// PublishReview publishes a durable review.Decision to GitHub exactly once
// (#233 requirements 11, 14, 15). It is always safe to call again: a decision
// already recorded as published is a no-op, and a publication failure never
// touches the durable decision it failed to publish - RecordReviewPublication
// writes a wholly separate row.
//
// Exclusivity lives HERE, not in a wrapper a caller might forget to use
// (#233 B4-3): the whole body below runs under an exclusive claim on
// "publish:"+decision.ID, so two direct callers of this exported function -
// not just two callers of some other entry point - can never both observe
// "not yet published" and both reach GitHub.
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
	claimKey := "publish:" + decision.ID
	claimed, token, err := deps.Store.ClaimReview(claimKey, "publish", clockNow(deps.Clock), reviewClaimStaleAfter)
	if err != nil {
		return review.Publication{}, err
	}
	if !claimed {
		return review.Publication{}, &ReviewClaimConflictError{ClaimKey: claimKey}
	}
	defer deps.Store.ReleaseReviewClaim(claimKey, token)

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
	created, submitErr := deps.GitHub.SubmitReview(ctx, repo, decision.Subject.PRNumber, submission)
	publishedVerdict := decision.Verdict
	var selfRejected *GitHubSelfApprovalRejectedError
	if submitErr != nil && wantApprove && errors.As(submitErr, &selfRejected) {
		// #233 requirement 13: GitHub's own DEFINITIVE refusal of this
		// identity's self-approval - and only that, never a timeout, a 5xx, a
		// rate limit, an auth failure or any other 422 - is the one permitted
		// downgrade to an explicit COMMENT_ONLY, rather than pretending GitHub
		// approved or silently failing a decision that is otherwise durable.
		submission.Event = GitHubReviewCommented
		created, submitErr = deps.GitHub.SubmitReview(ctx, repo, decision.Subject.PRNumber, submission)
		publishedVerdict = review.VerdictCommentOnly
	}
	if submitErr != nil {
		// The failure may describe OUR client, not GitHub's outcome: a
		// timeout or a dropped connection can lose the reply to a request
		// GitHub already processed. One more marker-based observation
		// distinguishes "nothing landed" from "it landed and we only lost the
		// reply", without ever submitting a second review to find out.
		if observed, ok, obsErr := observeOwnPublishedReview(ctx, deps, repo, decision); obsErr == nil && ok {
			if _, err := deps.Store.RecordReviewPublication(observed); err != nil {
				return review.Publication{}, err
			}
			return observed, nil
		}
		return review.Publication{}, fmt.Errorf("publishing review %s: %w", decision.ID, submitErr)
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

// observeOwnPublishedReview asks GitHub itself whether THIS EXACT decision was
// already published, for the crash window between SubmitReview succeeding and
// RecordReviewPublication committing. It requires a forge that can answer
// "who am I" (ForgeViewer); an adapter that cannot is not asked - which is
// also why a configuration lacking it must rely on RecordReviewPublication's
// insert-once guarantee alone, exactly as feedback admission already depends
// on ForgeViewer for its own self-identity guard.
//
// Matching is by decision-ID MARKER (#233 B1), never by identity alone: the
// self identity narrows to "a review this runtime published", and the marker
// - embedded in the body every publication writes, see reviewPublicationBody
// - narrows that to "the review THIS decision published". Without the marker,
// reviewer A's own earlier APPROVE/COMMENT on this exact head would be
// mistaken for proof that reviewer B's unrelated REQUEST_CHANGES decision was
// published, silently suppressing B's blocking feedback. More than one match
// is an authority question this function refuses to guess at.
func observeOwnPublishedReview(ctx context.Context, deps ReviewPublicationDeps, repo GitHubRepo, decision review.Decision) (review.Publication, bool, error) {
	viewer, ok := deps.GitHub.(ForgeViewer)
	if !ok {
		return review.Publication{}, false, nil
	}
	self, err := viewer.Viewer(ctx, repo)
	if err != nil {
		// Fails closed, exactly as feedback admission's own self-loop guard
		// does when it cannot resolve its publication identity
		// (docs/github-feedback.md): without knowing which account this
		// runtime publishes as, "no matching review observed" cannot be told
		// apart from "this decision was already published and we simply
		// cannot see it", and proceeding to submit anyway risks exactly the
		// duplicate-publication outcome this reconciliation exists to avoid.
		return review.Publication{}, false, fmt.Errorf("resolving this runtime's own publishing identity before publication: %w", err)
	}
	observation, err := deps.GitHub.Reviews(ctx, repo, decision.Subject.PRNumber, decision.Subject.HeadSHA)
	if err != nil {
		return review.Publication{}, false, err
	}
	var matches []GitHubReview
	for _, observed := range observation.Reviews {
		if observed.Author.Login == "" || !strings.EqualFold(observed.Author.Login, self.Login) {
			continue
		}
		if id, ok := reviewDecisionIDFromBody(string(observed.Body)); !ok || id != decision.ID {
			continue
		}
		matches = append(matches, observed)
	}
	switch len(matches) {
	case 0:
		return review.Publication{}, false, nil
	case 1:
		observed := matches[0]
		return review.Publication{
			DecisionID: decision.ID, Published: true, GitHubReviewID: observed.ID,
			PublishedVerdict: verdictFor(observed.State), PublishedAt: observed.SubmittedAt,
		}, true, nil
	default:
		return review.Publication{}, false, fmt.Errorf(
			"%d reviews by %s on exact head %s carry decision marker %s; refusing to guess which is authoritative",
			len(matches), self.Login, short12(decision.Subject.HeadSHA), decision.ID)
	}
}

// reviewDecisionMarker is the immutable, machine-readable identity a
// published review body carries: an HTML comment, invisible in GitHub's
// rendered markdown, naming the EXACT decision it is proof of publishing.
// Matching on this - never on a human name, a disposition alone, or "the
// first review by this identity" - is what makes reconciliation specific to
// one decision among however many this identity has ever published to one PR.
const (
	reviewDecisionMarkerPrefix = "<!-- zenchron-review-decision: "
	reviewDecisionMarkerSuffix = " -->"
)

func reviewDecisionMarker(id string) string {
	return reviewDecisionMarkerPrefix + id + reviewDecisionMarkerSuffix
}

// reviewDecisionIDFromBody extracts the decision id a review body's marker
// names, or false if the body carries none.
func reviewDecisionIDFromBody(body string) (string, bool) {
	start := strings.Index(body, reviewDecisionMarkerPrefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(reviewDecisionMarkerPrefix):]
	end := strings.Index(rest, reviewDecisionMarkerSuffix)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
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
	} else {
		out.WriteString("Findings:\n")
		for _, finding := range decision.Findings {
			fmt.Fprintf(&out, "- [%s] %s", finding.Severity, finding.Signature)
			if finding.Detail != "" {
				fmt.Fprintf(&out, ": %s", finding.Detail)
			}
			out.WriteString("\n")
		}
	}
	out.WriteString("\n" + reviewDecisionMarker(decision.ID) + "\n")
	return out.String()
}
