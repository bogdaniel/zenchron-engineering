package runtime

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// RunIndependentReviewInput is everything `review pr <n> --agent <id>` needs
// to perform one independent review end to end. The CLI composition root
// resolves the reviewing agent and its provider (the same way it resolves any
// other agent) and supplies everything else infrastructural; this file owns
// only the review-specific sequencing.
type RunIndependentReviewInput struct {
	Repo     GitHubRepo
	PRNumber int
	Reviewer ResolvedAgent
	Provider ExecutionProvider
	// ResolveAgent answers who produced a given run's candidate, by agent id.
	// It is a function rather than an AgentRegistry value so a caller can
	// hand over exactly "resolve an id" (ordinarily registry.Agent) without
	// this file depending on the registry's construction.
	ResolveAgent func(id string) (ResolvedAgent, error)

	Store  *SQLiteOperationStore
	GitHub GitHubAdapter

	StateDir     string
	ControllerID string
	Model        string
	Instructions []string
	Budgets      ProviderBudget
	// Source is a local directory holding a Git repository the exact PR head
	// is reachable from - the same local-checkout source
	// CreatePlanningWorkspace already takes for planning, and the same
	// reachability assumption: the operator/controller's local checkout (or
	// its remote-tracking refs) already covers the commit being reviewed.
	// Fetching an arbitrary PR head on demand is not solved here; see the
	// #233 PR description's outstanding integration dependencies.
	Source string
	Clock  Clock

	// Publish authorizes GitHub publication of the resulting decision. A
	// review may be performed and recorded durably without it - #233
	// requirement: GitHub publication requires SEPARATE authority from
	// performing the review itself.
	Publish bool
}

// RunIndependentReviewOutput is the complete, durable outcome of one `review
// pr` invocation.
type RunIndependentReviewOutput struct {
	Decision    review.Decision
	Created     bool
	Publication *review.Publication
}

// RunIndependentReview performs one independent PR review end to end:
// resolve the producing run, check independence, materialize a read-only
// exact-head workspace, invoke the reviewer through the execution Port,
// admit its verdict as a durable decision, and - only when authorized -
// publish it to GitHub. Every step fails closed; nothing here invents
// provenance it could not establish.
func RunIndependentReview(ctx context.Context, in RunIndependentReviewInput) (RunIndependentReviewOutput, error) {
	if in.Store == nil || in.GitHub == nil || in.Provider == nil {
		return RunIndependentReviewOutput{}, fmt.Errorf("an independent review requires a store, a forge adapter and a registered provider")
	}
	pr, err := in.GitHub.PullRequest(ctx, in.Repo, in.PRNumber)
	if err != nil {
		return RunIndependentReviewOutput{}, fmt.Errorf("resolving pull request #%d in %s: %w", in.PRNumber, in.Repo, err)
	}
	run, found, err := resolveRunForPullRequest(in.Store, in.Repo.String(), in.PRNumber)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	if !found {
		return RunIndependentReviewOutput{}, &ReviewSubjectUnboundError{Repository: in.Repo.String(), PRNumber: in.PRNumber}
	}
	producer, err := in.ResolveAgent(run.AgentID)
	if err != nil {
		return RunIndependentReviewOutput{}, fmt.Errorf("resolving the producing agent %q: %w", run.AgentID, err)
	}
	if err := CheckReviewIndependence(producer, in.Reviewer); err != nil {
		return RunIndependentReviewOutput{}, err
	}
	// If a decision for this EXACT subject and reviewer already exists, this
	// call is a retry (or a redundant concurrent request): return it rather
	// than performing a second independent review of work already judged.
	subject := review.Subject{Repository: in.Repo.String(), PRNumber: in.PRNumber, HeadSHA: pr.HeadSHA}
	decisionID, err := review.DecisionID(subject, in.Reviewer.ID)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	if existing, found, err := in.Store.ReviewDecision(decisionID); err != nil {
		return RunIndependentReviewOutput{}, err
	} else if found {
		out := RunIndependentReviewOutput{Decision: existing, Created: false}
		if in.Publish {
			// The decision already exists, but this invocation may be exactly
			// the retry that recovers from a crash between the decision
			// committing and its publication - requirement: a restart must be
			// able to complete publication, not merely discover there was
			// nothing left to decide. PublishReview is itself idempotent, so
			// this is always safe to attempt.
			publication, err := PublishReview(ctx, ReviewPublicationDeps{Store: in.Store, GitHub: in.GitHub}, in.Repo, existing)
			if err != nil {
				return out, err
			}
			out.Publication = &publication
		}
		return out, nil
	}
	reviewID := decisionID
	workspace, err := CreatePlanningWorkspace(in.StateDir, reviewID, in.Source, pr.HeadSHA, "")
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	defer workspace.Remove()

	packet, err := BuildReviewPacket(ctx, ReviewPacketDeps{Store: in.Store, GitHub: in.GitHub}, in.Repo, in.PRNumber, workspace)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	// BuildReviewPacket re-reads the pull request itself; a mismatch here
	// means the head moved in the narrow window between the fetch above and
	// this one, and the workspace this invocation will show the reviewer is
	// no longer the packet's own exact head. Refused rather than reconciled.
	if packet.Trusted.HeadSHA != workspace.Commit {
		return RunIndependentReviewOutput{}, fmt.Errorf(
			"pull request #%d moved from %s to %s while the review workspace was being prepared; retry the review",
			in.PRNumber, short12(workspace.Commit), short12(packet.Trusted.HeadSHA))
	}

	invocation, err := InvokeReviewer(ctx, ReviewInvocationInput{
		ReviewID: reviewID, Packet: packet, Workspace: workspace,
		Agent: in.Reviewer, Provider: in.Provider, Artifacts: ArtifactStore{Root: in.StateDir},
		StateDir: in.StateDir, ControllerID: in.ControllerID, Model: in.Model,
		Instructions: in.Instructions, Budgets: in.Budgets,
	})
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	decision, err := AdmitPRReview(ReviewInvocationInput{
		ReviewID: reviewID, Packet: packet, Workspace: workspace, Agent: in.Reviewer,
	}, invocation, in.Clock)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	stored, created, err := in.Store.CreateReviewDecision(decision)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	out := RunIndependentReviewOutput{Decision: stored, Created: created}
	if in.Publish {
		publication, err := PublishReview(ctx, ReviewPublicationDeps{Store: in.Store, GitHub: in.GitHub}, in.Repo, stored)
		if err != nil {
			return out, err
		}
		out.Publication = &publication
	}
	return out, nil
}
