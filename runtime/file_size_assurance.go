package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// fileSizePolicyBase is the base revision whose size policy a contract's
// assurance must enforce, or "" when the contract does not require it. The
// requirement is the claim FileSizePolicyClaimID in the contract's compiled
// required claims, so it exists only where the EngineeringPolicy that compiled
// the contract listed it: a contract compiled before the policy changed has no
// such claim and is verified exactly as it always was.
func fileSizePolicyBase(contract domain.EngineeringWorkContract, base string) string {
	if !fileSizePolicyRequired(contract) {
		return ""
	}
	return base
}

// fileSizePolicyRequired reports whether the contract's compiled required claims
// include the file-size claim. A required check with no base revision to read
// the rules from is refused by the caller, never skipped.
func fileSizePolicyRequired(contract domain.EngineeringWorkContract) bool {
	_, required := contract.RequiredClaims[FileSizePolicyClaimID]
	return required
}

// Assure is the baseline verifier: the checks that run on the host first, then
// the Go suite in the sandbox. A host-side refusal is a verdict and the suite
// is not started. A completed host-side check is recorded on whatever result
// follows, as a typed record bound to the exact commit, tree and base, because
// that record - not a passing suite - is what proves the file-size claim.
func (v BaselineGoVerifier) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	incomplete := request.Commit == "" || request.Tree == "" || request.CheckoutDir == "" || request.Contract.ID == ""
	if (request.FileSizePolicyBase == "" && !request.BrowserAcceptance) || incomplete {
		return v.assureInSandbox(ctx, request)
	}
	if result, done, err := v.fileSizeVerdict(request); done {
		return result, err
	}
	result, err := v.assureInSandbox(ctx, request)
	if request.FileSizePolicyBase != "" {
		result.Checks = append(result.Checks, fileSizeCheck(request, CheckOutcomePassed))
	}
	if request.BrowserAcceptance && err == nil && result.Passed {
		return v.withBrowserAcceptance(ctx, request, result)
	}
	return result, err
}

// fileSizeCheck is the record of the size check for this request's exact
// candidate and base.
func fileSizeCheck(request AssuranceRequest, outcome string) VerificationCheck {
	return VerificationCheck{
		Kind: CheckKindFileSizePolicy, Checker: fileSizeCheckerID, CheckerRevision: fileSizeCheckerRevision(),
		Outcome: outcome, Commit: request.Commit, Tree: request.Tree, BaseRevision: request.FileSizePolicyBase,
	}
}

// fileSizeVerdict runs the size policy for an assurance request that carries
// one, BEFORE the sandbox does any work: it takes milliseconds, and a refusal
// saves the 25 minutes a full suite takes.
//
// done reports whether this is the verdict. A refusal is a judged failure of
// the candidate and is returned as a RESULT (FailureVerification), so it takes
// the same route as a failing test: the transcript becomes the sanitized,
// attempt-scoped artifact a remediation agent is shown, and the evidence is
// bound to the exact commit and tree. A policy that cannot be evaluated is not
// a verdict about the candidate; it is a prerequisite failure.
func (v BaselineGoVerifier) fileSizeVerdict(request AssuranceRequest) (result AssuranceResult, done bool, err error) {
	if request.FileSizePolicyBase == "" {
		return AssuranceResult{}, false, nil
	}
	// The check reads the checkout, so the checkout must be proven to BE the
	// exact candidate first, as the suite's own run proves it (#437): it was
	// cloned from the verified subject store, and its commit and tree are
	// compared to the recorded ones here before any content is read.
	if err := verifyAssuranceCheckout(request); err != nil {
		return AssuranceResult{ProviderID: baselineGoProviderID, VerifierDefinition: v.Definition(), FailureClass: FailureAssurancePrerequisite}, true, err
	}
	violations, checkErr := CheckFileSizePolicy(request.CheckoutDir, request.FileSizePolicyBase, request.Tree)
	if checkErr != nil {
		return AssuranceResult{ProviderID: baselineGoProviderID, VerifierDefinition: v.Definition(), FailureClass: FailureAssurancePrerequisite},
			true, fmt.Errorf("the file-size policy could not be evaluated: %w", checkErr)
	}
	if len(violations) == 0 {
		return AssuranceResult{}, false, nil
	}
	var report strings.Builder
	for _, violation := range violations {
		report.WriteString(violation.String() + "\n")
	}
	report.WriteString("The file-size limits are fixed by the base revision " + shortRevision(request.FileSizePolicyBase) +
		"; a candidate cannot raise them. Move cohesive code into a new file instead of growing these, and do not edit " +
		fileSizeBaselineFile + " or " + fileSizeExceptionFile + ".\n")
	result, err = v.judgedBeforeSandbox(request, []byte(report.String()))
	if err != nil {
		return AssuranceResult{}, true, err
	}
	return result, true, nil
}

// verifyAssuranceCheckout proves the checkout's HEAD is the exact recorded
// commit and tree.
func verifyAssuranceCheckout(request AssuranceRequest) error {
	if tree, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD^{tree}"); err != nil || strings.TrimSpace(tree) != request.Tree {
		return errors.New("exact candidate tree unavailable")
	}
	if commit, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(commit) != request.Commit {
		return errors.New("exact candidate commit unavailable")
	}
	return nil
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// judgedBeforeSandbox records a verdict the verifier reached without starting a
// container, with the same artifacts and failure signature as one it reached
// inside it, and with the HOST binding: the check ran on the controller, so the
// evidence names that environment and not the sandbox's.
func (v BaselineGoVerifier) judgedBeforeSandbox(request AssuranceRequest, report []byte) (AssuranceResult, error) {
	if v.ArtifactStore.Root == "" {
		return AssuranceResult{}, errors.New("local artifact store required")
	}
	attempt := request.AttemptRef()
	artifacts, err := v.ArtifactStore.StoreExecutionAttemptTranscript(baselineGoProviderID, attempt, nil, report)
	if err != nil {
		return AssuranceResult{}, err
	}
	artifactRef, err := attemptTranscriptPrefix(baselineGoProviderID, attempt)
	if err != nil {
		return AssuranceResult{}, err
	}
	signature, err := AssuranceFailureSignature(redactTranscript(report))
	if err != nil {
		return AssuranceResult{}, err
	}
	return AssuranceResult{
		ProviderID: baselineGoProviderID, VerifierDefinition: v.Definition(), Passed: false, FailureClass: FailureVerification,
		Artifacts: artifacts, ArtifactRef: artifactRef, FailureSignature: signature, Evidence: v.hostEvidenceBinding(request),
		Checks: []VerificationCheck{fileSizeCheck(request, CheckOutcomeFailed)},
	}, nil
}

// The two places this verifier can reach a verdict have two different
// environments, and a binding must name the one that actually ran.
const (
	// fileSizeCheckerID and fileSizeHostEnvironmentID identify the size check:
	// the controller's Go port of the repository's checker, reading the exact
	// tree from the host-side assurance checkout through read-only git object
	// access. No container, image or network isolation is involved, so none is
	// claimed.
	fileSizeCheckerID          = "file-size-policy-go"
	fileSizeHostEnvironmentID  = "controller-host-git-readonly"
	fileSizeHostEnvironmentRev = "git-object-store-read-only"
)

// fileSizeCheckerRevision identifies the implementation that produced a size
// verdict, so a change to its rules is a change of identity, as it is for the
// sandbox verifier's definition.
func fileSizeCheckerRevision() string {
	digest, _ := Digest(struct{ Name, Version string }{"file-size-policy", "go-port-of-check_file_sizes.py-v1"})
	return digest
}

// hostEvidenceBinding is the provenance of a verdict the controller reached on
// the host, before any container ran. It must never be read as sandbox
// evidence: its environment is not the sandbox's.
func (v BaselineGoVerifier) hostEvidenceBinding(request AssuranceRequest) *EvidenceBinding {
	return &EvidenceBinding{
		Commit: request.Commit, Tree: request.Tree, Contract: request.Contract, Policy: request.Policy,
		Producer:    Ref{ID: fileSizeCheckerID, Revision: fileSizeCheckerRevision()},
		Environment: Ref{ID: fileSizeHostEnvironmentID, Revision: fileSizeHostEnvironmentRev},
	}
}

// sandboxEvidenceBinding is the provenance of a verdict the Go suite reached
// inside the network-less sandbox.
func (v BaselineGoVerifier) sandboxEvidenceBinding(request AssuranceRequest) *EvidenceBinding {
	return &EvidenceBinding{
		Commit: request.Commit, Tree: request.Tree, Contract: request.Contract, Policy: request.Policy,
		Producer:    Ref{ID: "baseline-go", Revision: v.Definition()},
		Environment: Ref{ID: "docker-network-none", Revision: v.Sandbox.Image},
	}
}
