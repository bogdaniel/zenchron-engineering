package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// browserAcceptanceBase is the base revision whose browser manifest and test
// sources a contract's assurance must run, or "" when the contract does not
// require the claim. A required check with no base to read them from is refused
// by the caller, never skipped.
func browserAcceptanceBase(contract domain.EngineeringWorkContract, base string) string {
	if !browserAcceptanceRequired(contract) {
		return ""
	}
	return base
}

func browserBaseRefusal(contract domain.EngineeringWorkContract, base string) error {
	if browserAcceptanceRequired(contract) && base == "" {
		return errors.New("the contract requires browser acceptance but the run has no base revision to read its tests from; refusing to verify without it")
	}
	return nil
}

// withBrowserAcceptance runs the browser-driving tests in the sandbox AFTER the
// Go suite passed, and records the typed check that is the only thing able to
// discharge BrowserAcceptanceClaimID. It returns the suite's result extended by
// the browser run; it never turns a failed suite into a pass.
//
// The container is the suite's own isolation: no network, read-only root, all
// capabilities dropped. The candidate it sees is the exact tree with the test
// surface replaced by the base revision's (browserWorkspace), mounted
// read-only; the only writable mounts are the runtime-owned artifact directory
// and the tmpfs the base already provides.
//
// A judged failure is a RESULT, as a failing test is. A run that cannot
// conclude - no browser in the image, a skipped, silent or artifact-less test,
// an unavailable sandbox - is an error with no passing record, so it can never
// become evidence, and it spends the same assurance attempts every other
// verification does.
func (v BaselineGoVerifier) withBrowserAcceptance(ctx context.Context, request AssuranceRequest, result AssuranceResult) (AssuranceResult, error) {
	inconclusive := func(class FailureClass, err error) (AssuranceResult, error) {
		result.Passed, result.FailureClass = false, class
		return result, err
	}
	selection, err := readBrowserSelection(request.CheckoutDir, request.BrowserAcceptanceBase)
	if err != nil {
		return inconclusive(FailureAssurancePrerequisite, err)
	}
	name := request.RunID + "-" + request.Commit + "-" + strconv.Itoa(request.Attempt) + "-" + strconv.FormatBool(request.Confirmation)
	work := filepath.Join(v.ArtifactStore.Root, "browser", name)
	workspace, artifactsDir := filepath.Join(work, "candidate"), filepath.Join(work, "artifacts")
	if err := os.RemoveAll(work); err != nil {
		return AssuranceResult{}, err
	}
	if err := os.MkdirAll(artifactsDir, 0700); err != nil {
		return AssuranceResult{}, err
	}
	defer os.RemoveAll(workspace)
	surface, err := browserWorkspace(request.CheckoutDir, request.BrowserAcceptanceBase, request.Tree, workspace)
	if err != nil {
		return inconclusive(FailureAssurancePrerequisite, err)
	}

	args := dockerBase(workspace, true)
	args = append(args, goModuleCacheMount(v.DependencyCacheDir), "--mount", "type=bind,src="+artifactsDir+",dst="+browserArtifactsPath, "--workdir", "/candidate")
	env := append([]string{"GOMODCACHE=/cache", "GONOSUMDB=*", "TMPDIR=" + sandboxBuildDir}, browserEnvironment...)
	args = append(args, envArgs(append(env, sandboxGoEnv...)...)...)
	args = append(args, v.Sandbox.Image, "sh", "-c", browserAcceptanceCommand(selection))
	sandbox := v.Sandbox
	sandbox.OperationID = "browser-assurance-" + request.RunID + "-" + request.Commit
	if sandbox.StateDir == "" {
		sandbox.StateDir = filepath.Join(v.ArtifactStore.Root, "docker-operations")
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

	outcome, counts := classifyBrowserRun(selection, out.Stdout, exitCode, ran && ctx.Err() == nil)
	artifactsDigest, silent, err := browserArtifacts(artifactsDir, selection)
	if err != nil {
		return AssuranceResult{}, err
	}
	result.Artifacts = append(result.Artifacts, artifacts...)
	record := func(outcome string) VerificationCheck {
		return VerificationCheck{
			Kind: CheckKindBrowserAcceptance, Checker: browserCheckerID, CheckerRevision: browserCheckerRevision(),
			Outcome: outcome, Commit: request.Commit, Tree: request.Tree, BaseRevision: request.BrowserAcceptanceBase,
			Selection: browserSelectionDigest(selection), Surface: surface, Results: &counts,
			Environment: "docker-network-none@" + v.Sandbox.Image, Artifact: artifactRef, ArtifactsSHA256: artifactsDigest,
		}
	}
	switch {
	case outcome == browserPassed && len(silent) == 0:
		result.Checks = append(result.Checks, record(CheckOutcomePassed))
		return result, nil
	case outcome == browserFailed:
		result.Passed, result.FailureClass = false, FailureVerification
		result.ArtifactRef, result.FailureSignature = artifactRef, signature
		result.Checks = append(result.Checks, record(CheckOutcomeFailed))
		return result, nil
	}
	// Inconclusive, including a pass whose tests left no artifacts: no record at
	// all, so nothing can read this as a pass.
	result.ArtifactRef, result.FailureSignature = artifactRef, signature
	class := FailureTransientInfrastructure
	switch {
	case ctx.Err() != nil:
		class = execution.CancellationClass(context.Cause(ctx))
	case ran:
		class = FailureAssurancePrerequisite
	}
	return inconclusive(class, fmt.Errorf("browser acceptance was inconclusive (%d of %d selected tests passed, %d skipped, no artifacts from %v): %w",
		counts.Passed, counts.Total, counts.Skipped, silent, errors.Join(runErr, ctx.Err())))
}

// browserCheckProves reports whether the observation PROVES the browser claim
// for this exact candidate and governing base: a completed, passing record by
// the current checker, bound to the observation's own commit and tree and to
// the base whose tests ran, in which every selected test passed, none failed or
// skipped, and the tests left artifacts. An observation with no record - every
// one written before the requirement existed, and every one a generic suite
// produced - proves nothing about a browser.
func browserCheckProves(observation *AssuranceObservation, commit, base string) bool {
	if observation == nil || commit == "" || base == "" {
		return false
	}
	for _, check := range observation.Checks {
		results := check.Results
		if check.Kind == CheckKindBrowserAcceptance && check.Outcome == CheckOutcomePassed &&
			check.Checker == browserCheckerID && check.CheckerRevision == browserCheckerRevision() &&
			check.BaseRevision == base && check.Selection != "" && check.Surface != "" && check.ArtifactsSHA256 != "" &&
			check.Environment != "" && check.Artifact != "" &&
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
func browserEvidenceItem(item domain.EvidenceItem, observation *AssuranceObservation, commit, base string) (domain.EvidenceItem, bool) {
	if !browserCheckProves(observation, commit, base) {
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
// changed checker or base makes the check eligible again, and a contract that
// does not require it keeps the binding it always had.
func (s *runState) browserBindingSuffix() string {
	if s.rt == nil {
		return ""
	}
	return browserBindingSuffixFor(s.rt.deps.Policy, s.pinnedBase(), func() (domain.EngineeringWorkContract, error) { return s.rt.contractFor(s) })
}

// browserBindingSuffixFor is the pure decision. A contract that cannot be
// rebuilt leaves the binding as it was; discharging the claim is refused
// independently of planning.
func browserBindingSuffixFor(policy domain.EngineeringPolicy, base string, rebuild func() (domain.EngineeringWorkContract, error)) string {
	if !policyListsClaim(policy, BrowserAcceptanceClaimID) {
		return ""
	}
	contract, err := rebuild()
	if err != nil || !browserAcceptanceRequired(contract) {
		return ""
	}
	return "|browser-acceptance@" + fileSizeBindingDigest(browserCheckerRevision(), base)
}
