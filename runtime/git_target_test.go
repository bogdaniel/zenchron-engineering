package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// initTargetRepo makes one ordinary repository with a commit in it.
func initTargetRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "-q", dir); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][]string{{"config", "user.name", "lab"}, {"config", "user.email", "lab@example.invalid"}} {
		if _, err := runGit(dir, pair...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "--no-gpg-sign", "-m", "base"); err != nil {
		t.Fatal(err)
	}
}

// classifyContext is the decision under test, asked the way the broker will ask
// it: resolve the repository an execution context reaches, then compare it to
// anchors the runtime established.
func classifyContext(t *testing.T, cwd, candidateDir, scratchDir string) GitTargetClass {
	t.Helper()
	identity, err := resolveRepoIdentity(cwd)
	if err != nil {
		return GitTargetExternalOrUnknown
	}
	return ClassifyGitTarget(identity, EstablishGitAuthorityAnchors(candidateDir, scratchDir))
}

// TestTheResourceMatrix is #257's claim, stated as a table.
//
// Authority follows the protected RESOURCE, so every row names an execution
// context and the resource it must resolve to. The classification is asserted
// DIRECTLY rather than inferred from whether some command happened to pass -
// inferring it is what let the original defect stay invisible through a whole
// production run.
//
// Nothing in this slice changes what is permitted. These are the answers the
// authority matrix will later be built on.
func TestTheResourceMatrix(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, candidate)

	// Inside the candidate: a subdirectory, a symlink onto it, a nested
	// repository, and a linked worktree sharing the common git directory.
	if err := os.MkdirAll(filepath.Join(candidate, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinked := filepath.Join(root, "link-to-candidate")
	if err := os.Symlink(candidate, symlinked); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(candidate, "nested")
	initTargetRepo(t, nested)
	linkedWorktree := filepath.Join(root, "linked-worktree")
	if _, err := runGit(candidate, "worktree", "add", "-q", linkedWorktree, "-b", "wt"); err != nil {
		t.Fatalf("linked worktree fixture: %v", err)
	}

	// Under the attempt's scratch root: what a test's t.TempDir() produces
	// once TMPDIR names that root.
	scratchRepo := filepath.Join(scratch, "TestSomething1234567", "001")
	initTargetRepo(t, scratchRepo)

	// Outside both, on the same filesystem: an operator's unrelated repository.
	unrelated := filepath.Join(t.TempDir(), "unrelated")
	initTargetRepo(t, unrelated)

	for name, tc := range map[string]struct {
		cwd  string
		want GitTargetClass
	}{
		"candidate exact worktree":            {candidate, GitTargetCandidate},
		"candidate subdirectory":              {filepath.Join(candidate, "subdir"), GitTargetCandidate},
		"candidate through a symlink":         {symlinked, GitTargetCandidate},
		"nested repo physically under it":     {nested, GitTargetCandidate},
		"linked worktree, shared common dir":  {linkedWorktree, GitTargetCandidate},
		"repo under the attempt scratch root": {scratchRepo, GitTargetRuntimeScratch},
		"unrelated operator repository":       {unrelated, GitTargetExternalOrUnknown},
		"a directory that is no repository":   {scratch, GitTargetExternalOrUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyContext(t, tc.cwd, candidate, scratch); got != tc.want {
				t.Fatalf("target=%s, want %s", got, tc.want)
			}
		})
	}
}

// TestAmbiguityNeverResolvesToScratch pins the direction every failure must
// fall in.
//
// runtime_scratch is the only class this model will eventually permit mutation
// in, so it is the one answer that must never be reached by accident. Every
// unresolvable, moved or half-outside case has to land elsewhere.
func TestAmbiguityNeverResolvesToScratch(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, candidate)
	scratchRepo := filepath.Join(scratch, "fixture")
	initTargetRepo(t, scratchRepo)

	// NO SCRATCH GRANT IS NOT A DEFAULT. The same repository, with the runtime
	// having granted no scratch root, has no scratch authority.
	if got := classifyContext(t, scratchRepo, candidate, ""); got == GitTargetRuntimeScratch {
		t.Fatal("scratch authority was assumed without a runtime grant")
	}
	// A CONTEXT THAT IS NO REPOSITORY resolves to nothing.
	if got := classifyContext(t, t.TempDir(), candidate, scratch); got != GitTargetExternalOrUnknown {
		t.Fatalf("a non-repository context classified %s", got)
	}
	// AN ANCHOR THAT MOVED cannot answer, and the answer is not scratch.
	moved := filepath.Join(root, "scratch-moved")
	if err := os.Rename(scratch, moved); err != nil {
		t.Fatal(err)
	}
	if got := classifyContext(t, filepath.Join(moved, "fixture"), candidate, scratch); got == GitTargetRuntimeScratch {
		t.Fatal("a scratch root that had been replaced still conferred scratch authority")
	}
	if err := os.Rename(moved, scratch); err != nil {
		t.Fatal(err)
	}
	// A REPLACED ANCHOR at the same path is a different resource. Identity is
	// the inode, so an attacker-supplied directory standing where the grant was
	// does not inherit its authority.
	if err := os.Rename(scratch, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, filepath.Join(scratch, "fixture"))
	anchors := EstablishGitAuthorityAnchors(candidate, moved)
	identity, err := resolveRepoIdentity(filepath.Join(scratch, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyGitTarget(identity, anchors); got == GitTargetRuntimeScratch {
		t.Fatal("a repository outside the granted root inherited scratch authority by standing at its old path")
	}
}

// TestCandidateIdentityIsAPair records why the candidate test looks at the work
// tree as well as the git directory.
//
// The redirection flags that produce a mismatch are refused outright today, so
// this is not reachable through the broker - it is the property that makes the
// model correct if they are ever relaxed, and losing it silently later would be
// hard to notice.
func TestCandidateIdentityIsAPair(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	other := filepath.Join(root, "other")
	initTargetRepo(t, candidate)
	initTargetRepo(t, other)

	candidateIdentity, err := resolveRepoIdentity(candidate)
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity, err := resolveRepoIdentity(other)
	if err != nil {
		t.Fatal(err)
	}
	anchors := EstablishGitAuthorityAnchors(candidate, "")

	// The shape that destroys candidate work: an unrelated git directory, the
	// candidate's work tree.
	mismatched := repoIdentity{
		CommonDir: otherIdentity.CommonDir, CommonID: otherIdentity.CommonID,
		WorkTree: candidateIdentity.WorkTree, WorkID: candidateIdentity.WorkID,
	}
	if got := ClassifyGitTarget(mismatched, anchors); got != GitTargetCandidate {
		t.Fatalf("a candidate work tree behind an unrelated git directory classified %s", got)
	}
	// And the ancestry direction: a work tree that CONTAINS the candidate
	// brings it into scope.
	ancestor, err := canonicalPath(root)
	if err != nil {
		t.Fatal(err)
	}
	ancestorID, err := identify(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	containing := repoIdentity{
		CommonDir: otherIdentity.CommonDir, CommonID: otherIdentity.CommonID,
		WorkTree: ancestor, WorkID: ancestorID,
	}
	if got := ClassifyGitTarget(containing, anchors); got != GitTargetCandidate {
		t.Fatalf("a work tree containing the candidate classified %s", got)
	}
}
