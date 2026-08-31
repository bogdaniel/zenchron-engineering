package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Fake providers make planner and boundary tests independent of a real Codex
// installation or an external assurance service.
type FakeExecutionProvider struct {
	Request ExecutionRequest
	Result  ExecutionResult
	Err     error
}

func (f *FakeExecutionProvider) Execute(_ context.Context, r ExecutionRequest) (ExecutionResult, error) {
	f.Request = r
	return f.Result, f.Err
}

type FakeAssuranceProvider struct {
	Requests []AssuranceRequest
	Results  []AssuranceResult
	Err      error
	// Produces overrides the declared capability, so a scenario can model a
	// configuration that lacks a producer for some class.
	Produces []domain.EvidenceClass
}

// ProducedEvidenceClasses makes the double faithful to what it stands in for:
// the baseline verifier. Capability is declared here for the same reason it is
// declared there - a provider that states nothing produces nothing, and a
// contract gated on evidence nothing produces is refused before any work.
func (f *FakeAssuranceProvider) ProducedEvidenceClasses() []domain.EvidenceClass {
	if len(f.Produces) > 0 {
		return f.Produces
	}
	return []domain.EvidenceClass{AssuranceEvidenceClass}
}

func (f *FakeAssuranceProvider) Assure(_ context.Context, r AssuranceRequest) (AssuranceResult, error) {
	f.Requests = append(f.Requests, r)
	if len(f.Results) == 0 {
		return AssuranceResult{}, f.Err
	}
	result := f.Results[0]
	f.Results = f.Results[1:]
	return result, f.Err
}

// FailureFingerprint deliberately contains durable identifiers rather than
// free-form provider transcripts. It is the retry/no-progress identity.
type FailureFingerprint struct{ CandidateTree, ContractRevision, FailureSignature, VerifierIdentity, ProviderIdentity, RemediationIdentity string }

func (f FailureFingerprint) String() string {
	parts := []string{f.CandidateTree, f.ContractRevision, f.FailureSignature, f.VerifierIdentity, f.ProviderIdentity, f.RemediationIdentity}
	return strings.Join(parts, "|")
}

type NoProgressTracker struct {
	Seen  map[string]int
	Limit int
}

func (t *NoProgressTracker) Allow(f FailureFingerprint) bool {
	if t.Seen == nil {
		t.Seen = map[string]int{}
	}
	if t.Limit <= 0 {
		t.Limit = 1
	}
	k := f.String()
	t.Seen[k]++
	return t.Seen[k] <= t.Limit
}

// AssuranceRerun enforces the single identical rerun law. A disagreement is
// flaky even if the retry passes; no producer mutation happens between calls.
func AssuranceRerun(ctx context.Context, provider AssuranceProvider, request AssuranceRequest) (AssuranceResult, FailureClass, error) {
	first, err := provider.Assure(ctx, request)
	if err != nil || first.Passed {
		return first, first.FailureClass, err
	}
	second, secondErr := provider.Assure(ctx, request)
	if secondErr != nil {
		return second, FailureUnknown, secondErr
	}
	if second.Passed != first.Passed || second.FailureClass != first.FailureClass {
		return second, FailureFlaky, nil
	}
	return second, second.FailureClass, nil
}

// MutationCoordinator is the only remediation bridge: it guards then creates a
// runtime-owned commit and immediately returns through KernelFlow/#8.
type MutationCoordinator struct {
	Flow       KernelFlow
	Workspace  *CandidateWorkspace
	Repository string
	MaxBytes   int64
}

func (c MutationCoordinator) CommitAndObserve(state KernelState, model domain.ProjectModel, policy domain.EngineeringPolicy, message string) (KernelState, CommitResult, error) {
	if c.Workspace == nil {
		return state, CommitResult{}, fmt.Errorf("candidate workspace required")
	}
	result, err := c.Workspace.Commit(message, c.MaxBytes)
	if err != nil {
		return state, result, err
	}
	next, err := c.Flow.ObserveCommit(state, model, policy, c.Repository, result)
	return next, result, err
}

// DeterministicGofmt is intentionally narrow. The caller supplies the actual
// formatter so test fixtures do not need a host Go installation; production
// wires this to a constrained deterministic tool invocation.
type DeterministicGofmt interface {
	Format(context.Context, string, []string) error
}
type GofmtFunc func(context.Context, string, []string) error

func (f GofmtFunc) Format(ctx context.Context, root string, paths []string) error {
	return f(ctx, root, paths)
}

// LocalGofmt is a runtime-owned deterministic producer. It accepts only
// normalized repository-relative Go paths and does not inherit an agent shell
// or provider environment.
type LocalGofmt struct {
	Executor CommandExecutor
	Grace    time.Duration
}

func (g LocalGofmt) Format(ctx context.Context, root string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no Go paths to format")
	}
	if g.Executor == nil {
		g.Executor = OSCommandExecutor{}
	}
	if g.Executor.LookPath("gofmt") != nil {
		return fmt.Errorf("gofmt unavailable")
	}
	args := []string{"-w"}
	for _, path := range paths {
		normalized, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: []string{path}, PathsKnown: true})
		if err != nil || filepath.IsAbs(path) || len(normalized.Paths) != 1 || filepath.Ext(normalized.Paths[0]) != ".go" {
			return fmt.Errorf("unsafe formatter path %q", path)
		}
		args = append(args, normalized.Paths[0])
	}
	_, err := g.Executor.Run(ctx, "gofmt", args, root, []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, g.Grace)
	return err
}
func FormatPaths(paths []string) []string {
	var goPaths []string
	for _, path := range paths {
		if strings.HasSuffix(path, ".go") {
			goPaths = append(goPaths, path)
		}
	}
	sort.Strings(goPaths)
	return goPaths
}
func (c MutationCoordinator) RemediateFormat(ctx context.Context, formatter DeterministicGofmt, state KernelState, model domain.ProjectModel, policy domain.EngineeringPolicy, paths []string) (KernelState, CommitResult, error) {
	if formatter == nil || c.Workspace == nil {
		return state, CommitResult{}, fmt.Errorf("formatter and candidate workspace required")
	}
	goPaths := FormatPaths(paths)
	if len(goPaths) == 0 {
		return state, CommitResult{}, fmt.Errorf("format failure has no Go paths")
	}
	if err := formatter.Format(ctx, c.Workspace.Dir, goPaths); err != nil {
		return state, CommitResult{}, err
	}
	return c.CommitAndObserve(state, model, policy, "zenchron: deterministic gofmt remediation")
}

// FakeSemanticAssuranceProvider is the deterministic stand-in for the
// independent semantic producer. It answers every claim it is asked about with
// Status, so a scenario can model pass, fail, inconclusive, or a verdict that
// was never reached - without a network call and without a model.
type FakeSemanticAssuranceProvider struct {
	Requests []AssuranceRequest
	Status   string
	// Omit, when set, is a claim id the verdict leaves unanswered, which is how
	// an incomplete answer is modelled.
	Omit string
	Err  error
	// Class overrides the declared capability so a scenario can model a
	// configuration whose semantic producer is absent or answers something else.
	Class []domain.EvidenceClass
}

func (f *FakeSemanticAssuranceProvider) ProducedEvidenceClasses() []domain.EvidenceClass {
	if len(f.Class) > 0 {
		return f.Class
	}
	return []domain.EvidenceClass{SemanticEvidenceClass}
}

func (f *FakeSemanticAssuranceProvider) Assure(_ context.Context, r AssuranceRequest) (AssuranceResult, error) {
	f.Requests = append(f.Requests, r)
	definition := SemanticVerifierDefinition()
	if f.Err != nil {
		return AssuranceResult{ProviderID: semanticProviderID, VerifierDefinition: definition, FailureClass: FailureTransientProvider}, f.Err
	}
	status := f.Status
	if status == "" {
		status = "pass"
	}
	claims := map[string]SemanticClaimVerdict{}
	passed := true
	for _, claim := range r.SemanticClaims {
		if claim.ClaimID == f.Omit {
			continue
		}
		claims[claim.ClaimID] = SemanticClaimVerdict{
			ClaimID: claim.ClaimID, ObligationIDs: claim.ObligationIDs,
			Status: status, Rationale: "fixture verdict",
		}
		if status != "pass" {
			passed = false
		}
	}
	if len(claims) == 0 {
		return AssuranceResult{ProviderID: semanticProviderID, VerifierDefinition: definition, FailureClass: FailureVerification},
			fmt.Errorf("semantic verdict answered no required claim")
	}
	return AssuranceResult{
		ProviderID: semanticProviderID, VerifierDefinition: definition, Passed: passed,
		SemanticClaims: claims, Model: "fixture-model",
		Evidence: &EvidenceBinding{Commit: r.Commit, Tree: r.Tree, Contract: r.Contract, Policy: r.Policy,
			Producer: Ref{ID: semanticProviderID, Revision: definition}},
	}, nil
}
