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

// TestCandidateGuardRejectsCredentialFilesAndSymlinks pins what the PATH gate
// answers, which is no longer "does this file contain a credential?".
//
// It used to refuse any file whose bytes contained "github_pat_", which made
// the source of a secret scanner unreadable while candidate.run printed the
// same file anyway. Credential VALUES are refused where that refusal is
// enforceable: provider admission, tool-result redaction and the commit gate.
// See credential_boundary_test.go.
func TestCandidateGuardRejectsCredentialFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "token.txt"), []byte("github_pat_secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := GuardCandidate(root, []string{"token.txt"}, 1024); err != nil {
		t.Fatalf("the path gate refused a file that merely names a token format: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "id_rsa"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := GuardCandidate(root, []string{"id_rsa"}, 1024); err == nil {
		t.Fatal("credential file name accepted")
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

// workerTestScratchRepository builds what a killed attempt leaves behind: the
// worker's own `go test` temporary tree, inside the candidate workspace,
// holding an assurance checkout - which is a real repository with a real commit
// and a worktree of its own that keeps changing.
//
// The caller names the path, because production had two such roots and six such
// repositories; the shape is what matters either way, a directory Git will not
// descend into and cannot commit the contents of.
func workerTestScratchRepository(t *testing.T, workspace, rel string, dirty bool) string {
	t.Helper()
	nested := filepath.Join(workspace, rel)
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", nested); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "scratch"}, {"config", "user.email", "scratch@example.invalid"}} {
		if _, err := runGit(nested, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(nested, "checked-out.txt"), []byte("assurance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(nested, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(nested, "commit", "--no-gpg-sign", "-m", "assurance checkout"); err != nil {
		t.Fatal(err)
	}
	// A killed attempt leaves SOME of its nested trees half-finished. Only
	// those make the parent report the gitlink as modified; a clean one is
	// reported nowhere, which is the silent half of the same defect.
	if dirty {
		if err := os.WriteFile(filepath.Join(nested, "checked-out.txt"), []byte("assurance half-written\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.ToSlash(rel)
}

// TestCandidateCommitRefusesAKilledAttemptsTestScratchRepository is issue #189.
//
// Recovery reuses the candidate workspace, so the killed attempt's own `go
// test` scratch is still in it. `git add -A` recorded that scratch as a gitlink,
// the post-commit cleanliness probe then read the gitlink as modified, and the
// runtime refused a commit it had already written - deterministically, on every
// remaining attempt.
//
// The runtime must refuse the unrepresentable path itself, by name, and must
// not write the commit first.
func TestCandidateCommitRefusesAKilledAttemptsTestScratchRepository(t *testing.T) {
	w := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "safe.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Production had six of these, across two in-tree scratch roots, and only
	// one of them was dirty enough for the probe to see. An operator shown a
	// single example of a workspace-wide condition goes looking for a one-off,
	// so every offending path has to be named.
	dirty := workerTestScratchRepository(t, w.Dir, filepath.Join(".validation-tmp",
		"TestM_RestartPreservesTheExactEvidenceAndItsReason266398414", "001", "state",
		"runs", "run-63ca61c8e818d9c361958e04afdcee8a", "assurance", "0077b658-1"), true)
	clean := workerTestScratchRepository(t, w.Dir, filepath.Join("t", "fixture-origin"), false)
	before, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Commit("runtime candidate", 1<<20)
	if err == nil {
		t.Fatal("the runtime committed a workspace holding a nested repository")
	}
	for _, want := range []string{dirty, clean} {
		// Quoted: neither `status -z` nor `ls-files -z` quotes a path, and this
		// refusal is journalled, so a producer-chosen directory name holding a
		// newline would otherwise write its own line into runtime evidence.
		if !strings.Contains(err.Error(), "\""+want+"\"") {
			t.Fatalf("the refusal does not name %q, which cannot be committed: %v", want, err)
		}
	}
	after, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatal("the runtime wrote a commit it then refused")
	}
}

// TestCandidateCommitStillCarriesAGenuineChange is the other half of the same
// repair: the refusal must not have been bought by ignoring anything.
//
// A modified tracked file, a new file, and an untracked directory that merely
// looks like scratch are all committed and all reported, so what reassessment
// observes is still everything the producer did.
func TestCandidateCommitStillCarriesAGenuineChange(t *testing.T) {
	w := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "safe.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	looksLikeScratch := filepath.Join(".validation-tmp", "leftover")
	if err := os.MkdirAll(filepath.Join(w.Dir, looksLikeScratch), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, looksLikeScratch, "note.txt"), []byte("plain\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := w.ObservedChange(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README.md", "safe.txt", filepath.ToSlash(filepath.Join(looksLikeScratch, "note.txt"))} {
		if !contains(result.Paths, want) {
			t.Fatalf("the commit result dropped %q: %q", want, result.Paths)
		}
		if !contains(observed.Paths, want) {
			t.Fatalf("reassessment cannot see %q: %q", want, observed.Paths)
		}
		content, err := gitOutput(w.Dir, "show", "HEAD:"+want)
		if err != nil || strings.TrimSpace(content) == "" {
			t.Fatalf("%q is not in the commit: %v", want, err)
		}
	}
}

// TestCandidateCommitRefusesAGitlinkAlreadyRecordedInTheIndex is the silent half
// of issue #189, and the half nothing was catching.
//
// Five of the six gitlinks in commit f0f72ba had clean nested worktrees. A clean
// one is in no changed path, so the workspace reports nothing, the cleanliness
// probe passes, and the runtime publishes a tree whose recorded paths hold no
// content - `git show HEAD:<path>` answers "exists on disk, but not in HEAD".
// Refusing only what the worktree reports would leave that latent.
func TestCandidateCommitRefusesAGitlinkAlreadyRecordedInTheIndex(t *testing.T) {
	w := commitGateWorkspace(t)
	gitlink := workerTestScratchRepository(t, w.Dir, filepath.Join("t", "fixture-origin"), false)
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", "an earlier commit recorded the gitlink"); err != nil {
		t.Fatal(err)
	}
	status, err := gitOutput(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(status) != "" {
		t.Fatalf("the fixture is not the silent case: %q", status)
	}
	// The fixture commit is the runtime's own history, not a producer touching
	// Git behind its back, so the integrity baseline is taken after it.
	if w.TrustedMetadata, err = gitMetadataDigest(w.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "safe.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("the runtime published a tree that does not hold the content of a recorded gitlink")
	} else if !strings.Contains(err.Error(), gitlink) {
		t.Fatalf("the refusal does not name the recorded gitlink: %v", err)
	}
	after, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatal("the runtime wrote a commit it then refused")
	}
}

// TestCandidateCommitPermitsRemovingARecordedGitlink pins that the refusal does
// not block its own repair.
//
// Removing the nested directory and committing that removal is the way OUT of a
// workspace that holds a recorded gitlink: the resulting tree has no gitlink in
// it and the workspace is clean afterwards. A guard that refuses the removal
// leaves the workspace permanently uncommittable, which is a worse place than
// the one it was protecting against.
//
// The exemption closes nothing, and the test says which weaker one it is not: a
// gitlink whose worktree path is gone cannot survive `add -A`, whereas a gitlink
// whose path is still there is refused however the directory now looks - which
// is what stops a producer staging a gitlink and renaming the nested .git away.
func TestCandidateCommitPermitsRemovingARecordedGitlink(t *testing.T) {
	w := commitGateWorkspace(t)
	gitlink := workerTestScratchRepository(t, w.Dir, filepath.Join("t", "fixture-origin"), false)
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", "an earlier commit recorded the gitlink"); err != nil {
		t.Fatal(err)
	}
	var err error
	if w.TrustedMetadata, err = gitMetadataDigest(w.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(w.Dir, filepath.FromSlash(gitlink))); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("remove the recorded gitlink", 1<<20)
	if err != nil {
		t.Fatalf("the guard refused its own repair: %v", err)
	}
	if !contains(result.Paths, gitlink) {
		t.Fatalf("the removal was not reported: %q", result.Paths)
	}
	staged, err := gitOutput(w.Dir, "ls-files", "--stage")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(staged, "160000") {
		t.Fatalf("the gitlink survived the removal: %q", staged)
	}
	status, err := gitOutput(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(status) != "" {
		t.Fatalf("the workspace is not clean after the repair: %q", status)
	}
}

// TestCandidateCommitRefusesAStagedGitlinkWhoseRepositoryWasHidden is the
// hostile case, and the one that overturned an earlier certification that a
// producer could not use a nested repository to hide a change.
//
// Two ordinary commands do it. `git add nested` stages a gitlink; renaming
// nested/.git away then makes every worktree predicate answer wrongly, while
// `add -A` leaves the staged gitlink alone. The path IS observed - status
// reports it, so reassessment is told it changed - but the commit carries a
// gitlink no object store can resolve and none of the producer's files.
// AssertIntegrity does not see it either, because `git add` touches only the
// index. Only the index can answer this.
func TestCandidateCommitRefusesAStagedGitlinkWhoseRepositoryWasHidden(t *testing.T) {
	w := commitGateWorkspace(t)
	hidden := workerTestScratchRepository(t, w.Dir, "nested", false)
	if _, err := runGit(w.Dir, "add", "--", hidden); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(w.Dir, hidden)
	if err := os.Rename(filepath.Join(nested, ".git"), filepath.Join(nested, "dot-git")); err != nil {
		t.Fatal(err)
	}
	if err := w.AssertIntegrity(); err != nil {
		t.Fatalf("the fixture tripped the integrity baseline, so it is not the case under test: %v", err)
	}
	before, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("a producer hid its change behind a staged gitlink")
	} else if !strings.Contains(err.Error(), hidden) {
		t.Fatalf("the refusal does not name the staged gitlink: %v", err)
	}
	after, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatal("the runtime wrote a commit it then refused")
	}
}
