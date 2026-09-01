package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestFormatFailureUsesDeterministicMutationThenReassesses(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if _, err := runGit("", "init", origin); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := runGit(origin, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(origin, "a.go"), []byte("package a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, _ := gitOutput(origin, "rev-parse", "HEAD")
	workspace, err := CreateCandidateClone(filepath.Join(root, "state"), "run", origin, strings.TrimSpace(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "a.go"), []byte("package a\nfunc x(){ }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	model, policy := runtimeGovernance()
	flow := KernelFlow{}
	state, err := flow.Compile(SourceSnapshot{ID: "issue", Objective: "format", AcceptanceIntent: []string{"formatted"}, PredictedPaths: []string{"a.go"}, PathsKnown: true}, model, policy, "contract", "1")
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	coordinator := MutationCoordinator{Flow: flow, Workspace: &workspace, Repository: model.Subject.Repository, MaxBytes: 4096}
	next, result, err := coordinator.RemediateFormat(context.Background(), GofmtFunc(func(_ context.Context, dir string, paths []string) error {
		called++
		if len(paths) != 1 || paths[0] != "a.go" {
			t.Fatal(paths)
		}
		return os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc x() {}\n"), 0600)
	}), state, model, policy, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || result.Commit == "" || result.Tree == "" {
		t.Fatalf("formatter was not runtime-owned commit: called=%d result=%#v", called, result)
	}
	if !next.Reassessment.Material || next.Contract.Subject.Revision != result.Commit {
		t.Fatalf("format mutation bypassed #8: %#v", next.Reassessment)
	}
}

func TestFailureRoutingFlakyAndNoProgressAreBounded(t *testing.T) {
	if RouteFailure(FailureAuthorityWait) != RouteWait || RouteFailure(FailureUnknown) != RouteStop || RouteFailure(FailureMaterialScope) != RouteReassess {
		t.Fatal("unsafe failure routing")
	}
	provider := &FakeAssuranceProvider{Results: []AssuranceResult{{Passed: false, FailureClass: FailureCompileTest}, {Passed: true}}}
	_, class, err := AssuranceRerun(context.Background(), provider, AssuranceRequest{Commit: "c", Tree: "t"})
	if err != nil || class != FailureFlaky || len(provider.Requests) != 2 {
		t.Fatalf("identical flaky rerun law failed: %v %s %#v", err, class, provider.Requests)
	}
	tracker := NoProgressTracker{Limit: 1}
	fp := FailureFingerprint{CandidateTree: "tree", ContractRevision: "2", FailureSignature: "compile", VerifierIdentity: "v", ProviderIdentity: "p", RemediationIdentity: "r"}
	if !tracker.Allow(fp) || tracker.Allow(fp) {
		t.Fatal("equivalent remediation did not consume no-progress budget")
	}
}

func TestRemediationRequestPreservesFindingsAndAuthorityWaitNeverInvokesProvider(t *testing.T) {
	fake := &FakeExecutionProvider{}
	if RouteFailure(FailureAuthorityWait) == RouteProviderRemediation {
		t.Fatal("authority wait routed to producer")
	}
	_, _ = fake.Execute(context.Background(), ExecutionRequest{Purpose: InvocationRemediation, Findings: []Finding{{Classification: FailureCompileTest, Signature: "compile"}}})
	if fake.Request.Findings[0].Signature != "compile" {
		t.Fatal("remediation finding lost")
	}
	_ = domain.ProducerExecutionProvider // keeps this test about generic provider identity, not Codex.
}

func TestLocalGofmtUsesOnlyRelativeGoPaths(t *testing.T) {
	fake := &fakeCommandExecutor{found: true}
	if err := (LocalGofmt{Executor: fake}).Format(context.Background(), "/candidate", []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	if got := commandText(fake.calls); !strings.Contains(got, "-w a.go") {
		t.Fatalf("unexpected gofmt invocation: %s", got)
	}
	if err := (LocalGofmt{Executor: fake}).Format(context.Background(), "/candidate", []string{"/host.go"}); err == nil {
		t.Fatal("formatter accepted host path")
	}
	if err := (LocalGofmt{Executor: fake}).Format(context.Background(), "/candidate", []string{"../escape.go"}); err == nil {
		t.Fatal("formatter accepted traversal path")
	}
	fake2 := &fakeCommandExecutor{found: true}
	if err := (LocalGofmt{Executor: fake2}).Format(context.Background(), "/candidate", []string{"pkg/file.go"}); err != nil {
		t.Fatal(err)
	}
	if got := commandText(fake2.calls); !strings.Contains(got, "-w pkg/file.go") {
		t.Fatalf("unexpected gofmt invocation: %s", got)
	}
}
