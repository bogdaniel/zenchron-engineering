package runtime

// #474: delivering an admitted independent-review BLOCK to the producer's
// own remediation invocation. This is the read side bindExecutionInvoke's
// review-remediation|... binding exists to feed: once that binding is
// scheduled, invokeExecution calls reviewRemediationFindings to turn the
// admitted, re-validated findings into the typed Finding values a
// remediation invocation carries - the same shape feedbackFindings already
// produces for admitted GitHub feedback.

import (
	"context"
	"fmt"

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

// reviewRemediationLiveHeadCheck re-confirms, by a live GitHub read rather
// than the run's own last-observed projection, that the pull request's
// ACTUAL current head still agrees with the head reviewFindings was just
// assembled against (#474 B2). reviewRemediationFindings and
// bindExecutionInvoke's binding both read only journalled state refreshed by
// whenever observeGitHub last ran; this is the one point, immediately before
// the provider is launched, that asks GitHub itself. An external PR move
// between admission and this call, or GitHub being unreachable, refuses THIS
// attempt rather than deliver findings whose subject may already be gone -
// the run retries under its own existing bounded attempt ceiling, the same
// as any other transient dispatch failure.
//
// The external read and the provider actually starting can never be made
// atomic from here - a move landing in that residual gap is irreducible from
// inside this process - but the gap this closes is the one between
// admission and dispatch that previously had no live check at all.
func (r *EngineeringRuntime) reviewRemediationLiveHeadCheck(ctx context.Context, state *runState, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	pr := state.projection.PullRequest
	if pr == nil {
		return fmt.Errorf("review-remediation findings were assembled with no published pull request to recheck live")
	}
	repo, err := parseGitHubRepo(state.run.Repository)
	if err != nil {
		return err
	}
	live, err := r.deps.GitHub.PullRequest(ctx, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("live pull request head recheck failed: %w", err)
	}
	if live.HeadSHA != state.projection.Head() {
		return fmt.Errorf("the pull request's live head %s disagrees with the head %s the admitted review-remediation findings were assembled against",
			short12(live.HeadSHA), short12(state.projection.Head()))
	}
	return nil
}
