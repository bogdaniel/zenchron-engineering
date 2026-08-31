package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ExecutionProvider can change only the supplied candidate directory. It has
// no authority and receives no publication credentials.
type ExecutionProvider interface {
	Execute(context.Context, ExecutionRequest) (ExecutionResult, error)
}
type ExecutionRequest struct {
	RunID                 string
	SourceSnapshot        Ref
	ControllerID          string
	Base                  Ref
	Candidate             Candidate
	CandidateDir          string
	Contract              Ref
	Objective             string
	AcceptanceObligations []string
	Constraints           []string
	Prohibitions          []string
	Permissions           []string
	TrustedInstructions   string
	Purpose               InvocationPurpose
	Findings              []Finding
	Budgets               ProviderBudget
}

// InvocationPurpose is deliberately operational rather than a provider role.
type InvocationPurpose string

const (
	InvocationInitial     InvocationPurpose = "initial_implementation"
	InvocationRemediation InvocationPurpose = "remediation"
	// InvocationContinuation resumes work a bounded stop interrupted. It is
	// exact-bound to the runtime-owned checkpoint commit and tree the previous
	// invocation produced, so the provider sees a clean workspace at a known
	// revision and never unbound dirty state. It carries no findings: nothing
	// judged the work, it was simply cut off.
	InvocationContinuation InvocationPurpose = "continuation"
)

type Finding struct {
	Classification         FailureClass
	Signature, ArtifactRef string
}
type ProviderBudget struct {
	MaxTokens     *int64
	MaxCostMicros *int64
	WallLimit     time.Duration
}
type ExecutionResult struct {
	ProviderID, Model, AuthMode string
	Attempt                     int
	Outcome                     OperationState
	Tokens, CostMicros          *int64
	Artifacts                   []Artifact
	ChangeSummary               string
	ChangedPaths                []string
	Failure                     *ProviderFailure
}
type ProviderFailure struct {
	Classification   FailureClass
	RawDiagnosticRef string
}
type AssuranceProvider interface {
	Assure(context.Context, AssuranceRequest) (AssuranceResult, error)
}

// EvidenceProducer is a provider that DECLARES which evidence classes it can
// produce. It is the capability half of the evidence model: a policy names the
// class of evidence a claim needs, and this names the classes the configured
// system can actually obtain.
//
// The two vocabularies were never checked against each other. A run could
// therefore compile a contract, spend a real model budget implementing it, pass
// exact-tree assurance, and only then discover that the claim gating
// publication asked for a class nothing configured could ever produce.
// run-0943e257539346f8763db04505cbf322 did exactly that.
//
// Declaring a class is not a promise the evidence will PASS; it is a statement
// that this producer can answer that kind of question at all.
type EvidenceProducer interface {
	ProducedEvidenceClasses() []domain.EvidenceClass
}

// ProducibleEvidenceClasses is the set of evidence classes the configured
// providers declare, plus the classes a human records directly. A provider that
// declares nothing contributes nothing: capability is stated, never assumed
// from a type name.
func ProducibleEvidenceClasses(providers ...any) map[domain.EvidenceClass]bool {
	producible := map[domain.EvidenceClass]bool{
		// A human approval is obtained by a person through the operator
		// authority boundary, not by a provider.
		HumanEvidenceClass: true,
	}
	for _, provider := range providers {
		if declaring, ok := provider.(EvidenceProducer); ok {
			for _, class := range declaring.ProducedEvidenceClasses() {
				if class != "" {
					producible[class] = true
				}
			}
		}
	}
	return producible
}

// UnsupportedEvidenceRequirement names one required claim whose evidence class
// no configured producer can supply. It is bounded, typed identity - a claim id
// and a class - and never free text.
type UnsupportedEvidenceRequirement struct {
	ClaimID       string               `json:"claim_id"`
	EvidenceClass domain.EvidenceClass `json:"evidence_class"`
}

// UnfulfillableEvidence reports the required claims for one protected action
// that no configured producer and no human can satisfy, in deterministic order.
// An empty result means every required claim has SOME producer; it says nothing
// about whether that producer will pass.
func UnfulfillableEvidence(contract domain.EngineeringWorkContract, action domain.Action, producible map[domain.EvidenceClass]bool) []UnsupportedEvidenceRequirement {
	// The claim set is the SAME union authority evaluates: the action's own
	// condition plus the discharge claims of every material acceptance
	// obligation. Checking only the condition would let a run spend its whole
	// budget and then sit INCOMPLETE on an acceptance claim nothing can
	// produce - the original defect, one level up.
	var required []string
	for _, condition := range contract.AuthorityConditions {
		if condition.Action == action {
			required = append(required, condition.RequiredClaims...)
		}
	}
	for _, obligation := range contract.Obligations {
		if obligation.Material {
			required = append(required, obligation.RequiredClaims...)
		}
	}
	var unsupported []UnsupportedEvidenceRequirement
	{
		seen := map[string]bool{}
		for _, claimID := range required {
			if seen[claimID] {
				continue
			}
			seen[claimID] = true
			claim, defined := contract.RequiredClaims[claimID]
			if !defined {
				// An undefined claim is refused by the compiler long before
				// here; treating it as unsupported keeps this total.
				unsupported = append(unsupported, UnsupportedEvidenceRequirement{ClaimID: claimID})
				continue
			}
			if !producible[claim.EvidenceClass] {
				unsupported = append(unsupported, UnsupportedEvidenceRequirement{ClaimID: claimID, EvidenceClass: claim.EvidenceClass})
			}
		}
	}
	sort.Slice(unsupported, func(i, j int) bool { return unsupported[i].ClaimID < unsupported[j].ClaimID })
	return unsupported
}

// SemanticClaimRequest is one acceptance question, stated by the runtime. The
// verifier is told which claim and which obligations it is judging; it cannot
// choose them.
type SemanticClaimRequest struct {
	ClaimID       string
	ObligationIDs []string
	Statements    []string
}

type AssuranceRequest struct {
	RunID, Commit, Tree, CheckoutDir string
	Contract                         Ref
	Policy                           Ref
	Producer                         Ref
	VerifierDefinition               string
	// The fields below are used by the semantic verifier. They are bounded,
	// read-only context: identity, the exact base the diff is taken against, the
	// changed-path inventory, a summary of the automated result, and the exact
	// claims to judge. None of them is a capability.
	Repository         string
	Base               string
	Objective          string
	ChangedPaths       []string
	AutomatedAssurance string
	SemanticClaims     []SemanticClaimRequest
}
type AssuranceResult struct {
	ProviderID, VerifierDefinition string
	Passed                         bool
	FailureClass                   FailureClass
	Artifacts                      []Artifact
	Evidence                       *EvidenceBinding
	// Model and Tokens are recorded only when the producer actually reports
	// them. Nothing here is invented when a provider does not expose usage.
	Model  string
	Tokens int64
	// SemanticClaims is the per-claim observation of a semantic verifier. A
	// verdict is claim-specific: one claim may be discharged while another is
	// not, and a bundle must say so rather than collapsing to one boolean.
	SemanticClaims map[string]SemanticClaimVerdict
}

// EvidenceBinding is adapter output, not an AuthorityDecision. The caller
// builds the domain EvidenceBundle only after validating this exact binding.
type EvidenceBinding struct {
	Commit, Tree                            string
	Contract, Policy, Producer, Environment Ref
}

// GuardCandidate rejects unsafe additions but never removes otherwise safe,
// out-of-contract changes: those must be reassessed by the kernel.
func GuardCandidate(root string, paths []string, maxBytes int64) error {
	var total int64
	for _, p := range paths {
		normalized, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: []string{p}, PathsKnown: true})
		if err != nil || filepath.IsAbs(p) || len(normalized.Paths) != 1 {
			return fmt.Errorf("unsafe candidate path %q", p)
		}
		p = normalized.Paths[0]
		lower := strings.ToLower(filepath.Base(p))
		if strings.Contains(lower, ".env") || strings.Contains(lower, "credential") || strings.Contains(lower, "id_rsa") || strings.Contains(lower, "private") || strings.Contains(lower, "secret") {
			return fmt.Errorf("sensitive candidate path %q", p)
		}
		info, err := os.Lstat(filepath.Join(root, p))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink candidate path %q", p)
		}
		total += info.Size()
		if maxBytes > 0 && total > maxBytes {
			return fmt.Errorf("candidate exceeds size ceiling")
		}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(filepath.Join(root, p))
			if readErr != nil {
				return readErr
			}
			content := strings.ToLower(string(data))
			if strings.Contains(content, "-----begin private key-----") || strings.Contains(content, "aws_secret_access_key") || strings.Contains(content, "github_pat_") {
				return fmt.Errorf("sensitive candidate content %q", p)
			}
		}
	}
	return nil
}

// VerificationSurfaceChanged identifies candidate-controlled verifier inputs.
func VerificationSurfaceChanged(paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if strings.HasSuffix(p, "_test.go") || p == "go.mod" || p == "go.sum" || strings.HasPrefix(p, ".github/workflows/") {
			return true
		}
	}
	return false
}

type FailureClass string

const (
	FailureFormat                  FailureClass = "format"
	FailureCompileTest             FailureClass = "compile_or_test"
	FailureVerification            FailureClass = "verification_failure"
	FailureSurface                 FailureClass = "verification_surface_changed"
	FailureWeakened                FailureClass = "verification_weakened"
	FailureTransientProvider       FailureClass = "transient_execution_provider"
	FailureTransientInfrastructure FailureClass = "transient_infrastructure"
	FailureMaterialScope           FailureClass = "material_scope_change"
	FailureAuthorityWait           FailureClass = "authority_wait"
	// FailureProviderAccountUnavailable is a recoverable EXTERNAL ACCOUNT
	// prerequisite of the execution provider: the provider refused at its own
	// account boundary before any reasoning happened. It is not transient (a
	// retry cannot clear it), not a producer or code failure (no work was
	// attempted), not an authority failure (nothing about permission changed),
	// and not terminal (an operator restores the account and the same run
	// continues). It is deliberately NOT FailureAuthorityWait: conflating an
	// external billing prerequisite with the human-authority boundary would
	// make status tell an operator to resolve an authority condition that does
	// not exist.
	FailureProviderAccountUnavailable FailureClass = "provider_account_unavailable"
	// FailureExecutionIncomplete is a producer invocation that produced real
	// work and then ran out of one of the runtime's own bounds. The work is
	// preserved as a checkpoint; the OPERATION did not complete, which is why
	// it is a failure at all. It routes to a retry because continuing is
	// exactly what it needs, and routing it that way is what puts continuations
	// under the ordinary execution attempt budget instead of a counter of their
	// own.
	FailureExecutionIncomplete FailureClass = "execution_incomplete"
	// FailureAssurancePrerequisite is the ENVIRONMENT the verifier needs not
	// being there: the configured image resolves no toolchain, the
	// operator-provisioned dependency cache is missing or empty, or the exact
	// tree needs a module the trusted offline cache does not hold.
	//
	// No verdict about the candidate was reached, so it is not a verification
	// failure. Re-running the identical command against the identical
	// environment produces the identical result, so it is not transient either -
	// classifying it as transient infrastructure is what let one deterministic
	// fault consume every assurance attempt in seconds. It waits: an operator
	// provisions what is missing and the same run re-derives assurance against
	// the same exact commit, tree and contract.
	//
	// It is deliberately NOT FailureAuthorityWait. Nothing about human authority
	// is involved.
	FailureAssurancePrerequisite FailureClass = "assurance_prerequisite_unavailable"
	// FailureGovernedRemoteMismatch is a deterministic trust refusal: the
	// remote a workspace is bound to is not this run's governed remote.
	//
	// It routes to a STOP, not a retry and not a wait. Retrying cannot change
	// the answer - the governed remote is configuration and nothing the runtime
	// does alters it - and waiting would be a lie about what an operator can
	// fix in place: changing the governed remote changes which repository the
	// run is about, which is a different trusted subject and therefore a
	// different run. It is not a producer failure, not a verification verdict,
	// and not an authority condition.
	FailureGovernedRemoteMismatch FailureClass = "governed_remote_mismatch"
	// FailureRequiredEvidenceUnsupported is a contract this configured system
	// can never satisfy: the claim gating a protected action names an evidence
	// class no configured producer declares and no human records.
	//
	// It routes to a STOP, and it is detected BEFORE any model budget is spent.
	// Retrying cannot conjure a producer, and waiting would imply an operator
	// could clear it in place - they cannot: the fix is a different policy or a
	// different configured producer, either of which changes the terms the run
	// is governed by and therefore needs a new run. Discovering it after the
	// work is done, as run-0943e257539346f8763db04505cbf322 did, is the defect.
	FailureRequiredEvidenceUnsupported FailureClass = "required_evidence_unsupported"
	FailureGovernanceMismatch          FailureClass = "governance_mismatch"
	FailureWorkspaceIntegrity          FailureClass = "workspace_integrity_violation"
	FailureBaseIntegrationConflict     FailureClass = "base_integration_conflict"
	FailureFlaky                       FailureClass = "flaky_verification"
	FailureUnknown                     FailureClass = "unknown"
)

type FailureRoute string

const (
	RouteGofmt               FailureRoute = "remediation.gofmt"
	RouteProviderRemediation FailureRoute = "execution.remediation"
	RouteRetry               FailureRoute = "retry"
	RouteReassess            FailureRoute = "reassess"
	RouteWait                FailureRoute = "wait"
	RouteRestore             FailureRoute = "restore_or_refuse"
	RouteStop                FailureRoute = "stop"
)

func RouteFailure(c FailureClass) FailureRoute {
	switch c {
	case FailureFormat:
		return RouteGofmt
	case FailureCompileTest, FailureBaseIntegrationConflict:
		return RouteProviderRemediation
	case FailureTransientProvider, FailureTransientInfrastructure, FailureExecutionIncomplete:
		return RouteRetry
	case FailureMaterialScope, FailureSurface, FailureWeakened, FailureGovernanceMismatch:
		return RouteReassess
	case FailureWorkspaceIntegrity:
		return RouteRestore
	case FailureAuthorityWait, FailureProviderAccountUnavailable, FailureAssurancePrerequisite:
		return RouteWait
	default:
		return RouteStop
	}
}

// MergePrecedence is deliberately observation-only: merged always wins over
// issue closure, including when a new controller would otherwise block work.
func MergePrecedence(merged, issueClosed bool) (Disposition, string) {
	if merged {
		return Completed, "merged"
	}
	if issueClosed {
		return Waiting, "source_closed"
	}
	return Active, ""
}

// HumanAuthorityBinding prevents an approval from being carried onto a moved
// candidate or contract. It intentionally does not perform a merge.
type HumanAuthorityBinding struct {
	RunID, CandidateRevision, CandidateTree string
	Contract                                Ref
	Action                                  string
	Decision                                string
	HumanID                                 string
}

func (b HumanAuthorityBinding) Validate(s RunSnapshot) error {
	if b.RunID != s.ID || b.CandidateRevision != s.Candidate.Revision || b.CandidateTree != s.Candidate.Tree || b.Contract != s.Contract {
		return fmt.Errorf("stale human authority binding")
	}
	if b.Decision != "approve" && b.Decision != "reject" {
		return fmt.Errorf("invalid human decision")
	}
	return nil
}
