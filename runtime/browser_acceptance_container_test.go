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

// TestBrowserAcceptanceInARealContainer drives the browser step in a real,
// pinned container with real Node, Playwright and Chrome. It is opt-in because
// it needs Docker, an image that provides the three at the documented paths and
// an operator module cache:
//
//	ZENCHRON_BROWSER_IMAGE=sha256:<image id> ZENCHRON_MODCACHE=$HOME/.zenchron/modcache \
//	  go test ./runtime -run TestBrowserAcceptanceInARealContainer -count=1 -v
//
// ZENCHRON_BROWSER_TRANSCRIPT=<file> also keeps the raw container transcript.
//
// It checks the same repository at its current commit as both base and
// candidate, and reports what the container really did. Whether Chrome can
// start under the existing isolation is a property of the image; the test
// states the result rather than assuming it.
func TestBrowserAcceptanceInARealContainer(t *testing.T) {
	image, modcache := os.Getenv("ZENCHRON_BROWSER_IMAGE"), os.Getenv("ZENCHRON_MODCACHE")
	if image == "" || modcache == "" {
		t.Skip("ZENCHRON_BROWSER_IMAGE and ZENCHRON_MODCACHE name a browser image and a module cache; unset")
	}
	repository, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	if out, err := exec.Command("git", "clone", "-q", repository, checkout).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	commit := strings.TrimSpace(mustGitOutput(t, checkout, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(mustGitOutput(t, checkout, "rev-parse", "HEAD^{tree}"))
	storeRoot := t.TempDir()
	verifier := BaselineGoVerifier{
		Sandbox:            DockerSandbox{Image: image, Executor: OSCommandExecutor{}, Grace: 30 * time.Second},
		ArtifactStore:      ArtifactStore{Root: storeRoot},
		DependencyCacheDir: modcache,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	request := AssuranceRequest{
		RunID: "run-real", Attempt: 1, Commit: commit, Tree: tree, CheckoutDir: checkout,
		Contract: Ref{ID: "contract", Revision: "1"}, BrowserAcceptanceBase: commit,
	}
	result, err := verifier.withBrowserAcceptance(ctx, request, AssuranceResult{Passed: true})
	t.Logf("passed=%v class=%v err=%v", result.Passed, result.FailureClass, err)
	for _, check := range browserChecks(result) {
		t.Logf("record: outcome=%s results=%+v selection=%s surface=%s artifacts=%s", check.Outcome, *check.Results, check.Selection, check.Surface, check.ArtifactsSHA256)
	}
	for _, artifact := range result.Artifacts {
		if !strings.HasSuffix(artifact.Path, "raw.log") {
			continue
		}
		if data, err := os.ReadFile(artifact.Path); err == nil {
			if keep := os.Getenv("ZENCHRON_BROWSER_TRANSCRIPT"); keep != "" {
				_ = os.WriteFile(keep, data, 0600)
			}
			if len(data) > 3000 {
				data = data[len(data)-3000:]
			}
			t.Logf("transcript tail:\n%s", data)
		}
	}
	artifacts := filepath.Join(storeRoot, "browser", request.RunID+"-"+commit+"-1-false", "artifacts")
	selection, _ := readBrowserSelection(checkout, commit)
	_, silent, _ := browserArtifacts(artifacts, selection)
	t.Logf("tests that wrote no artifacts: %v", silent)
	if result.Passed && len(browserChecks(result)) != 1 {
		t.Fatalf("a pass without a record: %+v", result.Checks)
	}
	if !result.Passed && len(browserChecks(result)) == 1 && browserChecks(result)[0].Outcome == CheckOutcomePassed {
		t.Fatalf("a failed result carries a passing record: %+v", result.Checks)
	}
}
