package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestBrowserAcceptanceInARealContainer drives the browser step in a real,
// pinned container with real Node, Playwright and Chrome, and REQUIRES it to
// succeed. It is opt-in because it needs Docker, an image that provides Node 22
// or later, Playwright, Chrome, tar and base64 at the documented paths, and an
// operator module cache:
//
//	ZENCHRON_BROWSER_IMAGE=sha256:<image id> ZENCHRON_MODCACHE=$HOME/.zenchron/modcache \
//	  go test ./runtime -run TestBrowserAcceptanceInARealContainer -count=1 -v
//
// Without those variables the test SKIPS and says so; a skip is not browser
// acceptance. With them, anything short of a complete pass fails the test:
// the verifier must succeed, all selected tests must have passed with none
// failed, skipped or silent, the record must be current and exact-bound, and the
// artifacts on the host must be what the record's digest says.
//
// The checkout is this repository's current commit used as both base and
// candidate, because the manifest and the test sources are base-pinned and the
// feature is unmerged. That is fixture proof, not proof against trusted main.
//
// ZENCHRON_BROWSER_TRANSCRIPT=<file> keeps the raw container transcript, which
// is how a failure is diagnosed; ZENCHRON_BROWSER_KEEP_ARTIFACTS=<dir> keeps the
// exported screenshots of a passing run for review.
func TestBrowserAcceptanceInARealContainer(t *testing.T) {
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
	t.Logf("image=%s base=%s candidate=%s tree=%s", image, commit, commit, tree)
	t.Logf("passed=%v class=%v err=%v", result.Passed, result.FailureClass, err)
	for _, artifact := range result.Artifacts {
		if !strings.HasSuffix(artifact.Path, "raw.log") {
			continue
		}
		data, readErr := os.ReadFile(artifact.Path)
		if readErr != nil {
			continue
		}
		if keep := os.Getenv("ZENCHRON_BROWSER_TRANSCRIPT"); keep != "" {
			_ = os.WriteFile(keep, data, 0o600)
		}
		if err != nil || !result.Passed {
			if len(data) > 3000 {
				data = data[len(data)-3000:]
			}
			t.Logf("transcript tail:\n%s", data)
		}
	}
	if err != nil {
		t.Fatalf("the browser verifier did not succeed: %v", err)
	}
	if !result.Passed {
		t.Fatalf("the browser verdict was not a pass: class=%v checks=%+v", result.FailureClass, result.Checks)
	}
	checks := browserChecks(result)
	if len(checks) != 1 || checks[0].Outcome != CheckOutcomePassed {
		t.Fatalf("no single passing browser record: %+v", result.Checks)
	}
	check := checks[0]
	t.Logf("record: results=%+v selection=%s surface=%s artifacts=%s environment=%s", *check.Results, check.Selection, check.Surface, check.ArtifactsSHA256, check.Environment)
	if want := (CheckResults{Total: len(browserSelectionOf(t, checkout, commit)), Passed: len(browserSelectionOf(t, checkout, commit))}); *check.Results != want {
		t.Fatalf("not every selected test passed: %+v, want %+v (no failed, skipped or silent test)", *check.Results, want)
	}
	if check.Commit != commit || check.Tree != tree || check.BaseRevision != commit || check.CheckerRevision != browserCheckerRevision() {
		t.Fatalf("the record is not exact-bound to the candidate and base: %+v", check)
	}
	observation := &AssuranceObservation{AssuranceObservedPayload: AssuranceObservedPayload{Passed: true, Commit: commit, Tree: tree, Checks: result.Checks}}
	if !browserCheckProves(observation, commit, commit) {
		t.Fatalf("the record does not prove the claim for this candidate and base: %+v", check)
	}
	if err := validateVerificationChecks(result.Checks); err != nil {
		t.Fatalf("the journal would refuse the record: %v", err)
	}

	// The artifacts on the host are what the record says: the kept manifest
	// hashes to the record's digest, every retained file is a regular file within
	// the bounds that the manifest lists with the same hash, and every selected
	// test both wrote something and has something retained.
	root := filepath.Join(storeRoot, "browser", request.RunID+"-"+commit+"-1-false", "artifacts")
	manifestText, err := os.ReadFile(filepath.Join(root, browserManifestKept))
	if err != nil {
		t.Fatalf("no kept manifest: %v", err)
	}
	listed := map[string]string{}
	var manifestLines []string
	for _, line := range strings.Split(strings.TrimSpace(string(manifestText)), "\n") {
		hash, name, _ := strings.Cut(line, "  ./")
		listed[name] = hash
		manifestLines = append(manifestLines, name+" "+hash)
	}
	sort.Strings(manifestLines)
	digest := sha256.Sum256([]byte(strings.Join(manifestLines, "\n")))
	if hex.EncodeToString(digest[:]) != check.ArtifactsSHA256 {
		t.Fatalf("the kept manifest does not hash to the record's digest")
	}
	wrote, retained := map[string]int{}, map[string]int{}
	for name := range listed {
		wrote[strings.Split(name, "/")[0]]++
	}
	var total int64
	files := 0
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		relative, _ := filepath.Rel(root, path)
		if walkErr != nil || entry.IsDir() || relative == browserManifestKept {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			t.Errorf("artifact %s is not a regular file", path)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		relative = filepath.ToSlash(relative)
		if listed[relative] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s is not what the manifest lists for it", relative)
		}
		retained[strings.Split(relative, "/")[0]]++
		files++
		total += int64(len(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if keep := os.Getenv("ZENCHRON_BROWSER_KEEP_ARTIFACTS"); keep != "" {
		if out, err := exec.Command("cp", "-R", root, keep).CombinedOutput(); err != nil {
			t.Logf("could not keep the artifacts: %v: %s", err, out)
		}
	}
	for _, name := range browserSelectionOf(t, checkout, commit) {
		if wrote[name] == 0 || retained[name] == 0 {
			t.Errorf("%s wrote %d files and has %d retained; both must be at least one", name, wrote[name], retained[name])
		}
	}
	if files == 0 || files > browserRetainedMaxFiles || total > browserRetainedMaxBytes {
		t.Fatalf("retained artifact evidence out of shape: %d files, %d bytes", files, total)
	}
	t.Logf("artifacts: %d written by the tests, %d retained (%d bytes) across %d tests", len(listed), files, total, len(retained))
}

func browserSelectionOf(t *testing.T, checkout, base string) []string {
	t.Helper()
	names, err := readBrowserSelection(checkout, base)
	if err != nil {
		t.Fatal(err)
	}
	return names
}
