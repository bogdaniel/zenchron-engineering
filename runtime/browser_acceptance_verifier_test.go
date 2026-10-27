package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// browserRun drives the real BaselineGoVerifier against a scripted Docker for
// one candidate. The first three container starts are, in order, dependency
// preparation, the Go suite and the browser run.
func browserRun(t *testing.T, docker *scriptedDocker, browser bool) (AssuranceResult, error, *scriptedDocker) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checkout")
	commit := initFixtureRepo(t, dir, "go.mod", "module x\n")
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	verifier := scriptedVerifier(t, docker, t.TempDir())
	result, runErr := verifier.Assure(context.Background(), AssuranceRequest{
		RunID: "run-1", Attempt: 1, Commit: commit, Tree: strings.TrimSpace(tree), CheckoutDir: dir,
		Contract: Ref{ID: "contract", Revision: "1"}, BrowserAcceptance: browser,
	})
	return result, runErr, docker
}

func browserChecks(result AssuranceResult) []VerificationCheck {
	var checks []VerificationCheck
	for _, check := range result.Checks {
		if check.Kind == CheckKindBrowserAcceptance {
			checks = append(checks, check)
		}
	}
	return checks
}

func TestAPassingBrowserRunRecordsExactBoundEvidence(t *testing.T) {
	docker := &scriptedDocker{startOutputs: map[int][]byte{3: goTestJSON(allBrowserTests("pass"))}}
	result, err, _ := browserRun(t, docker, true)
	if err != nil || !result.Passed {
		t.Fatalf("a fully passing browser run failed: passed=%v err=%v", result.Passed, err)
	}
	checks := browserChecks(result)
	if len(checks) != 1 || checks[0].Outcome != CheckOutcomePassed || checks[0].Results.Passed != len(browserSelection) {
		t.Fatalf("no passing record: %+v", result.Checks)
	}
	if checks[0].Commit == "" || checks[0].Tree == "" || checks[0].Selection != browserSelectionDigest() ||
		checks[0].CheckerRevision != browserCheckerRevision() || !strings.HasPrefix(checks[0].Environment, "docker-network-none@") ||
		!strings.Contains(checks[0].Artifact, browserProviderID) {
		t.Fatalf("the record is not bound to its candidate, selection, environment and transcript: %+v", checks[0])
	}
	if err := validateVerificationChecks(result.Checks); err != nil {
		t.Fatalf("the verifier wrote a record the journal would refuse: %v", err)
	}
}

func TestAMissingBrowserFailsClosed(t *testing.T) {
	docker := &scriptedDocker{startExits: map[int]int{3: browserPrerequisiteExit}}
	result, err, _ := browserRun(t, docker, true)
	if err == nil || result.Passed || result.FailureClass != FailureAssurancePrerequisite || len(browserChecks(result)) != 0 {
		t.Fatalf("an image without Chrome or Playwright produced evidence: passed=%v class=%v err=%v checks=%+v", result.Passed, result.FailureClass, err, result.Checks)
	}
}

func TestASkippedOrSilentBrowserRunNeverPasses(t *testing.T) {
	skipped := allBrowserTests("pass")
	skipped[browserSelection[0]] = "skip"
	silent := allBrowserTests("pass")
	delete(silent, browserSelection[3])
	for name, output := range map[string][]byte{"a skipped test": goTestJSON(skipped), "a test that reported nothing": goTestJSON(silent), "no tests at all": []byte("ok\n")} {
		docker := &scriptedDocker{startOutputs: map[int][]byte{3: output}}
		result, err, _ := browserRun(t, docker, true)
		if err == nil || result.Passed || len(browserChecks(result)) != 0 {
			t.Errorf("%s became evidence: passed=%v err=%v checks=%+v", name, result.Passed, err, result.Checks)
		}
	}
}

func TestAFailedBrowserRunBlocksTheCandidate(t *testing.T) {
	docker := &scriptedDocker{startExits: map[int]int{3: 1}}
	result, err, _ := browserRun(t, docker, true)
	checks := browserChecks(result)
	if err != nil || result.Passed || result.FailureClass != FailureVerification || len(checks) != 1 || checks[0].Outcome != CheckOutcomeFailed {
		t.Fatalf("a failed browser run was not judged: passed=%v class=%v err=%v checks=%+v", result.Passed, result.FailureClass, err, result.Checks)
	}
	if result.ArtifactRef == "" || result.FailureSignature == "" {
		t.Fatalf("the failure carries no transcript for remediation: %+v", result)
	}
}

func TestBrowserAcceptanceIsNotRunWhenTheSuiteFailsOrIsNotRequired(t *testing.T) {
	failing := &scriptedDocker{startExits: map[int]int{2: 1}}
	result, _, docker := browserRun(t, failing, true)
	if result.Passed || len(browserChecks(result)) != 0 || docker.count("start") != 2 {
		t.Fatalf("a candidate whose suite failed was sent to the browser: starts=%d %+v", docker.count("start"), result.Checks)
	}
	goOnly := &scriptedDocker{}
	result, err, docker := browserRun(t, goOnly, false)
	if err != nil || !result.Passed || len(result.Checks) != 0 || docker.count("start") != 2 {
		t.Fatalf("work with no browser obligation changed behavior: starts=%d err=%v %+v", docker.count("start"), err, result.Checks)
	}
}
