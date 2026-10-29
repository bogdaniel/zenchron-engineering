package runtime

import "github.com/bogdaniel/zenchron-engineering/execution"

// typedResults is what the HOST read from the runtime-owned result slots of
// one invocation (#521). It never comes from a provider: ExecutionResult has
// no field a provider could fill with a verdict or a resolution, so the only
// way to state one is to write the file the runtime prepared and named.
type typedResults struct {
	// Review is nil when none was written, which is an ordinary outcome: a
	// reviewer that produced only prose produced no verdict. It is a CLAIM and
	// authorizes nothing until AdmitReviewerResult has checked it.
	Review *ReviewerResult
	// ReviewRefusal carries the exact decode reason when the reviewer-result
	// file existed but failed to decode, through the same typed refusal
	// AdmitReviewerResult produces, so the reason is not discarded down to a
	// bare classification (#374).
	ReviewRefusal *ReviewerResultRefusedError
	// Resolution is nil when none was written, which AdmitFeedbackResolution
	// and outstandingReviewKeys already treat as "nothing was stated" (#376).
	Resolution *FeedbackResolution
}

// readTypedResultSlots reads the slots right after Provider.Execute returns,
// and only when the invocation itself SUCCEEDED: a verdict written by an
// invocation that then died is not its answer, and a malformed one would
// otherwise overwrite the real reason it died. The transcript is evidence and
// is never consulted for a verdict.
//
// A malformed document on a successful invocation fails it, rather than being
// silently dropped: a worker that tried to answer and produced something
// unreadable has not declined to answer. A bad verdict fails as a REVIEWER
// PROTOCOL failure, never FailureVerification (#374), because nothing was
// judged; a bad resolution fails as FailureVerification. A failed verdict read
// stops before the resolution is read.
//
// The success test is deliberately Outcome and Failure, not the Execute error:
// it is exactly the point at which the CLI adapter used to read these files,
// which a deferred candidate-writer stop error could follow without undoing.
func readTypedResultSlots(reviewerResultPath, feedbackResolutionPath string, result *ExecutionResult) typedResults {
	var typed typedResults
	if result.Outcome != execution.Succeeded || result.Failure != nil {
		return typed
	}
	diagnosticRef := ""
	if len(result.Artifacts) > 0 {
		diagnosticRef = result.Artifacts[0].Path
	}
	if reviewerResultPath != "" {
		review, err := ReadReviewerResult(reviewerResultPath)
		if err != nil {
			result.Outcome = execution.Failed
			result.Failure = &ProviderFailure{
				Classification: FailureReviewerProtocolIncomplete, RawDiagnosticRef: diagnosticRef,
			}
			typed.ReviewRefusal = &ReviewerResultRefusedError{Detail: boundedDetail(err.Error())}
			return typed
		}
		typed.Review = review
	}
	if feedbackResolutionPath != "" {
		resolution, err := ReadFeedbackResolution(feedbackResolutionPath)
		if err != nil {
			result.Outcome = execution.Failed
			result.Failure = &ProviderFailure{Classification: FailureVerification, RawDiagnosticRef: diagnosticRef}
			return typed
		}
		typed.Resolution = resolution
	}
	return typed
}
