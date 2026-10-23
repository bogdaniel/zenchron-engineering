package runtime

import (
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
	if _, required := contract.RequiredClaims[FileSizePolicyClaimID]; !required {
		return ""
	}
	return base
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

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// judgedBeforeSandbox records a verdict the verifier reached without starting a
// container, with the same artifacts, failure signature and evidence binding as
// one it reached inside it.
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
		Artifacts: artifacts, ArtifactRef: artifactRef, FailureSignature: signature, Evidence: v.evidenceBinding(request),
	}, nil
}

// evidenceBinding is the provenance every verdict of this verifier carries.
func (v BaselineGoVerifier) evidenceBinding(request AssuranceRequest) *EvidenceBinding {
	return &EvidenceBinding{
		Commit: request.Commit, Tree: request.Tree, Contract: request.Contract, Policy: request.Policy,
		Producer:    Ref{ID: "baseline-go", Revision: v.Definition()},
		Environment: Ref{ID: "docker-network-none", Revision: v.Sandbox.Image},
	}
}
