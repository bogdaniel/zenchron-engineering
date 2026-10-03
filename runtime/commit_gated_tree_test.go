package runtime

import (
	"errors"
	"os"
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
