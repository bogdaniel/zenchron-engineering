package runtime

// #474: the one automatic entry point that removes the operator as the
// relay between an independent reviewer and the producer it blocks.
//
// Nothing in runtime/reconciler.go calls RequestReview: #233's own review
// operation runs a provider and must stay outside a producer run's
// per-tick Reconcile pass, capacity-respecting the same way review pr
// already is as a bounded, separately-scheduled operation
// (docs/review.md, "Known scope limits"). ReconcileReviewRemediation is
// that separately-scheduled operation's automatic shape: a supervisor's own
// observation tick calls it once per tracked, published PR - the same
// relationship feedback_observe.go's polling has to Reconcile - and it is
// the only place #233's ReviewPort and #474's AdmitReviewRemediation are
// composed into one governed progression.

import (
	"context"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReconcileReviewRemediation performs one bounded pass of #474's automatic
// progression for repo#prNumber: a fresh independent review when one is
// needed, and a durable remediation admission when the review that is
// current - the one just performed, or an earlier one still current -
// reached REQUEST_CHANGES. It is idempotent at every step (NeedsReview,
// RequestReview and AdmitReviewRemediation all are), so calling it
// repeatedly - once per supervisor tick, forever - is the whole mechanism:
// nothing here remembers it was already called.
//
// admission is nil whenever no remediation was newly or previously admitted
// this call (no decision yet, an APPROVE/COMMENT_ONLY decision, or an
// AdmitReviewRemediation refusal - the refusal itself is still returned as
// err, never silently dropped).
func (r *EngineeringRuntime) ReconcileReviewRemediation(ctx context.Context, port ReviewPort, repo GitHubRepo, prNumber int, reviewerAgentID string) (ReviewOutcome, *ReviewRemediationAdmission, error) {
	needsReview, err := port.NeedsReview(ctx, repo, prNumber)
	if err != nil {
		return ReviewOutcome{}, nil, err
	}
	var outcome ReviewOutcome
	if needsReview {
		outcome, err = port.RequestReview(ctx, repo, prNumber, reviewerAgentID)
		if err != nil {
			return ReviewOutcome{}, nil, err
		}
	} else {
		decision, found, err := port.LatestDecision(repo, prNumber)
		if err != nil {
			return ReviewOutcome{}, nil, err
		}
		if !found {
			return ReviewOutcome{}, nil, nil
		}
		outcome = ReviewOutcome{Decision: decision}
	}
	if outcome.Decision.Verdict != review.VerdictRequestChanges {
		return outcome, nil, nil
	}
	admission, _, err := r.AdmitReviewRemediation(ctx, port, repo, prNumber)
	if err != nil {
		return outcome, nil, err
	}
	return outcome, &admission, nil
}
