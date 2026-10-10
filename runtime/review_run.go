package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// reviewClaimStaleAfter bounds how long a crashed claimant can strand a
// review or publication claim (#233 B4). It is generous enough to cover one
// ordinary bounded reviewer invocation plus publication, and finite so a
// genuinely abandoned claim is always eventually reclaimable rather than
// stuck forever - the same shape the review-continuation wall allowance
// (docs/github-feedback.md) already uses for a related bound.
const reviewClaimStaleAfter = 30 * time.Minute

// ReviewClaimConflictError reports that another caller already owns the
// exclusive claim over this exact review or publication operation (#233 B4).
// It is never a correctness failure: the caller that lost the race should
// simply retry once the winner finishes, which - on success - releases the
// claim immediately, and - on a crash - is reclaimed after reviewClaimStaleAfter.
type ReviewClaimConflictError struct{ ClaimKey string }

func (e *ReviewClaimConflictError) Error() string {
	return fmt.Sprintf("another caller is already performing %q; retry once it completes", e.ClaimKey)
}

// ReviewClaimLostError reports that this caller's exclusive claim over a
// review or publication was observed to be evicted - renewed by another
// owner - WHILE this caller still had work in flight under it (#233 B4-A).
// Unlike ReviewClaimConflictError, which refuses before any work starts,
// this is raised after an invocation already ran: its result is refused
// rather than durably admitted, because this caller can no longer prove it
// is still the one authorized to decide for this key.
type ReviewClaimLostError struct{ ClaimKey string }

func (e *ReviewClaimLostError) Error() string {
	return fmt.Sprintf("the exclusive claim over %q was lost to another caller before this invocation's result could be admitted", e.ClaimKey)
}

// clockNow is Clock's nil-safe default: a nil Clock (the ordinary case
// outside tests) reads the real wall clock.
func clockNow(clock Clock) time.Time {
	if clock == nil {
		return time.Now().UTC()
	}
	return clock.Now()
}

// reviewClaimRenewalInterval is how often a still-active holder of a review
// or publication claim renews it during the one potentially long-running step
// under that claim (the reviewer invocation, or the external GitHub calls a
// publication makes) so a merely-slow-but-alive operation is never stolen out
// from under it (#233 B4-2, B4-B). A third of the staleness bound leaves two
// missed renewals of margin before a genuinely abandoned claim is reclaimed.
//
// A var, not a const: a test that must prove the renewal itself defeats a
// staleness-window steal - without actually waiting out reviewClaimStaleAfter
// in real time - shrinks this for its own duration and restores it after.
var reviewClaimRenewalInterval = reviewClaimStaleAfter / 3

// renewReviewClaimWhile renews claimKey/token every reviewClaimRenewalInterval
// until stop is closed, or until a renewal confirms the claim is no longer
// held by token - at which point it sets lost and stops renewing rather than
// continuing to tick pointlessly (#233 B4-A). lost is this loss's ONE
// observable signal: the caller checks it before admitting anything reached
// while this goroutine was running, so a lease lost mid-invocation is
// discovered and refused rather than silently treated as still held. A
// transient store error alone does not set lost - only a renewal that
// EXECUTED and reported renewed=false is proof the claim moved to another
// owner, and that is retried on the next tick instead.
func renewReviewClaimWhile(store *SQLiteOperationStore, claimKey, token string, clock Clock, stop <-chan struct{}, lost *atomic.Bool) {
	ticker := time.NewTicker(reviewClaimRenewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			renewed, err := store.RenewReviewClaim(claimKey, token, clockNow(clock))
			if reviewClaimRenewedTestHook != nil {
				reviewClaimRenewedTestHook()
			}
			if err == nil && !renewed {
				lost.Store(true)
				return
			}
		case <-stop:
			return
		}
	}
}

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
	//
	// Remote and Credentials are the fallback (#474 B3) for a caller with no
	// such checkout - a long-running supervisor governing several
	// repositories has no single operator cwd to assume. reviewWorkspace
	// clones from Remote, through the SAME governed, credential-bound Git
	// boundary a candidate clone already uses (CreatePlanningWorkspaceFromRemote),
	// when Source is empty. Exactly one of the two must be supplied.
	Source      string
	Remote      RemoteIdentity
	Credentials CredentialProvider
	Clock       Clock

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

// reviewClaimTestHook runs, if set, immediately after RunIndependentReview
// observes no existing decision and immediately before it attempts the
// review claim. It exists ONLY so a test can construct the exact interleaving
// B4-1's regression proves is now handled - a second caller's pre-claim read
// genuinely racing the first caller's entire claimed section - without which
// that interleaving cannot be forced deterministically. Nil (and therefore
// free) outside that one test.
var reviewClaimTestHook func()

// reviewClaimRenewedTestHook runs, if set, immediately after every renewal
// attempt renewReviewClaimWhile makes (success, no-op, or failure). It exists
// ONLY so a test can deterministically wait for "a renewal has just executed
// against the current clock" - needed to prove #233 B4-B's publish-claim
// renewal defeats a staleness-window steal without actually waiting out
// reviewClaimStaleAfter in real time - instead of sleeping and hoping a
// shortened ticker has fired. Nil (and therefore free) outside that one test.
var reviewClaimRenewedTestHook func()

// reviewWorkspace materializes the exact PR head either from a local
// checkout (Source) or, when none is supplied, from the governed remote
// (#474 B3) - the same dual path CreatePlanningWorkspace and
// CreatePlanningWorkspaceFromRemote already offer planning.
// errReviewWorkspaceSource is reviewWorkspace's explicit, fail-fast refusal
// when neither a local Source checkout nor a governed Remote was supplied -
// distinct from whatever a clone attempt against an empty remote would
// otherwise produce, so a caller sees exactly which boundary it missed
// rather than a cloning command's own generic failure.
var errReviewWorkspaceSource = errors.New("an independent review requires either a local Source checkout or a governed Remote to clone from")

func reviewWorkspace(in RunIndependentReviewInput, reviewID, headSHA string) (*PlanningWorkspace, error) {
	if in.Source != "" {
		return CreatePlanningWorkspace(in.StateDir, reviewID, in.Source, headSHA, "")
	}
	if in.Remote.URL == "" {
		return nil, errReviewWorkspaceSource
	}
	return CreatePlanningWorkspaceFromRemote(in.StateDir, reviewID, in.Remote, in.Credentials, headSHA)
}

// publishDecision publishes decision when requested, through the one
// exclusive entry point (PublishReview itself now claims "publish:"+ID -
// #233 B4-3) - never a second, bypassable wrapper around it.
func publishDecision(ctx context.Context, in RunIndependentReviewInput, decision review.Decision) (*review.Publication, error) {
	if !in.Publish {
		return nil, nil
	}
	publication, err := PublishReview(ctx, ReviewPublicationDeps{Store: in.Store, GitHub: in.GitHub, Clock: in.Clock}, in.Repo, decision)
	if err != nil {
		return nil, err
	}
	return &publication, nil
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
	subject := review.Subject{Repository: in.Repo.String(), PRNumber: in.PRNumber, HeadSHA: pr.HeadSHA}
	decisionID, err := review.DecisionID(subject, in.Reviewer.ID)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	existingOutcome := func(existing review.Decision) (RunIndependentReviewOutput, error) {
		out := RunIndependentReviewOutput{Decision: existing, Created: false}
		// The decision already exists, but this invocation may be exactly the
		// retry that recovers from a crash between the decision committing
		// and its publication - requirement: a restart must be able to
		// complete publication, not merely discover there was nothing left to
		// decide. PublishReview is itself idempotent and itself exclusive, so
		// this is always safe to attempt.
		publication, err := publishDecision(ctx, in, existing)
		out.Publication = publication
		return out, err
	}
	// If a decision for this EXACT subject and reviewer already exists, this
	// call is a retry (or a redundant concurrent request): return it rather
	// than performing a second independent review of work already judged.
	if existing, found, err := in.Store.ReviewDecision(decisionID); err != nil {
		return RunIndependentReviewOutput{}, err
	} else if found {
		return existingOutcome(existing)
	}
	if reviewClaimTestHook != nil {
		reviewClaimTestHook()
	}
	// Claimed BEFORE any expensive or externally-visible work (#233 B4): two
	// concurrent callers for the same exact (subject, reviewer) identity must
	// never both materialize a workspace and invoke a provider. The claim is
	// released on every return path below; a crash leaves it for
	// reviewClaimStaleAfter, never forever.
	reviewID := decisionID
	claimed, token, err := in.Store.ClaimReview(reviewID, in.ControllerID, clockNow(in.Clock), reviewClaimStaleAfter)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	if !claimed {
		return RunIndependentReviewOutput{}, &ReviewClaimConflictError{ClaimKey: reviewID}
	}
	defer in.Store.ReleaseReviewClaim(reviewID, token)

	// #233 B4-1: the read above can be stale by the time the claim is won - a
	// concurrent caller may have created the decision and released the claim
	// in between. Re-checking INSIDE the claim is what makes the two
	// checks together exclusive rather than racy: whichever caller is inside
	// the claim always sees the current truth before doing anything further.
	if existing, found, err := in.Store.ReviewDecision(decisionID); err != nil {
		return RunIndependentReviewOutput{}, err
	} else if found {
		return existingOutcome(existing)
	}

	workspace, err := reviewWorkspace(in, reviewID, pr.HeadSHA)
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

	// #233 B4-2: renewed periodically for exactly as long as the one
	// potentially long-running step runs, so a merely-slow-but-alive
	// invocation is never stolen out from under it. #233 B4-A: claimLost is
	// this renewal loop's one observable signal that ownership was lost
	// anyway (a steal that outran renewal, or a renewal failure that is
	// itself proof another owner already holds the key) - checked below
	// before anything is admitted, so a lease lost mid-invocation is
	// discovered rather than silently treated as still held.
	renewalStop := make(chan struct{})
	var claimLost atomic.Bool
	go renewReviewClaimWhile(in.Store, reviewID, token, in.Clock, renewalStop, &claimLost)
	invocation, err := InvokeReviewer(ctx, ReviewInvocationInput{
		ReviewID: reviewID, Packet: packet, Workspace: workspace,
		Agent: in.Reviewer, Provider: in.Provider, Artifacts: ArtifactStore{Root: in.StateDir},
		StateDir: in.StateDir, ControllerID: in.ControllerID, Model: in.Model,
		Instructions: in.Instructions, Budgets: in.Budgets,
	})
	close(renewalStop)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	if claimLost.Load() {
		return RunIndependentReviewOutput{}, &ReviewClaimLostError{ClaimKey: reviewID}
	}
	decision, err := AdmitPRReview(ReviewInvocationInput{
		ReviewID: reviewID, Packet: packet, Workspace: workspace, Agent: in.Reviewer,
	}, invocation, in.Clock)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	// #233 B4-A: the admission write itself is fenced on reviewID/token, not
	// only guarded by the checks above - closing the window between the
	// check and this write, which a steal could otherwise still land in.
	stored, created, err := in.Store.CreateReviewDecisionFenced(decision, reviewID, token)
	if err != nil {
		return RunIndependentReviewOutput{}, err
	}
	out := RunIndependentReviewOutput{Decision: stored, Created: created}
	publication, err := publishDecision(ctx, in, stored)
	out.Publication = publication
	if err != nil {
		return out, err
	}
	return out, nil
}
