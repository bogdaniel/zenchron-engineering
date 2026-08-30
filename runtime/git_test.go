package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateCloneCommitAndMetadataIntegrity(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if _, err := runGit("", "init", origin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "config", "user.name", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "config", "user.email", "test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := gitOutput(origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateCandidateClone(filepath.Join(root, "state"), "run-1", origin, base[:len(base)-1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "safe.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1024)
	if err != nil || result.Commit == "" || result.Tree == "" {
		t.Fatal(err, result)
	}
	if _, err := runGit(w.Dir, "update-ref", "refs/heads/producer", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := w.AssertIntegrity(); err == nil {
		t.Fatal("accepted producer Git metadata mutation")
	}
}
func TestCandidateGuardRejectsSecretAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "token.txt"), []byte("github_pat_secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := GuardCandidate(root, []string{"token.txt"}, 1024); err == nil {
		t.Fatal("secret content accepted")
	}
	if err := os.Symlink("token.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := GuardCandidate(root, []string{"link"}, 1024); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestChangedPathsHandlesQuotedNonASCIIName(t *testing.T) {
	root := t.TempDir()
	if _, err := runGit("", "init", root); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.name", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.email", "test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	name := "café.go"
	if err := os.WriteFile(filepath.Join(root, name), []byte("package x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := changedPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != name {
		t.Fatalf("want [%q], got %q", name, paths)
	}
}
func TestPrePublicationRebaseReturnsTypedConflict(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if _, err := runGit("", "init", origin); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][]string{{"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := runGit(origin, pair...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := gitOutput(origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := gitOutput(origin, "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateCandidateClone(filepath.Join(root, "state"), "run", origin, strings.TrimSpace(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("candidate", 1024); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("base moved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-am", "move base"); err != nil {
		t.Fatal(err)
	}
	if err := w.FetchBase("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Rebase("origin/" + strings.TrimSpace(branch)); err == nil {
		t.Fatal("expected conflict")
	} else if _, ok := err.(*ConflictError); !ok {
		t.Fatalf("want conflict result: %T %v", err, err)
	}
}
