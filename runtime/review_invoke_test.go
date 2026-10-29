package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// reviewStubProvider writes a scripted ReviewerResult to the slot it is given
// and, optionally, mutates the workspace - which is exactly the boundary
// InvokeReviewer must catch regardless of what the provider's result claims.
type reviewStubProvider struct {
	document string
	mutate   func(dir string) error
}

func (p reviewStubProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if p.mutate != nil {
		if err := p.mutate(request.CandidateDir); err != nil {
			return ExecutionResult{}, err
		}
	}
	if p.document != "" {
		if err := os.WriteFile(request.ReviewerResultPath, []byte(p.document), 0600); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{ProviderID: "test-reviewer", Outcome: execution.Succeeded}, nil
}

func reviewInvocationFixture(t *testing.T, provider ExecutionProvider) ReviewInvocationInput {
	t.Helper()
	workspaceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceDir, "a.go"), []byte("package a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return ReviewInvocationInput{
		ReviewID: "review-1",
		Packet: ReviewPacket{Trusted: ReviewTrustedFacts{
			RunID: "run-1", Repository: "zenchron/fixture", PRNumber: 7,
			HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, ContractID: "contract", ContractRevision: "1",
		}},
		Workspace:    &PlanningWorkspace{Dir: workspaceDir, Commit: testHeadSHA, Tree: "tree1"},
		Agent:        ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode},
		Provider:     provider,
		Artifacts:    ArtifactStore{Root: t.TempDir()},
		StateDir:     t.TempDir(),
		ControllerID: "controller-1",
	}
}

func TestInvokeReviewerReadsBackTheWrittenVerdict(t *testing.T) {
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	provider := reviewStubProvider{document: string(document)}
	in := reviewInvocationFixture(t, provider)

	out, err := InvokeReviewer(context.Background(), in)
	if err != nil {
		t.Fatalf("InvokeReviewer: %v", err)
	}
	if out.Verdict == nil || out.Verdict.Verdict != StageReviewAccepted {
		t.Fatalf("expected the written verdict to be read back, got %+v", out.Verdict)
	}
}

func TestInvokeReviewerRequiresNonMutatingMode(t *testing.T) {
	provider := reviewStubProvider{}
	in := reviewInvocationFixture(t, provider)
	if _, err := InvokeReviewer(context.Background(), in); err != nil {
		t.Fatalf("expected an absent verdict to be a successful invocation with no verdict, got %v", err)
	}
}

// Mutation check: a provider that mutates the workspace it was given
// non-mutating authority over must be refused, regardless of what verdict it
// also wrote. Removing the before/after digest comparison in InvokeReviewer
// must make this test fail.
func TestInvokeReviewerRefusesAProviderThatMutatesTheWorkspace(t *testing.T) {
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	provider := reviewStubProvider{
		document: string(document),
		mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "sneaky.go"), []byte("package a\n"), 0600)
		},
	}
	in := reviewInvocationFixture(t, provider)
	_, err = InvokeReviewer(context.Background(), in)
	if err == nil {
		t.Fatal("expected a workspace mutation during a non-mutating review to be refused")
	}
	if _, ok := err.(*ReviewWorkspaceChangedError); !ok {
		t.Fatalf("expected *ReviewWorkspaceChangedError, got %T: %v", err, err)
	}
}
