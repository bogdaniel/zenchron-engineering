package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The built-in verifier returns a judged failure as a result, not an error, so
// it reaches the single confirmation rerun. A start that never ran the workload
// stays an infrastructure error.
func TestBaselineVerifierReturnsAJudgedFailureWithoutAnError(t *testing.T) {
	for name, c := range map[string]struct {
		exit    int
		want    FailureClass
		wantErr bool
	}{
		"workload exited non-zero":   {1, FailureVerification, false},
		"workload exited 2":          {2, FailureVerification, false},
		"first non-verdict exit":     {3, FailureTransientInfrastructure, true},
		"docker's own failure":       {125, FailureTransientInfrastructure, true},
		"workload not executable":    {126, FailureTransientInfrastructure, true},
		"workload killed by signal":  {137, FailureTransientInfrastructure, true},
		"start failed, no workload":  {-1, FailureTransientInfrastructure, true},
		"workload could not execute": {127, FailureTransientInfrastructure, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "checkout")
			commit := initFixtureRepo(t, dir, "go.mod", "module x\n")
			tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
			if err != nil {
				t.Fatal(err)
			}
			docker := &scriptedDocker{startExits: map[int]int{2: c.exit}}
			v := scriptedVerifier(t, docker, t.TempDir())
			result, err := v.Assure(context.Background(), AssuranceRequest{RunID: "run-1", Attempt: 1, Commit: commit, Tree: strings.TrimSpace(tree), CheckoutDir: dir, Contract: Ref{ID: "contract", Revision: "1"}})
			if result.Passed || result.FailureClass != c.want || (err != nil) != c.wantErr {
				t.Fatalf("classified %q passed=%v err=%v, want %q err=%v", result.FailureClass, result.Passed, err, c.want, c.wantErr)
			}
		})
	}
}

// End to end through the built-in verifier: fail -> fail reaches the
// confirmation and remediates; fail -> pass is flaky and stops.
func TestBaselineVerifierFailuresReachTheConfirmationRerun(t *testing.T) {
	t.Run("fail then fail remediates", func(t *testing.T) {
		docker := &scriptedDocker{startExits: map[int]int{2: 1, 4: 1}}
		f := goModFixture(t, docker)
		f.distinctMutations()
		// Docker starts seen when remediation begins: preparation and
		// verification for the first run and for exactly one confirmation.
		mutate, executions, startsAtRemediation := f.provider.mutate, 0, -1
		f.provider.mutate = func(dir string) error {
			if executions++; executions == 2 {
				startsAtRemediation = docker.count("start")
			}
			return mutate(dir)
		}
		runID := f.start()
		for pass := 0; pass < 30 && len(f.provider.requests) < 2; pass++ {
			f.reconcile(runID)
		}
		if startsAtRemediation != 4 {
			t.Fatalf("remediation began after %d docker starts, want 4 (one run and exactly one confirmation)", startsAtRemediation)
		}
		observed := automatedAssurancePayloads(t, f.state(runID).events)
		// The journalled verdict is the confirmation's own transcript.
		if len(observed) == 0 || observed[0].FailureClass != FailureVerification || !strings.HasSuffix(strings.TrimSuffix(observed[0].ArtifactRef, "/attempt-1"), "-confirmation") {
			t.Fatalf("a genuine failure was not confirmed as verification: %+v", observed)
		}
		if len(f.provider.requests) < 2 {
			t.Fatalf("a confirmed genuine failure did not remediate: %d executions", len(f.provider.requests))
		}
	})
	t.Run("fail then pass is flaky", func(t *testing.T) {
		docker := &scriptedDocker{startExits: map[int]int{2: 1}}
		f := goModFixture(t, docker)
		runID := f.start()
		var outcome Outcome
		for pass := 0; pass < 30 && outcome.Disposition != Failed; pass++ {
			outcome = f.reconcile(runID)
		}
		state := f.state(runID)
		observed := automatedAssurancePayloads(t, state.events)
		if len(observed) == 0 || observed[0].FailureClass != FailureFlaky || observed[0].Passed {
			t.Fatalf("fail then pass was not flaky: %+v", observed)
		}
		if len(state.projection.EvidenceBundles) != 0 || outcome.Reason != OpAssuranceGo+"_failure_not_retryable" {
			t.Fatalf("a flaky built-in verification bound evidence or did not stop: %+v %+v", state.projection.EvidenceBundles, outcome)
		}
	})
}

// Only two candidate verdicts can disagree as a flake. An infrastructure
// result on either run routes by its own class.
func TestAnInfrastructureResultIsNeverHalfOfAFlake(t *testing.T) {
	infra := AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureTransientInfrastructure}
	verdict := AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureVerification}
	for name, results := range map[string][]AssuranceResult{
		"infra then verdict": {infra, verdict},
		"verdict then infra": {verdict, infra},
	} {
		t.Run(name, func(t *testing.T) {
			result, class, err := AssuranceRerun(context.Background(), &FakeAssuranceProvider{Results: results}, AssuranceRequest{})
			if err != nil || class != FailureTransientInfrastructure || result.Passed {
				t.Fatalf("routed as %q (passed=%v, %v), want its own %q", class, result.Passed, err, FailureTransientInfrastructure)
			}
		})
	}
	// An unpassed verdict naming no class IS verification: the pair agrees.
	unclassified := AssuranceResult{ProviderID: "v", VerifierDefinition: "v1"}
	if _, class, err := AssuranceRerun(context.Background(), &FakeAssuranceProvider{Results: []AssuranceResult{unclassified, verdict}}, AssuranceRequest{}); err != nil || class != FailureVerification {
		t.Fatalf("an unclassified failure confirmed by verification_failure routed as %q (%v), want %q", class, err, FailureVerification)
	}
	if !CandidateVerdict(AssuranceResult{Passed: true}) || !CandidateVerdict(verdict) || CandidateVerdict(infra) {
		t.Fatal("CandidateVerdict misreads a verdict")
	}
}
