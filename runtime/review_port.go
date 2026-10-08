package runtime

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReviewPort is the narrow, provider-independent surface #474's automatic
// WorkGraph review/remediation progression is expected to consume. #233 owns
// everything behind it - the review operation itself, independence, exact-head
// binding, durable decisions and GitHub publication; #474 owns deciding WHEN
// to call it and how to route REQUEST_CHANGES back into producer remediation.
// Nothing here schedules anything or holds a lease: a call either performs one
// bounded, already-governed review operation or answers a read.
type ReviewPort interface {
	// RequestReview performs an independent review of the pull request's
	// CURRENT exact head by reviewerAgentID, or returns the decision already
	// reached for that exact (subject, reviewer) identity if one exists -
	// the same idempotent behavior `review pr` itself has. It fails closed
	// (never invents provenance) when the PR cannot be bound to a producing
	// run, or when reviewerAgentID collapses into the producer's identity.
	RequestReview(ctx context.Context, repo GitHubRepo, prNumber int, reviewerAgentID string) (review.Decision, error)
	// LatestDecision is the most recently reached decision for this pull
	// request, whichever head and reviewer it was bound to, or found=false
	// when no independent review has ever completed for it.
	LatestDecision(repo GitHubRepo, prNumber int) (decision review.Decision, found bool, err error)
	// IsStale reports whether the latest decision no longer applies: the pull
	// request has moved past the exact head it was reached against. A PR
	// with no decision yet is never stale - NeedsReview is the question for
	// that case - so callers checking staleness alone must handle found=false
	// from LatestDecision themselves.
	IsStale(ctx context.Context, repo GitHubRepo, prNumber int) (bool, error)
	// NeedsReview reports whether a fresh independent review is required:
	// either none has ever been reached, or the latest one is stale.
	NeedsReview(ctx context.Context, repo GitHubRepo, prNumber int) (bool, error)
}

// SupervisorReviewPort is the runtime-owned ReviewPort implementation. It
// holds everything RunIndependentReview needs besides the subject and
// reviewer a caller names per call.
type SupervisorReviewPort struct {
	Store  *SQLiteOperationStore
	GitHub GitHubAdapter
	// ResolveAgent answers who produced a run, and who the named reviewer is -
	// the same function RunIndependentReview itself takes.
	ResolveAgent func(id string) (ResolvedAgent, error)
	// ProviderFor resolves the execution provider for a reviewer agent. It is
	// a function, not a fixed provider, because the reviewer named per call
	// can be any registered agent.
	ProviderFor  func(ResolvedAgent) (ExecutionProvider, error)
	StateDir     string
	Source       string
	ControllerID string
	Model        string
	Budgets      ProviderBudget
	Clock        Clock
	// Publish authorizes GitHub publication of every review this port
	// performs. #474 drives automatic progression; whether that progression
	// is also authorized to publish is an operator decision stated once here,
	// not a per-call choice #474 makes for itself.
	Publish bool
}

var _ ReviewPort = (*SupervisorReviewPort)(nil)

func (p *SupervisorReviewPort) RequestReview(ctx context.Context, repo GitHubRepo, prNumber int, reviewerAgentID string) (review.Decision, error) {
	reviewer, err := p.ResolveAgent(reviewerAgentID)
	if err != nil {
		return review.Decision{}, err
	}
	provider, err := p.ProviderFor(reviewer)
	if err != nil {
		return review.Decision{}, err
	}
	out, err := RunIndependentReview(ctx, RunIndependentReviewInput{
		Repo: repo, PRNumber: prNumber, Reviewer: reviewer, Provider: provider,
		ResolveAgent: p.ResolveAgent, Store: p.Store, GitHub: p.GitHub,
		StateDir: p.StateDir, ControllerID: p.ControllerID, Model: p.Model,
		Budgets: p.Budgets, Source: p.Source, Clock: p.Clock, Publish: p.Publish,
	})
	return out.Decision, err
}

func (p *SupervisorReviewPort) LatestDecision(repo GitHubRepo, prNumber int) (review.Decision, bool, error) {
	return p.Store.LatestReviewDecision(repo.String(), prNumber)
}

func (p *SupervisorReviewPort) IsStale(ctx context.Context, repo GitHubRepo, prNumber int) (bool, error) {
	latest, found, err := p.LatestDecision(repo, prNumber)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	pr, err := p.GitHub.PullRequest(ctx, repo, prNumber)
	if err != nil {
		return false, fmt.Errorf("observing the current pull request head: %w", err)
	}
	return latest.StaleAgainst(pr.HeadSHA), nil
}

func (p *SupervisorReviewPort) NeedsReview(ctx context.Context, repo GitHubRepo, prNumber int) (bool, error) {
	_, found, err := p.LatestDecision(repo, prNumber)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	return p.IsStale(ctx, repo, prNumber)
}
