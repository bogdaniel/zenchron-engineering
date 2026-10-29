package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Absence the base CONFIRMS is an empty manifest (only the hard cap then
// applies, as in the checker). A lookup that FAILED is an error: git exits 128
// both for a missing path and for a corrupt object, so only a successful,
// empty tree listing may be read as absence.
func TestAConfirmedAbsentManifestIsEmptyButAFailedLookupIsAnError(t *testing.T) {
	dir := t.TempDir()
	base, baseTree := sizeRepo(t, dir, map[string]string{"a.go": "package a\n"}, "base")
	if limits, err := readFileSizeManifest(dir, base, fileSizeBaselineFile, 2); err != nil || len(limits) != 0 {
		t.Fatalf("a confirmed-absent manifest must be empty and not an error: %v %v", limits, err)
	}
	violations, err := CheckFileSizePolicy(dir, base, baseTree)
	if err != nil || len(violations) != 0 {
		t.Fatalf("a repository with no manifests is judged by the hard cap alone: %v %v", violations, err)
	}

	// The base commit is intact but its root tree object has been lost. The
	// lookup fails; it must not be mistaken for "the base has no manifest".
	_, candidateTree := sizeRepo(t, dir, map[string]string{"grown.go": manyLines(1200)}, "candidate")
	object := filepath.Join(dir, ".git", "objects", baseTree[:2], baseTree[2:])
	if err := os.Remove(object); err != nil {
		t.Fatalf("the fixture could not remove the base tree object: %v", err)
	}
	violations, err = CheckFileSizePolicy(dir, base, candidateTree)
	if err == nil {
		t.Fatalf("a failed manifest lookup was read as absence: %+v", violations)
	}
	if !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), "refusing to treat it as absent") {
		t.Fatalf("the refusal does not say what failed: %v", err)
	}
}

// A manifest that is not a regular file is refused, as the checker refuses it.
func TestAManifestThatIsNotARegularFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("elsewhere.tsv", filepath.Join(dir, fileSizeBaselineFile)); err != nil {
		t.Fatal(err)
	}
	base, tree := sizeRepo(t, dir, map[string]string{"a.go": "package a\n"}, "base")
	if _, err := CheckFileSizePolicy(dir, base, tree); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlinked manifest was accepted: %v", err)
	}
}

// The size check runs on the controller host, before any container. Its
// evidence must say so, and the sandbox's binding must stay the sandbox's.
func TestAHostSideVerdictNamesTheHostAndNeverTheSandbox(t *testing.T) {
	dir := t.TempDir()
	base, _ := sizeRepo(t, dir, map[string]string{"a.go": "package a\n"}, "base")
	commit, tree := sizeRepo(t, dir, map[string]string{"grown.go": manyLines(1200)}, "candidate")
	verifier := BaselineGoVerifier{
		Sandbox:       DockerSandbox{Image: "sha256:pinned-sandbox-image"},
		ArtifactStore: ArtifactStore{Root: t.TempDir()},
	}
	request := AssuranceRequest{
		RunID: "run-host", Commit: commit, Tree: tree, CheckoutDir: dir, Attempt: 1,
		Contract: Ref{ID: "contract-run-host", Revision: "1"}, Policy: Ref{ID: "policy", Revision: "7"},
		FileSizePolicyBase: base,
	}
	result, done, err := verifier.fileSizeVerdict(request)
	if err != nil || !done || result.Evidence == nil {
		t.Fatalf("done=%v err=%v evidence=%v", done, err, result.Evidence)
	}
	host := result.Evidence
	if host.Environment.ID != fileSizeHostEnvironmentID || host.Producer.ID != fileSizeCheckerID {
		t.Fatalf("the evidence does not name the host-side checker: %+v", host)
	}
	if strings.Contains(host.Environment.ID, "docker") || host.Environment.Revision == verifier.Sandbox.Image {
		t.Fatalf("host-side evidence names the sandbox: %+v", host.Environment)
	}
	if host.Producer.Revision != fileSizeCheckerRevision() || host.Producer.Revision == verifier.Definition() {
		t.Fatalf("the producer is not the size checker's own identity: %+v", host.Producer)
	}
	if host.Commit != commit || host.Tree != tree || host.Contract.ID != "contract-run-host" {
		t.Fatalf("the exact bindings were lost: %+v", host)
	}

	sandbox := verifier.sandboxEvidenceBinding(request)
	if sandbox.Environment != (Ref{ID: "docker-network-none", Revision: "sha256:pinned-sandbox-image"}) || sandbox.Producer.ID != "baseline-go" {
		t.Fatalf("the sandbox binding changed: %+v", sandbox)
	}
	if *host == *sandbox {
		t.Fatal("host and sandbox evidence are indistinguishable")
	}
}
