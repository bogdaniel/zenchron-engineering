package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stagedCommitPaths is what Commit gates: the index written to a tree, diffed
// against HEAD.
func stagedCommitPaths(dir string) ([]string, []stagedBlob, error) {
	tree, err := gitOutput(dir, "write-tree")
	if err != nil {
		return nil, nil, err
	}
	return treeCommitPaths(dir, "HEAD", strings.TrimSpace(tree))
}

func withAfterCommitGates(t *testing.T, hook func(dir string)) {
	t.Helper()
	afterCommitGates = hook
	t.Cleanup(func() { afterCommitGates = nil })
}

// TestCommitCarriesExactlyTheGatedTree is #437: an index rewritten after the
// gates ran cannot change what is committed, so a secret staged in that window
// never lands.
func TestCommitCarriesExactlyTheGatedTree(t *testing.T) {
	w := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "clean.go"), []byte("package clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gated string
	withAfterCommitGates(t, func(dir string) {
		tree, err := gitOutput(dir, "write-tree")
		if err != nil {
			t.Fatal(err)
		}
		gated = strings.TrimSpace(tree)
		if err := os.WriteFile(filepath.Join(dir, "secret.go"), []byte(pemPrivateKeyBlock()), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(dir, "add", "--", "secret.go"); err != nil {
			t.Fatal(err)
		}
	})
	result, _ := w.Commit("gated", 1<<20) // the rewritten index is residue; the refusal is fine
	if result.Commit != strings.TrimSpace(headOf(t, w)) {
		t.Fatalf("Commit did not report the head it made: %q", result.Commit)
	}
	tree, err := gitOutput(w.Dir, "rev-parse", "HEAD^{tree}")
	if err != nil || strings.TrimSpace(tree) != gated || result.Tree != gated {
		t.Fatalf("committed tree %q (result %q) is not the gated tree %q: %v", tree, result.Tree, gated, err)
	}
	if files, _ := gitOutput(w.Dir, "ls-tree", "-r", "--name-only", "HEAD"); strings.Contains(files, "secret.go") {
		t.Fatalf("a blob staged after gating was committed: %q", files)
	}
}

// TestCommitRefusesAConcurrentHeadMove: update-ref moves HEAD only from the
// gated parent, so a HEAD moved in the window is an integrity violation and
// no commit is reported.
func TestCommitRefusesAConcurrentHeadMove(t *testing.T) {
	w := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var moved string
	withAfterCommitGates(t, func(dir string) {
		out, err := runGitInput(dir, []byte("elsewhere\n"), "commit-tree", "HEAD^{tree}", "-p", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		moved = strings.TrimSpace(string(out))
		if _, err := runGit(dir, "update-ref", "HEAD", moved); err != nil {
			t.Fatal(err)
		}
	})
	result, err := w.Commit("raced", 1<<20)
	var integrity *WorkspaceIntegrityError
	if !errors.As(err, &integrity) || result.Commit != "" {
		t.Fatalf("a concurrent HEAD move was not refused as an integrity violation: %+v %v", result, err)
	}
	if head := strings.TrimSpace(headOf(t, w)); head != moved {
		t.Fatalf("the runtime overwrote a concurrently moved HEAD: %s", head)
	}
}

// TestCommitGatesIgnoreReplaceObjects: a refs/replace ref mapping a credential
// blob to a harmless one must not make the gates read the stand-in while the
// commit carries the real blob. The ref is created and the trusted baseline
// re-taken, which is the state of a ref created just after AssertIntegrity.
func TestCommitGatesIgnoreReplaceObjects(t *testing.T) {
	w := commitGateWorkspace(t)
	before := strings.TrimSpace(headOf(t, w))
	key := []byte(pemPrivateKeyBlock())
	if err := os.WriteFile(filepath.Join(w.Dir, "key.go"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := runGitInput(w.Dir, key, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	fake, err := runGitInput(w.Dir, []byte("package harmless\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, w.Dir, "replace", strings.TrimSpace(string(real)), strings.TrimSpace(string(fake)))
	if w.TrustedMetadata, err = gitMetadataDigest(w.Dir); err != nil {
		t.Fatal(err)
	}
	_, err = w.Commit("replaced", 1<<20)
	var material *CredentialMaterialError
	if !asCredentialMaterial(err, &material) || material.Path != "key.go" {
		t.Fatalf("the gates judged a replace-object stand-in: %v", err)
	}
	out, gitErr := exec.Command("git", "-C", w.Dir, "--no-replace-objects", "rev-parse", "HEAD").Output()
	if gitErr != nil || strings.TrimSpace(string(out)) != before {
		t.Fatalf("a commit was made despite the refusal: %s %v", out, gitErr)
	}
}

// TestCommitPathsAreTheTreeDiff: a staged rename is recorded as its delete and
// add, as #431 recovery's diffPaths --no-renames recomputes it.
func TestCommitPathsAreTheTreeDiff(t *testing.T) {
	w := commitGateWorkspace(t)
	if _, err := runGit(w.Dir, "mv", "README.md", "docs.md"); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("rename", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want, err := diffPaths(w.Dir, result.Commit+"^", result.Commit, "--no-renames")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Paths, ",") != strings.Join(want, ",") || len(want) != 2 {
		t.Fatalf("recorded paths %q are not the tree diff %q", result.Paths, want)
	}
}

// TestCommitRefusesAnEmptyTreeDiff: a status path that stages nothing (an
// intent-to-add entry whose file is gone) must not mint an empty commit.
func TestCommitRefusesAnEmptyTreeDiff(t *testing.T) {
	w := commitGateWorkspace(t)
	before := headOf(t, w)
	if err := os.WriteFile(filepath.Join(w.Dir, "f.go"), []byte("package f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, w.Dir, "add", "-N", "f.go")
	if err := os.Remove(filepath.Join(w.Dir, "f.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("empty", 1<<20); err == nil || !strings.Contains(err.Error(), "stage nothing") {
		t.Fatalf("an empty tree diff was not refused: %v", err)
	}
	if headOf(t, w) != before {
		t.Fatal("HEAD moved on an empty tree diff")
	}
}
