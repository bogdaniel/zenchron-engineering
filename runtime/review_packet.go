package runtime

import (
	"context"
	"fmt"
	"strings"
)

// The review context packet (#233).
//
// A packet is assembled ENTIRELY from runtime-owned and machine-observed
// facts: the producing EngineeringRun this runtime already holds, and the
// forge's own answers about the exact PR head. Nothing here is read from a
// PR description, an issue body or a comment and then trusted as fact - that
// material is carried in ReviewUntrustedContext, which is exactly as far as
// it ever travels toward being an instruction.

// ReviewTrustedFacts are facts this runtime itself established or the forge
// reported about identity and state - never free text another party wrote.
type ReviewTrustedFacts struct {
	RunID      string
	Repository string
	PRNumber   int
	PRURL      string
	HeadSHA    string
	BaseRef    string
	BaseSHA    string
	// ProducerAgentID is the run's own AgentID: the worker that produced the
	// candidate this review inspects. It is what CheckReviewIndependence
	// compares the reviewer against.
	ProducerAgentID   string
	CandidateRevision string
	CandidateTree     string
	ContractID        string
	ContractRevision  string
	// CIState and FailingChecks are the forge's CI conclusion for EXACTLY
	// HeadSHA (GitHubAdapter.Checks never answers "latest"), so there is no
	// stale-CI case to additionally guard against here: an observation bound
	// to a superseded head is simply not this field, by construction.
	CIState       GitHubCheckState
	FailingChecks []string
	// ExistingReviews is prior review METADATA (who, when, what state) for
	// exactly HeadSHA. Review bodies are untrusted prose and travel in
	// ReviewUntrustedContext.ReviewComments instead; this is deliberately the
	// trusted half of the same observation.
	ExistingReviews []GitHubReview
}

// ReviewUntrustedContext is everything a human, a producer agent, or
// repository content wrote. It is handed to the reviewer the same way an
// upstream stage's diff and a feedback item already are elsewhere in this
// runtime: delimited, labelled, and never parsed by this runtime as an
// instruction.
type ReviewUntrustedContext struct {
	IssueNumber    int
	IssueTitle     UntrustedText
	IssueBody      UntrustedText
	PRComments     []GitHubComment
	ReviewComments []GitHubReviewComment
	// Diff is the exact machine-retrieved change between BaseSHA and HeadSHA.
	// Retrieval is machine-owned (git, never prose), but the BYTES are
	// candidate content like any other diff this runtime frames for a worker,
	// so they are untrusted the same way runtime.UpstreamContext.Diff is.
	Diff      string
	Truncated bool
}

// ReviewPacket is the complete, runtime-assembled review context (#233). Its
// two halves are never merged: a reviewer contract renders Trusted as facts
// and Untrusted as explicitly delimited third-party data, the same separation
// feedback and upstream diffs already keep everywhere else in this runtime.
type ReviewPacket struct {
	Trusted   ReviewTrustedFacts
	Untrusted ReviewUntrustedContext
}

// ReviewSubjectUnboundError is the fail-closed refusal when a PR cannot be
// bound to a producing EngineeringRun. #233 is explicit that this runtime
// must refuse rather than invent provenance from the PR description, so this
// is the one error BuildReviewPacket returns for that case rather than
// proceeding with an empty or guessed run identity.
type ReviewSubjectUnboundError struct {
	Repository string
	PRNumber   int
}

func (e *ReviewSubjectUnboundError) Error() string {
	return fmt.Sprintf("no EngineeringRun in %s has observed pull request #%d; "+
		"it cannot be safely bound to a producing run, candidate or evidence subject", e.Repository, e.PRNumber)
}

// ReviewPacketDeps is the narrow read surface BuildReviewPacket needs. It
// takes the store and the forge directly, rather than a composed engine,
// because assembling a packet is a set of reads and never drives a run.
type ReviewPacketDeps struct {
	Store  *SQLiteOperationStore
	GitHub GitHubAdapter
}

// BuildReviewPacket assembles the complete review context for one pull
// request. workspace is the already-materialized, exact-head checkout the
// caller built for the review invocation itself (see InvokeReviewer); it is
// read here only to produce the diff, and may be nil, which answers Diff as
// empty with Truncated false - "the runtime could not read it", stated rather
// than silently hidden, mirroring readCandidateDiff's own convention.
func BuildReviewPacket(ctx context.Context, deps ReviewPacketDeps, repo GitHubRepo, prNumber int, workspace *PlanningWorkspace) (ReviewPacket, error) {
	if deps.Store == nil || deps.GitHub == nil {
		return ReviewPacket{}, fmt.Errorf("a review packet requires a store and a forge adapter")
	}
	if prNumber <= 0 {
		return ReviewPacket{}, fmt.Errorf("pull request number must be positive")
	}
	pr, err := deps.GitHub.PullRequest(ctx, repo, prNumber)
	if err != nil {
		return ReviewPacket{}, fmt.Errorf("resolving pull request #%d in %s: %w", prNumber, repo, err)
	}
	run, found, err := resolveRunForPullRequest(deps.Store, repo.String(), prNumber)
	if err != nil {
		return ReviewPacket{}, err
	}
	if !found {
		return ReviewPacket{}, &ReviewSubjectUnboundError{Repository: repo.String(), PRNumber: prNumber}
	}
	// CI for the EXACT head. A failed observation is propagated rather than
	// read as "no CI": missing or stale CI is never equivalent to green CI,
	// and silently substituting GitHubCheckNone here would do exactly that.
	checks, err := deps.GitHub.Checks(ctx, repo, pr.HeadSHA)
	if err != nil {
		return ReviewPacket{}, fmt.Errorf("observing CI for exact head %s: %w", short12(pr.HeadSHA), err)
	}
	reviews, err := deps.GitHub.Reviews(ctx, repo, prNumber, pr.HeadSHA)
	if err != nil {
		return ReviewPacket{}, fmt.Errorf("observing existing reviews for exact head %s: %w", short12(pr.HeadSHA), err)
	}
	var prComments []GitHubComment
	if conversation, ok := deps.GitHub.(ForgeConversation); ok {
		prComments, err = conversation.PullRequestComments(ctx, repo, prNumber)
		if err != nil {
			return ReviewPacket{}, fmt.Errorf("reading pull request comments: %w", err)
		}
	}
	// The source issue is enrichment, read on a best-effort basis: run.Goal
	// already carries the trusted engineering intent (issueGoal's own
	// deterministic encoding), and a forge read failing here is not a reason
	// to refuse the whole packet the way an unresolved run binding is.
	var issueNumber int
	var issue GitHubIssue
	if number, ok := parseIssueGoal(run.Goal, repo.String()); ok {
		issueNumber = number
		if observed, err := deps.GitHub.Issue(ctx, repo, number); err == nil {
			issue = observed
		}
	}
	var diff string
	var truncated bool
	if workspace != nil {
		diff, truncated, err = verifiedReviewDiff(workspace.Dir, pr.BaseSHA, pr.HeadSHA)
		if err != nil {
			return ReviewPacket{}, fmt.Errorf("establishing the verified diff for exact head %s: %w", short12(pr.HeadSHA), err)
		}
	}
	return ReviewPacket{
		Trusted: ReviewTrustedFacts{
			RunID: run.ID, Repository: repo.String(), PRNumber: prNumber, PRURL: pr.URL,
			HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, BaseSHA: pr.BaseSHA,
			ProducerAgentID:   run.AgentID,
			CandidateRevision: run.Candidate.Revision, CandidateTree: run.Candidate.Tree,
			ContractID: run.Contract.ID, ContractRevision: run.Contract.Revision,
			CIState: checks.State, FailingChecks: failingCheckNames(checks.Runs),
			ExistingReviews: reviews.Reviews,
		},
		Untrusted: ReviewUntrustedContext{
			IssueNumber: issueNumber, IssueTitle: issue.Title, IssueBody: issue.Body,
			PRComments: prComments, ReviewComments: reviews.Comments,
			Diff: diff, Truncated: truncated,
		},
	}, nil
}

func failingCheckNames(runs []GitHubCheckRun) []string {
	var names []string
	for _, run := range runs {
		if run.State == GitHubCheckFailure {
			names = append(names, string(run.Name))
		}
	}
	return names
}

// parseIssueGoal recovers the issue number from a run's Goal when it was
// created by StartIssueRun, whose issueGoal encodes it deterministically
// rather than as free text this function would have to guess at.
func parseIssueGoal(goal, repository string) (int, bool) {
	prefix := "github-issue:" + repository + "#"
	if len(goal) <= len(prefix) || goal[:len(prefix)] != prefix {
		return 0, false
	}
	var number int
	if _, err := fmt.Sscanf(goal[len(prefix):], "%d", &number); err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

// resolveRunForPullRequest finds the EngineeringRun that opened exactly this
// pull request number in this repository, from the runtime's own
// github.pr_observed journal history - never from the PR itself, which is
// the one thing a hostile PR description could lie about.
//
// ponytail: a linear scan over this repository's runs and their journals,
// fine for an operator's tens-to-hundreds of runs; add a dedicated index if a
// repository's run count ever makes this slow.
func resolveRunForPullRequest(store *SQLiteOperationStore, repository string, prNumber int) (EngineeringRun, bool, error) {
	runs, err := store.runsForRepository(repository)
	if err != nil {
		return EngineeringRun{}, false, err
	}
	var matched *EngineeringRun
	for i := range runs {
		events, err := store.Events(runs[i].ID)
		if err != nil {
			return EngineeringRun{}, false, err
		}
		for _, event := range events {
			if event.Type != EventGitHubPRObserved {
				continue
			}
			var payload GitHubPRObservedPayload
			if err := strictJSON(event.Payload, &payload); err != nil {
				continue
			}
			if payload.Number != prNumber {
				continue
			}
			if matched != nil && matched.ID != runs[i].ID {
				return EngineeringRun{}, false, fmt.Errorf(
					"pull request #%d in %s is bound to more than one run (%s and %s); refusing to guess which produced it",
					prNumber, repository, matched.ID, runs[i].ID)
			}
			found := runs[i]
			matched = &found
			break
		}
	}
	if matched == nil {
		return EngineeringRun{}, false, nil
	}
	return *matched, true, nil
}

// verifiedReviewDiff reads the exact base..head diff from the content-verified
// subject store (#437), never from the review workspace directly: after a
// runtime commit a workspace directory is not a content authority, because
// Git does not re-hash a loose object it reads and a process that outlived
// its invocation could swap the bytes behind a recorded name. Both commits
// are independently fetched into the store - base is not assumed to be an
// ancestor head's own fetch would carry along - and each fetch re-hashes
// every object it receives, refusing any that does not match its name.
//
// A failure here is returned, never swallowed into an empty, unflagged diff
// (#233 B3): a review packet must never silently claim "no changes" when the
// actual fact is "the change could not be verified".
func verifiedReviewDiff(workspaceDir, base, head string) (diff string, truncated bool, err error) {
	if strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return "", false, fmt.Errorf("a review diff requires both the base and head commits")
	}
	if _, err := subjectStore(workspaceDir, base); err != nil {
		return "", false, fmt.Errorf("verifying base %s: %w", short12(base), err)
	}
	store, err := subjectStore(workspaceDir, head)
	if err != nil {
		return "", false, fmt.Errorf("verifying head %s: %w", short12(head), err)
	}
	out, err := gitOutput(store, "diff", base+".."+head)
	if err != nil {
		return "", false, fmt.Errorf("computing the verified diff: %w", err)
	}
	if len(out) <= maxUpstreamDiffBytes {
		return out, false, nil
	}
	return boundedTo(out, maxUpstreamDiffBytes), true, nil
}
