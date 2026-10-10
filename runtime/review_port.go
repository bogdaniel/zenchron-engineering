package runtime

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReviewOutcome is everything #474 needs from one RequestReview call: the
// durable decision AND what actually happened when this runtime tried to
// publish it.
//
// #474 MUST route a REQUEST_CHANGES decision to producer remediation from
// THIS outcome - never by waiting for the published GitHub review to pass
// ordinary human feedback admission. runtime/feedback.go's self-loop guard
// deliberately refuses a review authored by this runtime's own publishing
// identity, including a REQUEST_CHANGES, precisely to stop the runtime
// feeding itself; when the reviewer's publishing identity is the same
// principal that opened the producer PR (the common case absent a
// deliberately distinct reviewer-publishing identity), a published review
// would be refused by that very guard and never reach the ordinary feedback
// path at all. Decision.Verdict, read directly off this outcome, is the
// independently reached FACT #474 routes on - never itself the authority to
// act on it: #233 establishes what was decided, #474 remains the sole owner
// of deciding whether and how that fact becomes an authorized remediation
// invocation (docs/review.md, "The #474 interface"). GitHub publication is
// for human visibility and is never the channel #474 consumes decisions
// through.
type ReviewOutcome struct {
	Decision review.Decision
	// Published reports whether a GitHub review was submitted for Decision.
	// False is a legitimate, common outcome (publication wasn't requested, or
	// is not yet authorized) and never implies the decision itself is any
	// less durable or any less ready to route to remediation.
	Published bool
	// PublishedVerdict is what was ACTUALLY sent to GitHub when Published is
	// true - which can differ from Decision.Verdict (the APPROVE -> COMMENT_ONLY
	// fallback). #474 routes on Decision.Verdict regardless of this field.
	PublishedVerdict review.Verdict
}

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
	RequestReview(ctx context.Context, repo GitHubRepo, prNumber int, reviewerAgentID string) (ReviewOutcome, error)
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
	ProviderFor func(ResolvedAgent) (ExecutionProvider, error)
	StateDir    string
	Source      string
	// Remote and Credentials are RunIndependentReviewInput's own fallback
	// (#474 B3), passed straight through: a long-running supervisor governing
	// several repositories has no single Source checkout to assume.
	Remote       RemoteIdentity
	Credentials  CredentialProvider
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

func (p *SupervisorReviewPort) RequestReview(ctx context.Context, repo GitHubRepo, prNumber int, reviewerAgentID string) (ReviewOutcome, error) {
	reviewer, err := p.ResolveAgent(reviewerAgentID)
	if err != nil {
		return ReviewOutcome{}, err
	}
	provider, err := p.ProviderFor(reviewer)
	if err != nil {
		return ReviewOutcome{}, err
	}
	out, err := RunIndependentReview(ctx, RunIndependentReviewInput{
		Repo: repo, PRNumber: prNumber, Reviewer: reviewer, Provider: provider,
		ResolveAgent: p.ResolveAgent, Store: p.Store, GitHub: p.GitHub,
		StateDir: p.StateDir, ControllerID: p.ControllerID, Model: p.Model,
		Budgets: p.Budgets, Source: p.Source, Remote: p.Remote, Credentials: p.Credentials,
		Clock: p.Clock, Publish: p.Publish,
	})
	outcome := ReviewOutcome{Decision: out.Decision}
	if out.Publication != nil {
		outcome.Published, outcome.PublishedVerdict = out.Publication.Published, out.Publication.PublishedVerdict
	}
	return outcome, err
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
