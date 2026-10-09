package runtime

// #474: delivering an admitted independent-review BLOCK to the producer's
// own remediation invocation. This is the read side bindExecutionInvoke's
// review-remediation|... binding exists to feed: once that binding is
// scheduled, invokeExecution calls reviewRemediationFindings to turn the
// admitted, re-validated findings into the typed Finding values a
// remediation invocation carries - the same shape feedbackFindings already
// produces for admitted GitHub feedback.

import (
	"github.com/bogdaniel/zenchron-engineering/review"
)

// reviewRemediationFindings re-reads every admission bound to the run's
// current head, re-validates each via reviewRemediationBindingInvariants -
// never trusting that bindExecutionInvoke's own earlier read is still
// current - and renders the referenced decision's actual blocking findings
// as typed, bounded Finding values: Signature carries the reviewer's own
// bounded dedup key (the trusted half, printed directly - the same
// treatment stage-review findings already get), and Diagnostic carries the
// reviewer's free-form Detail text, which reaches the provider only inside
// verifierEvidenceEnvelope's existing UNTRUSTED-SOURCE markers and is never
// read as instruction.
//
// A store read failure is returned as an error, never swallowed into "no
// findings": an admitted BLOCK this call cannot currently prove must never
// look identical to one that was never admitted at all.
func (r *EngineeringRuntime) reviewRemediationFindings(state *runState) ([]Finding, error) {
	admissions, err := r.deps.Store.ReviewRemediationAdmissionsForRun(state.run.ID)
	if err != nil {
		return nil, err
	}
	head := state.projection.Head()
	var findings []Finding
	for _, admission := range admissions {
		if admission.HeadSHA != head {
			continue
		}
		decision, found, err := r.deps.Store.ReviewDecision(admission.DecisionID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if reviewRemediationBindingInvariants(decision, admission, state.run, state.projection) != nil {
			continue
		}
		for _, finding := range decision.Findings {
			if finding.Severity != review.SeverityBlocking {
				continue
			}
			findings = append(findings, Finding{
				Classification: FailureVerification,
				Verifier:       "independent-review:" + boundedDetail(decision.ReviewerAgentID),
				Signature:      "review-remediation:" + boundedDetail(finding.Signature),
				Diagnostic:     boundedDetail(finding.Detail),
			})
		}
	}
	return findings, nil
}
