package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestReviewerReceivesOnlyExactHeadRuntimeAssurance(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: "Implement the change.",
			InvocationMode: domain.InvocationModeMutating, RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer, Objective: "Review the change.",
			InvocationMode: domain.InvocationModeMutating, RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis},
			DependsOn: []string{"implementation"}},
	})
	fixture.approve(t)
	report := fixture.reconcile(t)
	if len(report.Started) != 1 {
		t.Fatalf("implementation stage did not start: %#v", report)
	}
	runID := report.Started[0].RunID
	const candidate = "aaaaaaaaaaaa"
	recordCandidateAndAssurance(t, fixture, runID, candidate)
	runtime := fixture.newRuntime(fixture.deps)
	assurance, err := runtime.upstreamAssurance(domain.UpstreamOutput{RunID: runID, Candidate: candidate, Tree: candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(assurance) != 1 || !assurance[0].Passed || assurance[0].Commit != candidate || assurance[0].Tree != candidate {
		t.Fatalf("exact-head assurance = %#v", assurance)
	}
	stale, err := runtime.upstreamAssurance(domain.UpstreamOutput{RunID: runID, Candidate: "bbbbbbbbbbbb", Tree: "bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("assurance for a different head reached the reviewer: %#v", stale)
	}
}

func TestRuntimeOwnedAssuranceReachesReviewerPrompt(t *testing.T) {
	prompt := agentPrompt(ExecutionRequest{RunID: "run-review", OperationID: "review", Attempt: 1,
		TrustedInstructions: "trusted", Upstream: []UpstreamContext{{
			StageID: "implementation", RunID: "run-impl", Commit: "c0ffee", Tree: "7ree",
			Assurance: []AssuranceContext{{ProviderID: "go", VerifierDefinition: "go-test", Passed: true,
				Commit: "c0ffee", Tree: "7ree", BundleID: "bundle-go", BundleRevision: "1"}},
		}}})
	for _, evidence := range []string{
		"Runtime-owned assurance for stage implementation: provider=go verifier=go-test passed=true",
		"commit=c0ffee tree=7ree bundle=bundle-go/1",
	} {
		if !strings.Contains(prompt, evidence) {
			t.Fatalf("runtime-owned assurance evidence %q did not reach the reviewer:\n%s", evidence, prompt)
		}
	}
}
