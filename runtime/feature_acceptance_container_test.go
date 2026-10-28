package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFeatureAcceptanceInARealContainer runs a feature-specific manifest through
// the pinned assurance image, against an isolated fixture base and the exact
// candidate, and REQUIRES a genuine passing verdict for one candidate and a
// genuine failing verdict for another. It is opt-in for the reasons
// TestBrowserAcceptanceInARealContainer is (Docker, the pinned image, a module
// cache), and a skip is not a pass:
//
//	ZENCHRON_BROWSER_IMAGE=sha256:<image id> ZENCHRON_MODCACHE=$HOME/.zenchron/modcache \
//	  go test ./runtime -run TestFeatureAcceptanceInARealContainer -count=1 -v
//
// The fixture is a clone of this repository with one commit added as the BASE:
// browser-acceptance/delivery-report.txt naming two of the repository's real
// browser tests. The passing candidate is a child of that base with an unrelated
// file added (so base and candidate are different trees); the failing candidate
// is a child that changes the text TestBrowserWorkspace asserts the dashboard
// shows, so a real browser really fails a real assertion. No verifier double is
// involved.
func TestFeatureAcceptanceInARealContainer(t *testing.T) {
	image, modcache := os.Getenv("ZENCHRON_BROWSER_IMAGE"), os.Getenv("ZENCHRON_MODCACHE")
	if image == "" || modcache == "" {
		t.Skip("SKIPPED, NOT PASSED: ZENCHRON_BROWSER_IMAGE and ZENCHRON_MODCACHE name a browser image and a module cache; unset")
	}
	repository, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	if out, err := exec.Command("git", "clone", "-q", repository, checkout).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	git := func(args ...string) string {
		t.Helper()
		full := append([]string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false"}, args...)
		out, err := exec.Command("git", append([]string{"-C", checkout}, full...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(checkout, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const feature = "delivery-report"
	selection := []string{"TestBrowserWorkspace", "TestReactEngineeringOSGovernedWorkspace"}
	origin := git("rev-parse", "HEAD")

	write(featureManifestFile(feature), strings.Join(selection, "\n")+"\n")
	git("add", "-A")
	git("commit", "-q", "-m", "fixture base: the feature manifest")
	base := git("rev-parse", "HEAD")

	write("docs/feature-fixture-note.txt", "an unrelated change; the feature tests are the base's\n")
	git("add", "-A")
	git("commit", "-q", "-m", "passing candidate")
	passing, passingTree := git("rev-parse", "HEAD"), git("rev-parse", "HEAD^{tree}")

	git("checkout", "-q", "--detach", base)
	console, err := os.ReadFile(filepath.Join(checkout, "controlplane/assets/static/console.js"))
	if err != nil || !strings.Contains(string(console), "No templates configured.") {
		t.Fatalf("the fixture's failing change has nothing to change: %v", err)
	}
	write("controlplane/assets/static/console.js", strings.Replace(string(console), "No templates configured.", "Templates are absent.", 1))
	git("add", "-A")
	git("commit", "-q", "-m", "failing candidate")
	failing, failingTree := git("rev-parse", "HEAD"), git("rev-parse", "HEAD^{tree}")
	t.Logf("image=%s origin=%s base=%s passing=%s failing=%s", image, origin, base, passing, failing)

	storeRoot := t.TempDir()
	verifier := BaselineGoVerifier{
		Sandbox:            DockerSandbox{Image: image, Executor: OSCommandExecutor{}, Grace: 30 * time.Second},
		ArtifactStore:      ArtifactStore{Root: storeRoot},
		DependencyCacheDir: modcache,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	run := func(name, commit, tree string) (AssuranceRequest, AssuranceResult) {
		git("checkout", "-q", "--detach", commit)
		request := AssuranceRequest{
			RunID: name, Attempt: 1, Commit: commit, Tree: tree, CheckoutDir: checkout, Contract: Ref{ID: "contract", Revision: "1"},
			FeatureAcceptance: feature, FeatureAcceptanceBase: base,
		}
		result, err := verifier.runBrowserCheck(ctx, request, AssuranceResult{Passed: true}, featureSpec(request))
		t.Logf("%s: passed=%v class=%v err=%v", name, result.Passed, result.FailureClass, err)
		if err != nil {
			t.Fatalf("%s: the verifier did not reach a verdict: %v", name, err)
		}
		if len(result.Checks) != 1 || result.Checks[0].Kind != CheckKindFeatureAcceptance {
			t.Fatalf("%s: want exactly one feature_acceptance record, got %+v", name, result.Checks)
		}
		if err := validateVerificationChecks(result.Checks); err != nil {
			t.Fatalf("%s: the journal would refuse the record: %v", name, err)
		}
		return request, result
	}
	expected := featureExpectation{Selection: browserSelectionDigest(selection), Total: len(selection)}

	// A genuine PASS: both tests passed, none skipped, bound to this candidate.
	_, pass := run("run-feature-pass", passing, passingTree)
	if !pass.Passed {
		t.Fatalf("the passing candidate was judged: class=%v checks=%+v", pass.FailureClass, pass.Checks)
	}
	record := pass.Checks[0]
	t.Logf("pass record: results=%+v selection=%s surface=%s artifacts=%s", *record.Results, record.Selection, record.Surface, record.ArtifactsSHA256)
	if record.Outcome != CheckOutcomePassed || *record.Results != (CheckResults{Total: 2, Passed: 2}) {
		t.Fatalf("not every selected test passed, with none failed or skipped: %+v", record)
	}
	if record.Feature != feature || record.Commit != passing || record.Tree != passingTree || record.BaseRevision != base ||
		record.CheckerRevision != featureCheckerRevision() || record.Checker != featureCheckerID || record.Selection != expected.Selection {
		t.Fatalf("the record is not exact-bound to the feature, candidate, base and manifest: %+v", record)
	}
	observation := &AssuranceObservation{AssuranceObservedPayload: AssuranceObservedPayload{Passed: true, Commit: passing, Tree: passingTree, Checks: pass.Checks}}
	if err := provesFeatureAcceptance(checkout, base, feature, passing, passingTree, pass.Checks); err != nil {
		t.Fatalf("the verifier boundary refuses its own genuine record: %v", err)
	}
	if !featureCheckProves(observation, passing, base, feature, expected) {
		t.Fatal("the genuine record does not prove the feature")
	}
	for name, proves := range map[string]bool{
		"another base":      featureCheckProves(observation, passing, passing, feature, expected),
		"another commit":    featureCheckProves(observation, failing, base, feature, expected),
		"another feature":   featureCheckProves(observation, passing, base, "another-feature", expected),
		"another selection": featureCheckProves(observation, passing, base, feature, featureExpectation{Selection: strings.Repeat("0", 64), Total: 2}),
	} {
		if proves {
			t.Errorf("the genuine record proves the feature for %s", name)
		}
	}
	if browserCheckProves(observation, passing, base) {
		t.Error("a feature record was read as the ordinary browser claim")
	}
	assertRetainedArtifacts(t, filepath.Join(storeRoot, "feature", "run-feature-pass-"+passing+"-1-false", "artifacts"), record, selection)

	// A genuine FAIL: a real browser failed a real assertion about the candidate.
	_, fail := run("run-feature-fail", failing, failingTree)
	if fail.Passed || fail.FailureClass != FailureFeatureAcceptance {
		t.Fatalf("the failing candidate was not judged a feature failure: passed=%v class=%v", fail.Passed, fail.FailureClass)
	}
	failure := fail.Checks[0]
	t.Logf("fail record: results=%+v selection=%s artifacts=%s", *failure.Results, failure.Selection, failure.ArtifactsSHA256)
	if failure.Outcome != CheckOutcomeFailed || failure.Results.Total != 2 || failure.Results.Failed < 1 || failure.Results.Skipped != 0 ||
		failure.Results.Passed+failure.Results.Failed != 2 {
		t.Fatalf("the failure is not a complete judged verdict (failed tests, none skipped): %+v", failure.Results)
	}
	if failure.Feature != feature || failure.Commit != failing || failure.Tree != failingTree || failure.BaseRevision != base || failure.Selection != expected.Selection {
		t.Fatalf("the failing record is not exact-bound: %+v", failure)
	}
	failed := &AssuranceObservation{AssuranceObservedPayload: AssuranceObservedPayload{Commit: failing, Tree: failingTree, FailureClass: FailureFeatureAcceptance, Checks: fail.Checks}}
	if featureCheckProves(failed, failing, base, feature, expected) {
		t.Fatal("a failed record proves the feature")
	}
	var transcript []byte
	for _, artifact := range fail.Artifacts {
		if strings.HasSuffix(artifact.Path, "raw.log") {
			if transcript, err = os.ReadFile(artifact.Path); err != nil {
				t.Fatalf("the failure's transcript is unreadable: %v", err)
			}
		}
	}
	if len(transcript) == 0 {
		t.Fatal("the failure keeps no transcript to show a remediation")
	}
	if !strings.Contains(string(transcript), "FAIL agent configuration exposes profiles and template empty states: empty template configuration must be explicit") {
		t.Errorf("the failure transcript does not show the assertion the candidate broke:\n%.2000s", transcript)
	}
}
