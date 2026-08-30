package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
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
type AssuranceRequest struct {
	RunID, Commit, Tree, CheckoutDir string
	Contract                         Ref
	Policy                           Ref
	Producer                         Ref
	VerifierDefinition               string
}
type AssuranceResult struct {
	ProviderID, VerifierDefinition string
	Passed                         bool
	FailureClass                   FailureClass
	Artifacts                      []Artifact
	Evidence                       *EvidenceBinding
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
	FailureGovernanceMismatch      FailureClass = "governance_mismatch"
	FailureWorkspaceIntegrity      FailureClass = "workspace_integrity_violation"
	FailureBaseIntegrationConflict FailureClass = "base_integration_conflict"
	FailureFlaky                   FailureClass = "flaky_verification"
	FailureUnknown                 FailureClass = "unknown"
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
	case FailureTransientProvider, FailureTransientInfrastructure:
		return RouteRetry
	case FailureMaterialScope, FailureSurface, FailureWeakened, FailureGovernanceMismatch:
		return RouteReassess
	case FailureWorkspaceIntegrity:
		return RouteRestore
	case FailureAuthorityWait:
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
