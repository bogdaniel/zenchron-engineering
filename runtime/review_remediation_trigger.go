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
	"errors"
	"fmt"

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

// ReconcileReviewRemediationForRun is ReconcileReviewRemediation's run-driven
// form (#474 B3): resolves the repository and the run's own currently
// published PR from durable state, and is a clean no-op - not an error, not
// a review - when the run has not published one yet. It is what lets a
// supervisor's existing per-run tick (driveOne, same cadence
// ObserveFeedback already runs on) call the automatic trigger by run id
// alone, without reaching into the run's projection itself.
func (r *EngineeringRuntime) ReconcileReviewRemediationForRun(ctx context.Context, runID string, port ReviewPort, reviewerAgentID string) (outcome *ReviewOutcome, admission *ReviewRemediationAdmission, err error) {
	state, err := r.load(runID)
	if err != nil {
		return nil, nil, err
	}
	if state.projection.PullRequest == nil {
		return nil, nil, nil
	}
	repo, err := parseGitHubRepo(state.run.Repository)
	if err != nil {
		return nil, nil, err
	}
	// #474 R5/R8: a durable claim against BOTH shared capacity ceilings -
	// verification AND work (an independent review is read-only but still
	// provider work) - participating in the SAME counts every ordinary
	// operation and nested VerificationPermit already share, never a
	// second, uncoordinated counter. No free slot this pass is a clean
	// no-op, exactly as idempotent as every other call here: the next tick
	// tries again.
	//
	// #474 R11: the claim's TTL is bound to THIS review's own actual
	// permitted wall-clock budget, plus the system's own minimum grace
	// margin for claim/release overhead - never a fixed constant shorter
	// than a budget the operator may configure larger. "Expired" must mean
	// the review's own enforced deadline has passed, never merely "some
	// fixed number of minutes elapsed" while a legitimate, still-alive
	// review could still be running.
	ttl := r.ReviewBudget().WallLimit + reviewVerificationClaimTTL
	claim, claimed, claimErr := r.scheduler.claimReviewVerificationSlot(r.scheduler.Owner, runID, ttl)
	if claimErr != nil {
		return nil, nil, claimErr
	}
	if !claimed {
		return nil, nil, nil
	}
	// #474 R9: a release failure is surfaced rather than swallowed, and
	// stays visible even when the review itself ALSO failed - joined, never
	// one error silently discarding the other, so an operator reading this
	// can see both the reviewer's own failure and that its capacity claim
	// also failed to release. claimReviewVerificationSlot's own
	// leaked-claim self-heal, on this exact (owner, runID) pair's NEXT call
	// once this claim expires (#474 R11: only once expired, never on the
	// mere pair match alone), is what makes that failure recoverable within
	// one bounded TTL rather than a permanent capacity loss.
	defer func() {
		if releaseErr := r.scheduler.releaseReviewVerificationSlot(claim.ID); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("releasing the review verification claim: %w", releaseErr))
		}
	}()
	var reconcileOutcome ReviewOutcome
	reconcileOutcome, admission, err = r.ReconcileReviewRemediation(ctx, port, repo, state.projection.PullRequest.Number, reviewerAgentID)
	if err != nil {
		return nil, nil, err
	}
	outcome = &reconcileOutcome
	return outcome, admission, nil
}

// ReviewBudget is the finite provider budget #474's automatic trigger binds
// every independent review invocation to (R6): the SAME operator-configured
// envelope every other unattended invocation resolves through
// RunBudgets.defaults() - planningWallLimit does the identical thing for
// InvokePlanner's own unattended, run-less invocation. defaults() never
// leaves either member at zero: zero would mean the provider may stall
// forever, the exact condition #238 exists to remove, and an unattended
// automatic reviewer is precisely the shape #238 was about. A review has no
// per-call narrowing input the way a plan stage states one for planning, so
// the configured envelope is used directly rather than narrowed against
// anything.
func (r *EngineeringRuntime) ReviewBudget() ProviderBudget {
	defaults := r.deps.Budgets.defaults()
	return ProviderBudget{WallLimit: defaults.WallLimit, InactivityLimit: defaults.ProviderInactivityLimit}
}
