package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// The confirmation pass is a DIFFERENT verification and writes its own
	// immutable transcript. Reusing the first pass's identity would make the
	// flake check overwrite the very evidence it exists to compare against.
	confirmation := request
	confirmation.Confirmation = true
	second, secondErr := provider.Assure(ctx, confirmation)
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
	// Answer, when set, builds a raw model-shaped verdict for the request, which
	// this fake then decodes through the REAL gate. It is how a scenario models
	// what a provider does with an answer the runtime must refuse - a narrowed
	// obligation set, say - without the fixture deciding the refusal itself.
	Answer func(AssuranceRequest) string
	Err    error
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
	if f.Answer != nil {
		results, err := decodeSemanticVerdict([]byte(f.Answer(r)), r.SemanticClaims)
		if err != nil {
			return AssuranceResult{ProviderID: semanticProviderID, VerifierDefinition: definition, FailureClass: FailureVerification}, err
		}
		passed := true
		for _, result := range results {
			if result.Status != "pass" {
				passed = false
			}
		}
		return AssuranceResult{
			ProviderID: semanticProviderID, VerifierDefinition: definition, Passed: passed,
			SemanticClaims: results, Model: "fixture-model",
			Evidence: &EvidenceBinding{
				Commit: r.Commit, Tree: r.Tree, Contract: r.Contract, Policy: r.Policy,
				Producer:    Ref{ID: semanticProviderID, Revision: definition},
				Environment: Ref{ID: "fixture-control-plane", Revision: "fixture-model"},
			},
		}, nil
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

// FakeReviewerProvider is the deterministic stand-in for a reviewer worker: it
// writes a real ReviewerResult to the runtime-owned path it was given, exactly
// as an installed CLI would, and the runtime then reads and admits it through
// the production path.
//
// It writes a FILE rather than returning a struct on purpose. A fake that
// returned ExecutionResult.Review directly would skip the adapter's read, the
// strict decode, the bound and the slot preparation - which is most of what the
// protocol is - and would prove a state machine production cannot enter. The
// #126 regression exists because exactly that gap went unnoticed once already.
type FakeReviewerProvider struct {
	*FakeExecutionProvider
	// Verdicts are consumed in order, one per reviewer invocation, so a
	// scenario can block once and accept later.
	Verdicts []ReviewerResult
	// Raw, when set for an invocation index, is written verbatim instead of the
	// encoded verdict. It is how a malformed or hostile document is modelled
	// without the fixture deciding the refusal itself.
	Raw map[int]string
	// Reviewed records the result path of every reviewer invocation, so a test
	// can assert the channel was used at all.
	Reviewed []string
	calls    int
}

func NewFakeReviewerProvider(verdicts ...ReviewerResult) *FakeReviewerProvider {
	return &FakeReviewerProvider{
		FakeExecutionProvider: &FakeExecutionProvider{Result: ExecutionResult{ProviderID: "claude", Outcome: Succeeded}},
		Verdicts:              verdicts,
	}
}

func (f *FakeReviewerProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (f *FakeReviewerProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	result, err := f.FakeExecutionProvider.Execute(ctx, request)
	if err != nil || request.ReviewerResultPath == "" {
		return result, err
	}
	// A review note in the workspace, so the reviewer's own run produces a
	// candidate and is verified like any other. It is NOT the verdict and is
	// never read as one: the verdict goes to the runtime-owned path below, and
	// a reviewer that wrote only this would have produced no verdict at all.
	if note := filepath.Join(request.CandidateDir, "review-notes.md"); request.CandidateDir != "" {
		if writeErr := os.WriteFile(note, []byte("# review\n"), 0o600); writeErr != nil {
			return result, writeErr
		}
	}
	index := f.calls
	f.calls++
	f.Reviewed = append(f.Reviewed, request.ReviewerResultPath)
	if raw, ok := f.Raw[index]; ok {
		if writeErr := os.WriteFile(request.ReviewerResultPath, []byte(raw), 0o600); writeErr != nil {
			return result, writeErr
		}
		return f.read(request, result)
	}
	if index >= len(f.Verdicts) {
		// No verdict for this invocation: the reviewer produced prose and
		// nothing else, which is a real outcome the lifecycle has to handle.
		return result, nil
	}
	document, marshalErr := json.Marshal(f.Verdicts[index])
	if marshalErr != nil {
		return result, marshalErr
	}
	if writeErr := os.WriteFile(request.ReviewerResultPath, document, 0o600); writeErr != nil {
		return result, writeErr
	}
	return f.read(request, result)
}

// read is the adapter half: the same strict decode CLIAgentProvider performs,
// so the fixture exercises the production reader rather than a second one.
func (f *FakeReviewerProvider) read(request ExecutionRequest, result ExecutionResult) (ExecutionResult, error) {
	review, err := ReadReviewerResult(request.ReviewerResultPath)
	if err != nil {
		result.Outcome = OperationFailed
		result.Failure = &ProviderFailure{Classification: FailureVerification}
		return result, nil
	}
	result.Review = review
	return result, nil
}
