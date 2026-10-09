package runtime

// #474: automatic independent-review-to-remediation routing.
//
// runtime/review_port.go establishes the boundary this file sits exactly on:
// #233 owns the independent review operation, its exact-head binding and its
// durable Decision; #474 owns deciding whether and how a REQUEST_CHANGES
// Decision becomes an authorized producer remediation invocation. This file
// is that one authorization gate - nothing else here schedules, dispatches or
// retries anything. See docs/review-remediation.md for the admission
// contract this implements and for the exact, still-pending reconciler wiring
// that turns an admission into a scheduled invocation.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// reviewRemediationAdmissionSchemaVersion versions the durable
// ReviewRemediationAdmission document.
const reviewRemediationAdmissionSchemaVersion = "0.1"

// ReviewRemediationAdmission is the durable, one-shot authorization fact that
// routes one independent review's REQUEST_CHANGES Decision (#233) into its
// producer's own bounded remediation invocation. Decision.Verdict alone is
// independent evidence, never permission (docs/review.md, "The #474
// interface"): AdmitReviewRemediation is the one place that evidence becomes
// an authorized remediation trigger, and it is written at most once per
// DecisionID - a second admission attempt for an already-admitted decision
// returns the existing admission rather than minting a second remediation
// budget envelope or a second provider invocation.
type ReviewRemediationAdmission struct {
	SchemaVersion string `json:"schema_version"`
	DecisionID    string `json:"decision_id"`
	RunID         string `json:"run_id"`
	Repository    string `json:"repository"`
	PRNumber      int    `json:"pr_number"`
	HeadSHA       string `json:"head_sha"`
	// FindingSignatures are the admitted decision's blocking findings'
	// signatures (review.Finding), recorded so an operator can read what
	// remediation was authorized to fix without re-fetching the decision.
	FindingSignatures []string  `json:"finding_signatures"`
	AdmittedAt        time.Time `json:"admitted_at"`
}

func (a ReviewRemediationAdmission) validate() error {
	if a.SchemaVersion != reviewRemediationAdmissionSchemaVersion {
		return fmt.Errorf("review remediation admission schema version %q is not %q", a.SchemaVersion, reviewRemediationAdmissionSchemaVersion)
	}
	if strings.TrimSpace(a.DecisionID) == "" {
		return errors.New("a review remediation admission requires the decision it admits")
	}
	if strings.TrimSpace(a.RunID) == "" {
		return errors.New("a review remediation admission requires the producer run it remediates")
	}
	if strings.TrimSpace(a.Repository) == "" || a.PRNumber <= 0 || strings.TrimSpace(a.HeadSHA) == "" {
		return errors.New("a review remediation admission requires its exact subject")
	}
	if len(a.FindingSignatures) == 0 {
		return errors.New("a review remediation admission names no blocking finding to remediate")
	}
	if a.AdmittedAt.IsZero() {
		return errors.New("a review remediation admission requires its admission time")
	}
	return nil
}

// ReviewRemediationRefusalReason names exactly why AdmitReviewRemediation
// refused, the same fail-closed, named-reason shape runtime/feedback.go
// already uses for its own admission refusals.
type ReviewRemediationRefusalReason string

const (
	ReviewRemediationRefusedNoDecision       ReviewRemediationRefusalReason = "no_independent_decision"
	ReviewRemediationRefusedNotBlocking      ReviewRemediationRefusalReason = "decision_not_blocking"
	ReviewRemediationRefusedStaleSubject     ReviewRemediationRefusalReason = "subject_stale"
	ReviewRemediationRefusedNotIndependent   ReviewRemediationRefusalReason = "reviewer_not_independent"
	ReviewRemediationRefusedSubjectMismatch  ReviewRemediationRefusalReason = "subject_mismatch"
	ReviewRemediationRefusedProducerMismatch ReviewRemediationRefusalReason = "producer_mismatch"
	ReviewRemediationRefusedUnknownRun       ReviewRemediationRefusalReason = "run_unknown"
	ReviewRemediationRefusedRunTerminal      ReviewRemediationRefusalReason = "run_terminal"
	ReviewRemediationRefusedCandidateMoved   ReviewRemediationRefusalReason = "candidate_superseded"
	ReviewRemediationRefusedNoContract       ReviewRemediationRefusalReason = "no_compiled_contract"
)

// reviewRemediationRaceTestHook runs, if set, immediately after the staleness
// check and before the re-confirmation read that follows it. It exists ONLY
// so a test can deterministically force the exact interleaving a second,
// newer decision admitted in that narrow window requires - without which
// that race cannot be reproduced on demand. Nil (and therefore free) outside
// that one test.
var reviewRemediationRaceTestHook func()

// ReviewRemediationRefusedError reports that an independently reached
// REQUEST_CHANGES decision exists but AdmitReviewRemediation refuses to treat
// it as remediation authorization. It is never a transient failure: the exact
// same (repo, PR, reviewer) call refuses again until the named condition
// itself changes (a fresh review, a restored current head, and so on).
type ReviewRemediationRefusedError struct {
	Reason ReviewRemediationRefusalReason
	Detail string
}

func (e *ReviewRemediationRefusedError) Error() string {
	return fmt.Sprintf("review remediation refused (%s): %s", e.Reason, e.Detail)
}

// AdmitReviewRemediation is #474's one authorization gate between an
// independently reached review.Decision (#233) and producer remediation. It
// resolves the decision ITSELF from the durable review store through
// ReviewPort.LatestDecision/IsStale - never from a caller-supplied copy, so a
// forged or stale Decision-shaped argument has no channel into this gate at
// all - and fails closed on every condition docs/review.md and #474 require
// before that fact may become an authorized remediation trigger:
//
//  1. a decision exists at all for this exact (repo, PR);
//  2. it is REQUEST_CHANGES, not APPROVE or COMMENT_ONLY;
//  3. producer and reviewer are not the same identity (defense in depth: see
//     below);
//  4. it is not stale against the PR's current head, AND it is still the
//     latest decision at the moment this gate is about to act on it (closes
//     the TOCTOU window between the staleness read and this read: a newer
//     decision admitted in between must never let the OLD one through merely
//     because staleness happened to be evaluated against the newer one);
//  5. its Subject names exactly the requested repository and PR, re-checked
//     against the document itself rather than trusted from the query that
//     found it;
//  6. the producing run can be loaded, is not terminal - checked both as
//     already-concluded (the persisted snapshot) and as about-to-become-
//     terminal-but-not-yet-journalled (a fresh runState.conditions() call,
//     which catches budget exhaustion a later tick has not recorded yet) -
//     and its own Repository and AgentID match the decision's claimed
//     subject and producer - never trusted from the decision document alone;
//  7. the run has a compiled contract;
//  8. the run's CURRENT candidate is still the exact head the decision was
//     reached against.
//
// On success it is recorded exactly once, by DecisionID, in the cross-run
// review_remediation_admissions table (global uniqueness: no decision is
// ever admitted twice, from any caller, any process - see
// SQLiteOperationStore.CreateReviewRemediationAdmission). That single durable
// write IS the complete authorization: nothing else has to happen for it to
// become visible, because pendingReviewRemediationKeys reads this same table
// directly rather than a second, separately-written journal event - see that
// function's own comment for why a second write was tried and removed.
//
// What this function deliberately does NOT do: dispatch a provider, consume a
// budget, or touch the run's scheduling state. Admission is a fact; turning
// that fact into a scheduled invocation is bindExecutionInvoke's job, exactly
// as it already is for remediation|... and feedback|... bindings - see
// docs/review-remediation.md for the exact, still-pending wiring this
// deliberately stops short of. The existing scheduler's AcquireOperation is
// what re-validates terminal/paused run state, transactionally, at the actual
// moment an invocation would start; this gate's own freshness check (6) is a
// best-effort reduction of a stale-authorization window, never a substitute
// for that final, authoritative gate.
func (r *EngineeringRuntime) AdmitReviewRemediation(ctx context.Context, port ReviewPort, repo GitHubRepo, prNumber int) (ReviewRemediationAdmission, bool, error) {
	decision, found, err := port.LatestDecision(repo, prNumber)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if !found {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedNoDecision,
			Detail: fmt.Sprintf("no independent review decision exists yet for %s#%d", repo, prNumber),
		}
	}
	if decision.Verdict != review.VerdictRequestChanges {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedNotBlocking,
			Detail: fmt.Sprintf("the latest decision for %s#%d is %q, not %q", repo, prNumber, decision.Verdict, review.VerdictRequestChanges),
		}
	}
	// Defense in depth: #233 refuses to construct a Decision at all when the
	// producer and reviewer identities collapse (CheckReviewIndependence,
	// called before the reviewer is ever dispatched). This gate re-checks the
	// two identities the stored Decision itself recorded anyway, so a
	// corrupted or hand-crafted row in the durable store can never buy
	// remediation authorization merely by existing.
	if decision.ProducerAgentID != "" && decision.ProducerAgentID == decision.ReviewerAgentID {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedNotIndependent,
			Detail: fmt.Sprintf("decision %s names the same agent %q as both producer and reviewer", decision.ID, decision.ProducerAgentID),
		}
	}
	if decision.Subject.Repository != repo.String() || decision.Subject.PRNumber != prNumber {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedSubjectMismatch,
			Detail: fmt.Sprintf("decision %s's own subject names %s#%d, not the requested %s#%d",
				decision.ID, decision.Subject.Repository, decision.Subject.PRNumber, repo, prNumber),
		}
	}
	stale, err := port.IsStale(ctx, repo, prNumber)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if stale {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedStaleSubject,
			Detail: fmt.Sprintf("decision %s is bound to %s, which is no longer %s#%d's current head", decision.ID, short12(decision.Subject.HeadSHA), repo, prNumber),
		}
	}
	if reviewRemediationRaceTestHook != nil {
		reviewRemediationRaceTestHook()
	}
	// IsStale's own LatestDecision read above may have evaluated a DIFFERENT,
	// newer decision than the one fetched at the top of this function, if one
	// was admitted in the narrow window between the two reads - in which case
	// "not stale" above is true of that newer decision, never of this one.
	// Re-confirming identity here closes that window without a second GitHub
	// round trip: nothing changed if, and only if, the latest decision for
	// this subject is still exactly the one being authorized.
	reconfirm, found, err := port.LatestDecision(repo, prNumber)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if !found || reconfirm.ID != decision.ID {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedStaleSubject,
			Detail: fmt.Sprintf("a newer independent decision now exists for %s#%d, superseding %s before it could be admitted", repo, prNumber, decision.ID),
		}
	}

	// The decision is independent evidence; the run it names is the
	// authoritative place every remaining check is evaluated against. The
	// decision document is never trusted for anything beyond its own
	// identity and verdict.
	state, err := r.load(decision.RunID)
	if err != nil {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedUnknownRun,
			Detail: fmt.Sprintf("decision %s names run %q, which could not be loaded: %v", decision.ID, decision.RunID, err),
		}
	}
	if state.run.Repository != repo.String() {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedSubjectMismatch,
			Detail: fmt.Sprintf("decision %s names run %q, whose own repository %q disagrees with the requested %s", decision.ID, decision.RunID, state.run.Repository, repo),
		}
	}
	// Matching repository and agent is not enough: two different pull
	// requests at the same repository, produced by the same agent, can share
	// an exact head SHA (a cherry-pick, a shared base, a coincidence). The
	// run's OWN runtime-recorded publication - never the decision's claim -
	// is what proves this run actually published THIS PR number, not merely
	// some PR at the same commit.
	if state.projection.PullRequest == nil || state.projection.PullRequest.Number != prNumber {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedSubjectMismatch,
			Detail: fmt.Sprintf("decision %s names run %s, but that run's own runtime-recorded pull request is not %s#%d", decision.ID, decision.RunID, repo, prNumber),
		}
	}
	if decision.ProducerAgentID == "" || state.run.AgentID != decision.ProducerAgentID {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedProducerMismatch,
			Detail: fmt.Sprintf("decision %s claims producer %q, but run %s's actual agent is %q", decision.ID, decision.ProducerAgentID, decision.RunID, state.run.AgentID),
		}
	}
	// Two different staleness directions, both checked: the PERSISTED
	// snapshot (state.snapshot.Disposition) is how an ALREADY-concluded run -
	// merged, explicitly failed, operator-cancelled - is read back, folded
	// from the events that actually recorded it; conditions() is the
	// opposite direction, re-deriving FRESH what the run's disposition
	// would be right now, which catches a run whose budget exhausted (or
	// otherwise became terminal) since the last tick but whose next
	// reconciliation pass has not yet run to journal that fact. Neither
	// alone is sufficient: conditions() does not re-derive an
	// already-concluded Completed/Failed from the snapshot (that is its
	// OWN output, not its input), and the snapshot alone cannot see an
	// exhaustion that has not been journalled yet.
	if terminalDisposition(state.snapshot.Disposition) {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedRunTerminal,
			Detail: fmt.Sprintf("run %s is %s, and a terminal run is never eligible for another invocation", decision.RunID, state.snapshot.Disposition),
		}
	}
	if disposition, reason := state.conditions(); terminalDisposition(disposition) {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedRunTerminal,
			Detail: fmt.Sprintf("run %s is about to become %s (%s), even though its persisted disposition has not caught up yet, and is never eligible for another invocation", decision.RunID, disposition, reason),
		}
	}
	if state.projection.Contract == (Ref{}) {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedNoContract,
			Detail: fmt.Sprintf("run %s has no compiled contract yet", decision.RunID),
		}
	}
	if state.projection.CandidateRevision != decision.Subject.HeadSHA {
		return ReviewRemediationAdmission{}, false, &ReviewRemediationRefusedError{
			Reason: ReviewRemediationRefusedCandidateMoved,
			Detail: fmt.Sprintf("run %s's current candidate %s no longer matches the decision's exact reviewed head %s",
				decision.RunID, short12(state.projection.CandidateRevision), short12(decision.Subject.HeadSHA)),
		}
	}

	signatures := make([]string, 0, len(decision.Findings))
	for _, finding := range decision.Findings {
		if finding.Severity == review.SeverityBlocking {
			signatures = append(signatures, finding.Signature)
		}
	}
	sort.Strings(signatures)

	admission := ReviewRemediationAdmission{
		SchemaVersion:     reviewRemediationAdmissionSchemaVersion,
		DecisionID:        decision.ID,
		RunID:             decision.RunID,
		Repository:        repo.String(),
		PRNumber:          prNumber,
		HeadSHA:           decision.Subject.HeadSHA,
		FindingSignatures: signatures,
		AdmittedAt:        clockNow(r.deps.Clock),
	}
	if err := admission.validate(); err != nil {
		return ReviewRemediationAdmission{}, false, err
	}

	stored, created, err := r.deps.Store.CreateReviewRemediationAdmission(admission)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	return stored, created, nil
}

// --- Deferred reconciler wiring -------------------------------------------
//
// Everything below is isolated #474 core: real runState methods, fully
// testable against a real loaded run, but NOT YET CALLED from
// runtime/reconciler.go's bindExecutionInvoke. runtime/reconciler.go is
// PR #546's active file (#475); wiring these in is a two-line addition to
// that function once #546 reaches a stable head - see
// docs/review-remediation.md for the exact diff. Neither method mutates
// anything. unresolvedReviewRemediationBinding is a pure read over the run's
// already-loaded operations; pendingReviewRemediationKeys reads the durable
// admission table (see its own comment for why), so its signature returns an
// error a future bindExecutionInvoke branch must decide how to treat - the
// documented wiring treats a read failure as "nothing pending this tick",
// never as a reason to fail the run, since the underlying authorization
// remains durable and un-lost regardless.

// unresolvedReviewRemediationBinding re-finds an EXISTING, not-yet-succeeded
// execution.invoke binding for an admitted review remediation on head, the
// same re-finding priority rule unresolvedFeedbackBinding uses and for the
// same reason (#376): a fresh binding derived purely from what is still
// "pending" would stop proposing this operation again the moment an attempt
// under it failed.
func (s *runState) unresolvedReviewRemediationBinding(head string) (string, bool) {
	prefix := reviewRemediationBindingPrefix + head + "|"
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke || op.State == Succeeded {
			continue
		}
		if binding := bindingOf(op); strings.HasPrefix(binding, prefix) {
			return binding, true
		}
	}
	return "", false
}

// reviewRemediationBindingPrefix marks an execution binding as remediating an
// admitted independent-review BLOCK, mirroring the "remediation|" and
// "feedback|" binding families bindExecutionInvoke already derives.
const reviewRemediationBindingPrefix = "review-remediation|"

// pendingReviewRemediationKeys is every admitted review-remediation
// DecisionID bound to the CURRENT head for this run. It reads
// review_remediation_admissions directly - the SAME table, and the same
// single write, AdmitReviewRemediation's durable authorization commits to -
// rather than a second, separately-written journal event.
//
// An earlier version of this method folded from a dedicated
// EventReviewRemediationAdmitted journal event instead, written as a second
// store call right after the admission row committed. That shape had a
// crash window: if the process stopped (or the second write failed) between
// the two writes, the admission row was durable but no event existed to
// fold, and because a repeat AdmitReviewRemediation call finds the existing
// row (created=false) and never re-attempts the journal write, the gap was
// permanent, not merely delayed. Reading the one authoritative table
// directly removes the second write - and the gap - entirely: the row IS
// the fact, immediately and durably visible the instant it commits, restart
// or no restart.
func (s *runState) pendingReviewRemediationKeys() ([]string, error) {
	admissions, err := s.rt.deps.Store.ReviewRemediationAdmissionsForRun(s.run.ID)
	if err != nil {
		return nil, err
	}
	head := s.projection.Head()
	keys := make([]string, 0, len(admissions))
	for _, admission := range admissions {
		if admission.HeadSHA != head {
			continue
		}
		// Re-establishes every binding invariant - never trusts that a row
		// existing under this run's own id means it is actually good for
		// it. CreateReviewRemediationAdmission already checks this at write
		// time; re-checking here is what makes a row written through ANY
		// OTHER path - a direct store call, for instance - unable to become
		// an executable binding merely by existing in this table.
		decision, found, err := s.rt.deps.Store.ReviewDecision(admission.DecisionID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if reviewRemediationBindingInvariants(decision, admission, s.run, s.projection) != nil {
			continue
		}
		keys = append(keys, admission.DecisionID)
	}
	sort.Strings(keys)
	return keys, nil
}
