package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReviewInvocationInput is everything one independent review invocation needs.
// It mirrors PlannerInput's shape (#233 requirement 4: review through the
// existing execution/provider seam): the caller resolves the agent, the
// provider and the workspace; this file only drives one bounded, non-mutating
// invocation and reads back its typed verdict.
type ReviewInvocationInput struct {
	ReviewID     string
	Packet       ReviewPacket
	Workspace    *PlanningWorkspace
	Agent        ResolvedAgent
	Provider     ExecutionProvider
	Artifacts    ArtifactStore
	StateDir     string
	ControllerID string
	Model        string
	Instructions []string
	Budgets      ProviderBudget
}

// ReviewInvocationOutput is the raw outcome of one invocation: the provider's
// ExecutionResult and whatever structured verdict it wrote, before AdmitPRReview
// turns it into a durable review.Decision.
type ReviewInvocationOutput struct {
	Attempt int
	Result  ExecutionResult
	Verdict *ReviewerResult
}

// ReviewWorkspaceChangedError is refused the same way PlanningWorkspaceError
// is: a provider that mutated a workspace it was given non-mutating authority
// over has broken the one guarantee this invocation depends on, so its answer
// is refused rather than parsed.
type ReviewWorkspaceChangedError struct{ Dir string }

func (e *ReviewWorkspaceChangedError) Error() string {
	return fmt.Sprintf("the review workspace at %s changed during a non-mutating independent review invocation; "+
		"the provider's restriction did not hold and its verdict is refused", e.Dir)
}

// InvokeReviewer performs one bounded, non-mutating independent review
// invocation (#233). Like InvokePlanner, verification is NEVER inferred from
// what the provider claims: the workspace digest is measured again after
// Execute returns, before the typed result is even read, and a changed
// workspace refuses the verdict outright.
func InvokeReviewer(ctx context.Context, in ReviewInvocationInput) (ReviewInvocationOutput, error) {
	if in.Workspace == nil || in.Provider == nil {
		return ReviewInvocationOutput{}, fmt.Errorf("an independent review requires a materialized workspace and a registered provider")
	}
	attempt := in.Artifacts.NextAttempt(in.Agent.ID, ExecutionAttemptRef{RunID: in.ReviewID, OperationID: reviewOperationID, Attempt: 1})
	attemptRef := ExecutionAttemptRef{RunID: in.ReviewID, OperationID: reviewOperationID, Attempt: attempt}
	resultPath, err := PrepareReviewerResult(in.StateDir, attemptRef)
	if err != nil {
		return ReviewInvocationOutput{}, err
	}
	before, err := in.Workspace.Digest()
	if err != nil {
		return ReviewInvocationOutput{}, err
	}
	request := ExecutionRequest{
		RunID: in.ReviewID, OperationID: reviewOperationID, Attempt: attempt,
		SourceSnapshot:  Ref{ID: fmt.Sprintf("pull-request-%d", in.Packet.Trusted.PRNumber), Revision: in.Packet.Trusted.HeadSHA},
		ControllerID:    in.ControllerID,
		Base:            Ref{ID: in.Packet.Trusted.BaseRef, Revision: in.Packet.Trusted.BaseSHA},
		Candidate:       Candidate{Revision: in.Workspace.Commit, Tree: in.Workspace.Tree},
		CandidateDir:    in.Workspace.Dir,
		Contract:        Ref{ID: in.Packet.Trusted.ContractID, Revision: in.Packet.Trusted.ContractRevision},
		Objective:       reviewObjective(in.Packet),
		Instructions:    in.Instructions,
		Purpose:         InvocationIndependentReview,
		Mode:            domain.InvocationModeNonMutatingPlanning,
		ModelPreference: in.Model,
		Upstream: []UpstreamContext{{
			StageID: "pull-request", RunID: in.Packet.Trusted.RunID,
			Commit: in.Packet.Trusted.HeadSHA, Tree: in.Workspace.Tree,
			Diff: in.Packet.Untrusted.Diff, Truncated: in.Packet.Untrusted.Truncated,
		}},
		ReviewerResultPath: resultPath,
		Budgets:            in.Budgets,
	}
	result, execErr := in.Provider.Execute(ctx, request)

	// VERIFICATION FIRST, exactly as InvokePlanner orders it: the workspace is
	// re-measured before anything the provider wrote - the transcript or the
	// typed result - is read.
	after, digestErr := in.Workspace.Digest()
	if digestErr != nil {
		return ReviewInvocationOutput{Attempt: attempt, Result: result}, digestErr
	}
	if before != after {
		return ReviewInvocationOutput{Attempt: attempt, Result: result}, &ReviewWorkspaceChangedError{Dir: in.Workspace.Dir}
	}
	if execErr != nil {
		return ReviewInvocationOutput{Attempt: attempt, Result: result}, execErr
	}
	if result.Failure != nil {
		return ReviewInvocationOutput{Attempt: attempt, Result: result}, fmt.Errorf("the reviewing provider reported %s", result.Failure.Classification)
	}
	verdict, err := ReadReviewerResult(resultPath)
	if err != nil {
		return ReviewInvocationOutput{Attempt: attempt, Result: result}, &ReviewerResultRefusedError{Detail: err.Error(), Protocol: true}
	}
	return ReviewInvocationOutput{Attempt: attempt, Result: result, Verdict: verdict}, nil
}

// reviewOperationID is the one operation identity every independent review
// invocation authorizes under. A review is a single bounded operation, not a
// multi-phase run, so there is exactly one id to name here - unlike an
// EngineeringRun's many distinct operation kinds.
const reviewOperationID = "independent-review"

// AdmitPRReview turns a reviewer's structured result into a durable
// review.Decision, binding it to the EXACT subject this invocation
// materialized - never to anything the result itself restates - the same
// authority-over-opinion separation AdmitReviewerResult keeps for a plan-stage
// verdict (#233 requirement 6: use the existing protocol where suitable,
// without inventing a second, contradictory one). The two-value verdict a
// reviewer writes (StageReviewAccepted/StageReviewBlocked) is unchanged; what
// is new is the three-way decision #233 itself requires, derived from that
// verdict together with each finding's severity.
func AdmitPRReview(in ReviewInvocationInput, out ReviewInvocationOutput, clock Clock) (review.Decision, error) {
	refuse := func(format string, args ...any) (review.Decision, error) {
		return review.Decision{}, &ReviewerResultRefusedError{Detail: fmt.Sprintf(format, args...)}
	}
	refuseProtocol := func(format string, args ...any) (review.Decision, error) {
		return review.Decision{}, &ReviewerResultRefusedError{Detail: fmt.Sprintf(format, args...), Protocol: true}
	}
	result := out.Verdict
	if result == nil {
		return refuse("the reviewer produced no structured result at %s", in.StateDir)
	}
	if result.SchemaVersion != ReviewerResultSchemaVersion {
		return refuseProtocol("result schema version %q is not %q", result.SchemaVersion, ReviewerResultSchemaVersion)
	}
	if result.Candidate != "" && result.Candidate != in.Workspace.Commit {
		return refuse("the result claims it reviewed %s and the invocation materialized %s", short12(result.Candidate), short12(in.Workspace.Commit))
	}
	if result.Tree != "" && result.Tree != in.Workspace.Tree {
		return refuse("the result claims tree %s and the invocation materialized %s", short12(result.Tree), short12(in.Workspace.Tree))
	}
	if len(result.Findings) > maxReviewerFindings {
		return refuseProtocol("the result names %d findings, above the %d finding bound", len(result.Findings), maxReviewerFindings)
	}
	var findings []review.Finding
	hasBlocking := false
	for _, f := range result.Findings {
		signature := boundedDetail(f.Signature)
		if signature == "" {
			return refuseProtocol("a finding states no signature, so it identifies nothing to remediate")
		}
		severity := review.SeverityBlocking
		if f.Severity == ReviewSeverityNonBlocking {
			severity = review.SeverityNonBlocking
		}
		if severity == review.SeverityBlocking {
			hasBlocking = true
		}
		findings = append(findings, review.Finding{Severity: severity, Signature: signature, Detail: boundedDetail(f.Detail)})
	}
	var verdict review.Verdict
	switch result.Verdict {
	case StageReviewAccepted:
		if hasBlocking {
			return refuseProtocol("an accepting verdict names a blocking finding: acceptance and an outstanding blocker are different answers")
		}
		if len(findings) > 0 {
			verdict = review.VerdictCommentOnly
		} else {
			verdict = review.VerdictApprove
		}
	case StageReviewBlocked:
		if !hasBlocking {
			return refuseProtocol("a blocking verdict names no blocking finding, so remediation would have nothing to be bound to")
		}
		verdict = review.VerdictRequestChanges
	default:
		return refuseProtocol("verdict %q must be %q or %q", result.Verdict, StageReviewAccepted, StageReviewBlocked)
	}
	subject := review.Subject{Repository: in.Packet.Trusted.Repository, PRNumber: in.Packet.Trusted.PRNumber, HeadSHA: in.Packet.Trusted.HeadSHA}
	id, err := review.DecisionID(subject, in.Agent.ID)
	if err != nil {
		return review.Decision{}, err
	}
	contextDigest, err := Digest(in.Packet)
	if err != nil {
		return review.Decision{}, err
	}
	now := time.Now().UTC()
	if clock != nil {
		now = clock.Now()
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject,
		BaseSHA: in.Packet.Trusted.BaseSHA, RunID: in.Packet.Trusted.RunID,
		ProducerAgentID: in.Packet.Trusted.ProducerAgentID, ReviewerAgentID: in.Agent.ID,
		ReviewerProviderKind: in.Agent.Kind, ReviewerVendorFamily: VendorFamilyFor(in.Agent.Kind),
		Verdict: verdict, Findings: findings, Reason: boundedDetail(result.Reason),
		ContextDigest: contextDigest, CreatedAt: now,
	}
	if err := decision.Validate(); err != nil {
		return review.Decision{}, err
	}
	return decision, nil
}
