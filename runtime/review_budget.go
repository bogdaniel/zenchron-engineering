package runtime

import (
	"context"
	"fmt"
	"time"
)

// reportReviewBudgetStop finishes the delivery's status reporting before the
// durable budget wait. Failure leaves the
// run retryable and is surfaced to the operator. Updating the body is
// idempotent even if the process dies after the remote write but before settle.
// No feedback text, provider output, or local artifact enters the notice.
func (r *EngineeringRuntime) reportReviewBudgetStop(ctx context.Context, state *runState) error {
	pr := state.projection.PullRequest
	if pr == nil || pr.Merged || pr.State != string(GitHubOpen) {
		return nil
	}
	if state.snapshot.Disposition == Waiting && state.snapshot.Reason == ReasonReviewBudgetExhausted {
		return nil
	}
	if state.controllerChanged || state.projection.ObservedExternalHead != "" || !state.authorizedForPublication() {
		return fmt.Errorf("run_wall_budget_exhausted: PR status notice requires current publication authority")
	}
	// Status reporting is bounded independently of the exhausted engineering
	// allowance; it never invokes a provider or changes the candidate.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	current, err := r.deps.GitHub.PullRequest(ctx, r.repo, pr.Number)
	if err != nil {
		return fmt.Errorf("read PR for budget stop notice: %w", err)
	}
	if current.Merged || current.State != GitHubOpen {
		return nil
	}
	if current.HeadSHA != pr.HeadRevision {
		return fmt.Errorf("run_wall_budget_exhausted: PR head changed before status notice")
	}
	body, err := r.publicationBody(state)
	if err != nil {
		return err
	}
	body, err = NewPublication(body.Body() + "\n\n## Runtime stopped: wall budget exhausted\n\n" +
		"Automated review work has stopped. New review feedback is deferred; any admitted feedback not yet delivered remains pending. " +
		"This pull request remains open for human review. An operator must explicitly authorize a larger total wall allowance with autonomy budget-extend " + state.run.ID + " <total-duration>, then resume the run. Changing configuration alone does not extend this run.")
	if err != nil {
		return err
	}
	if _, err := r.deps.GitHub.UpdatePullRequest(ctx, r.repo, pr.Number, GitHubPullRequestUpdate{Body: body}); err != nil {
		return fmt.Errorf("publish PR budget stop notice: %w", err)
	}
	return nil
}
