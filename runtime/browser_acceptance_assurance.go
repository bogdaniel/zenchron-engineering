package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// withBrowserAcceptance runs the browser-driving tests in the sandbox AFTER the
// Go suite passed, and records the typed check that is the only thing able to
// discharge BrowserAcceptanceClaimID. It returns the suite's result extended by
// the browser run; it never turns a failed suite into a pass.
//
// A judged failure is a RESULT, as a failing test is. A run that cannot
// conclude - no browser in the image, a skipped or silent test, an unavailable
// sandbox - is an error with no passing record, so it can never become
// evidence, and it spends the same assurance attempts every other
// verification does.
func (v BaselineGoVerifier) withBrowserAcceptance(ctx context.Context, request AssuranceRequest, result AssuranceResult) (AssuranceResult, error) {
	args := dockerBase(request.CheckoutDir, true)
	args = append(args, goModuleCacheMount(v.DependencyCacheDir), "--workdir", "/candidate")
	args = append(args, envArgs(append([]string{
		"GOMODCACHE=/cache", "GONOSUMDB=*", "TMPDIR=" + sandboxBuildDir, "ZENCHRON_BROWSER_E2E=1",
		"ZENCHRON_PLAYWRIGHT_MODULE=" + browserPlaywrightPath, "ZENCHRON_CHROME_EXECUTABLE=" + browserChromePath,
	}, sandboxGoEnv...)...)...)
	args = append(args, v.Sandbox.Image, "sh", "-ec", browserAcceptanceCommand())
	sandbox := v.Sandbox
	sandbox.OperationID = "browser-assurance-" + request.RunID + "-" + request.Commit
	if sandbox.StateDir == "" {
		sandbox.StateDir = v.ArtifactStore.Root + "/docker-operations"
	}
	out, runErr := sandbox.run(ctx, args)

	var exited *containerExitError
	ran := runErr == nil || errors.As(runErr, &exited)
	exitCode := 0
	if exited != nil {
		exitCode = exited.Code
	}
	attempt := request.AttemptRef()
	artifacts, err := v.ArtifactStore.StoreExecutionAttemptTranscript(browserProviderID, attempt, out.Stdout, out.Stderr)
	if err != nil {
		return AssuranceResult{}, err
	}
	artifactRef, err := attemptTranscriptPrefix(browserProviderID, attempt)
	if err != nil {
		return AssuranceResult{}, err
	}
	signature, err := AssuranceFailureSignature(redactTranscript(append(append([]byte{}, out.Stdout...), out.Stderr...)))
	if err != nil {
		return AssuranceResult{}, err
	}
	if tree, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD^{tree}"); err != nil || strings.TrimSpace(tree) != request.Tree {
		return AssuranceResult{}, fmt.Errorf("verifier input changed during browser acceptance")
	}

	outcome, counts := classifyBrowserRun(out.Stdout, exitCode, ran && ctx.Err() == nil)
	result.Artifacts = append(result.Artifacts, artifacts...)
	environment := "docker-network-none@" + v.Sandbox.Image
	switch {
	case outcome == browserPassed:
		result.Checks = append(result.Checks, browserCheck(request, CheckOutcomePassed, counts, environment, artifactRef))
		return result, nil
	case outcome == browserFailed:
		result.Passed, result.FailureClass = false, FailureVerification
		result.ArtifactRef, result.FailureSignature = artifactRef, signature
		result.Checks = append(result.Checks, browserCheck(request, CheckOutcomeFailed, counts, environment, artifactRef))
		return result, nil
	}
	// Inconclusive: no record at all, so nothing can read this as a pass.
	result.Passed, result.ArtifactRef, result.FailureSignature = false, artifactRef, signature
	switch {
	case ctx.Err() != nil:
		result.FailureClass = execution.CancellationClass(context.Cause(ctx))
	case ran:
		result.FailureClass = FailureAssurancePrerequisite
	default:
		result.FailureClass = FailureTransientInfrastructure
	}
	return result, fmt.Errorf("browser acceptance was inconclusive (%d of %d selected tests passed, %d skipped): %w",
		counts.Passed, counts.Total, counts.Skipped, errors.Join(runErr, ctx.Err()))
}

// browserCheck is the record of one completed browser run for this request's
// exact candidate.
func browserCheck(request AssuranceRequest, outcome string, counts CheckResults, environment, artifact string) VerificationCheck {
	return VerificationCheck{
		Kind: CheckKindBrowserAcceptance, Checker: browserCheckerID, CheckerRevision: browserCheckerRevision(),
		Outcome: outcome, Commit: request.Commit, Tree: request.Tree,
		Selection: browserSelectionDigest(), Results: &counts, Environment: environment, Artifact: artifact,
	}
}

// browserCheckProves reports whether the observation PROVES the browser claim
// for this exact candidate: a completed, passing record by the current checker
// and selection, bound to the observation's own commit and tree, in which every
// selected test passed and none failed or skipped. An observation with no
// record - every one written before the requirement existed, and every one a
// generic suite produced - proves nothing about a browser.
func browserCheckProves(observation *AssuranceObservation, commit string) bool {
	if observation == nil || commit == "" {
		return false
	}
	for _, check := range observation.Checks {
		results := check.Results
		if check.Kind == CheckKindBrowserAcceptance && check.Outcome == CheckOutcomePassed &&
			check.Checker == browserCheckerID && check.CheckerRevision == browserCheckerRevision() &&
			check.Selection == browserSelectionDigest() && check.Environment != "" && check.Artifact != "" &&
			check.Commit == commit && check.Commit == observation.Commit && check.Tree != "" && check.Tree == observation.Tree &&
			results != nil && results.Total > 0 && results.Passed == results.Total && results.Failed == 0 && results.Skipped == 0 {
			return true
		}
	}
	return false
}

// browserEvidenceItem turns the generic passing-assurance item for the browser
// claim into the evidence that actually proves it, or reports that none can be
// produced. The producer and environment name the browser check that ran, not
// the Go suite that ran before it.
func browserEvidenceItem(item domain.EvidenceItem, observation *AssuranceObservation, commit string) (domain.EvidenceItem, bool) {
	if !browserCheckProves(observation, commit) {
		return domain.EvidenceItem{}, false
	}
	for _, check := range observation.Checks {
		if check.Kind == CheckKindBrowserAcceptance && check.Outcome == CheckOutcomePassed {
			item.Producer = domain.EvidenceProducer{ID: browserCheckerID, Type: domain.ProducerDeterministicTool}
			item.Environment = domain.EvidenceEnvironment{Type: "assurance_provider", Identifier: check.Environment}
			return item, true
		}
	}
	return domain.EvidenceItem{}, false
}

// browserBindingSuffix extends the assurance operation binding when, and only
// when, the contract requires the browser claim, exactly as the file-size
// suffix does: a newly required check is planned instead of stranded, a
// changed checker or selection makes the check eligible again, and a contract
// that does not require it keeps the binding it always had.
func (s *runState) browserBindingSuffix() string {
	if s.rt == nil {
		return ""
	}
	return browserBindingSuffixFor(s.rt.deps.Policy, func() (domain.EngineeringWorkContract, error) { return s.rt.contractFor(s) })
}

// browserBindingSuffixFor is the pure decision. A contract that cannot be
// rebuilt leaves the binding as it was; discharging the claim is refused
// independently of planning.
func browserBindingSuffixFor(policy domain.EngineeringPolicy, rebuild func() (domain.EngineeringWorkContract, error)) string {
	if !policyListsClaim(policy, BrowserAcceptanceClaimID) {
		return ""
	}
	contract, err := rebuild()
	if err != nil || !browserAcceptanceRequired(contract) {
		return ""
	}
	return "|browser-acceptance@" + browserCheckerRevision()
}
