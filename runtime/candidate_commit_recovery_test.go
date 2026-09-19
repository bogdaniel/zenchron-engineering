package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Issue #189: crash recovery reuses the damaged candidate workspace.
//
// The killed attempt's own `go test` scratch is still in it, the runtime-owned
// commit swept that scratch in, the post-commit cleanliness probe refused the
// result, and the retry path burned every remaining attempt on the identical
// deterministic condition. These tests pin the two halves of the repair
// together: the candidate work a crashed attempt produced must survive into a
// commit, and the scratch must not.
//
// They drive CandidateWorkspace.Commit directly. Nothing here constructs a
// provider: the ownership decision belongs to runtime workspace semantics, so
// an adapter-specific test would be testing the wrong layer.

// killedAttemptWorkspace is the shape a controller crash leaves behind: a
// workspace holding real candidate edits AND the worker's own validation
// scratch, which is a real Git repository with a worktree that keeps changing.
func killedAttemptWorkspace(t *testing.T) (*CandidateWorkspace, string) {
	t.Helper()
	w := commitGateWorkspace(t)
	scratch := workerTestScratchRepository(t, w.Dir, filepath.Join(".validation-tmp",
		"TestM_RestartPreservesTheExactEvidenceAndItsReason266398414", "001", "state",
		"runs", "run-63ca61c8e818d9c361958e04afdcee8a", "assurance", "0077b658-1"), true)
	return w, scratch
}

func commitTree(t *testing.T, w *CandidateWorkspace) string {
	t.Helper()
	tree, err := gitOutput(w.Dir, "ls-tree", "-r", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// A. An interrupted attempt's validation scratch does not cost the run its work.
//
// The intended source edit survives, the scratch stays out of the tree, the
// commit succeeds on the FIRST attempt - there is no retry to exhaust - and the
// workspace it leaves behind holds nothing dirty but the scratch itself.
func TestRecoveryCommitsTheCandidateEditAndNotTheInterruptedAttemptsScratch(t *testing.T) {
	w, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatalf("recovery lost the candidate work it inherited: %v", err)
	}
	if content, err := gitOutput(w.Dir, "show", "HEAD:README.md"); err != nil || strings.TrimSpace(content) != "intended edit" {
		t.Fatalf("the intended source edit is not in the commit: %q %v", content, err)
	}
	if tree := commitTree(t, w); strings.Contains(tree, ".validation-tmp") || strings.Contains(tree, "160000") {
		t.Fatalf("the interrupted attempt's scratch entered the candidate commit: %q", tree)
	}
	if !contains(result.Excluded, scratch) {
		t.Fatalf("the commit does not name the scratch it excluded: %q", result.Excluded)
	}
	// COHERENT, not merely committed: everything the runtime decided was
	// candidate state is in the tree, and the only thing still dirty is the
	// path it declared it does not own.
	residue, err := dirtyPathsOutside(w.Dir, result.Excluded)
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 0 {
		t.Fatalf("the workspace is not coherent after the commit: %q", residue)
	}
}

// B. Anti-vacuity: an untracked file a producer legitimately created is
// committed, in the same workspace whose scratch is excluded.
//
// This is the test that stops the repair being bought by dropping real work.
// A rule that excluded "things that look like scratch" would pass A and fail
// here, which is why both live in one fixture.
func TestRecoveryCommitsANewSourceFileBesideExcludedScratch(t *testing.T) {
	w, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "new_source.go"), []byte("package x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// An ordinary untracked directory whose name merely resembles scratch is
	// candidate work: the decision is structural, never nominal.
	lookalike := filepath.Join(".validation-tmp", "leftover")
	if err := os.MkdirAll(filepath.Join(w.Dir, lookalike), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, lookalike, "note.txt"), []byte("plain\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"new_source.go", filepath.ToSlash(filepath.Join(lookalike, "note.txt"))} {
		if !contains(result.Paths, want) {
			t.Fatalf("the commit result dropped %q: %q", want, result.Paths)
		}
		if _, err := gitOutput(w.Dir, "show", "HEAD:"+want); err != nil {
			t.Fatalf("%q is not in the commit: %v", want, err)
		}
	}
	if contains(result.Paths, scratch) {
		t.Fatalf("the scratch was reported as candidate work: %q", result.Paths)
	}
}

// C. A modification to a tracked file survives the same recovery.
func TestRecoveryCommitsAModifiedTrackedFile(t *testing.T) {
	w, _ := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(result.Paths, "README.md") {
		t.Fatalf("the tracked modification was not reported: %q", result.Paths)
	}
	if content, err := gitOutput(w.Dir, "show", "HEAD:README.md"); err != nil || strings.TrimSpace(content) != "modified" {
		t.Fatalf("the tracked modification is not in the commit: %q %v", content, err)
	}
}

// D. A legitimate deletion and a legitimate rename still enter the commit.
//
// Selective staging is where deletes and renames get silently dropped, so both
// are asserted against the TREE rather than against the reported path set.
func TestRecoveryCommitsADeletionAndARename(t *testing.T) {
	w, _ := killedAttemptWorkspace(t)
	for name, body := range map[string]string{"doomed.txt": "gone\n", "before.txt": "moved\n"} {
		if err := os.WriteFile(filepath.Join(w.Dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", "base for the rename"); err != nil {
		t.Fatal(err)
	}
	var err error
	if w.TrustedMetadata, err = gitMetadataDigest(w.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.Dir, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(w.Dir, "before.txt"), filepath.Join(w.Dir, "after.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(w.Dir, "show", "HEAD:doomed.txt"); err == nil {
		t.Fatal("the deletion was not committed")
	}
	if _, err := gitOutput(w.Dir, "show", "HEAD:before.txt"); err == nil {
		t.Fatal("the rename left the original path in the tree")
	}
	if content, err := gitOutput(w.Dir, "show", "HEAD:after.txt"); err != nil || strings.TrimSpace(content) != "moved" {
		t.Fatalf("the rename destination is not in the commit: %q %v", content, err)
	}
}

// E. Scratch that keeps changing around commit time cannot fail the commit.
//
// This is the exact production shape. One scratch file changed between the
// commit and the cleanliness probe, the probe read the whole workspace, and a
// commit that had captured every candidate path correctly was thrown away. The
// churn here runs across the commit and continues after it.
func TestScratchChangingAroundCommitTimeCannotFailTheCommit(t *testing.T) {
	w, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	churn := filepath.Join(w.Dir, filepath.FromSlash(scratch), "checked-out.txt")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(churn, []byte(strings.Repeat("x", i%64+1)+"\n"), 0600)
		}
	}()
	result, err := w.Commit("runtime candidate", 1<<20)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("scratch activity failed the candidate commit: %v", err)
	}
	// And after settlement: a late write must not make the workspace look
	// incoherent to the runtime either.
	if err := os.WriteFile(churn, []byte("written after the commit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	residue, err := dirtyPathsOutside(w.Dir, result.Excluded)
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 0 {
		t.Fatalf("a late scratch write made the workspace look dirty: %q", residue)
	}
	if tree := commitTree(t, w); strings.Contains(tree, ".validation-tmp") {
		t.Fatalf("the churning scratch entered the candidate tree: %q", tree)
	}
}

// F. Restart derives the same ownership decision.
//
// A recovering controller rebuilds the workspace struct from durable state and
// re-reads the integrity baseline from the repository; nothing about the
// exclusion is carried in memory from the attempt that died. The decision is a
// function of the workspace and its index, so it has to come out the same.
func TestRestartDerivesTheSameScratchOwnership(t *testing.T) {
	w, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := changedPaths(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := classifyRuntimeDebris(w.Dir, before)
	if err != nil {
		t.Fatal(err)
	}
	// THE RESTART: a new process, a new workspace value, the same directory.
	metadata, err := gitMetadataDigest(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &CandidateWorkspace{Dir: w.Dir, BaseRevision: w.BaseRevision, TrustedMetadata: metadata}
	after, err := changedPaths(restarted.Dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := classifyRuntimeDebris(restarted.Dir, after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first.Excluded, "\n") != strings.Join(second.Excluded, "\n") {
		t.Fatalf("restart derived a different exclusion: %q then %q", first.Excluded, second.Excluded)
	}
	result, err := restarted.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatalf("the restarted controller could not commit: %v", err)
	}
	if !contains(result.Excluded, scratch) {
		t.Fatalf("the restarted controller did not exclude the scratch: %q", result.Excluded)
	}
	if content, err := gitOutput(restarted.Dir, "show", "HEAD:README.md"); err != nil || strings.TrimSpace(content) != "intended edit" {
		t.Fatalf("the restarted controller lost the candidate edit: %q %v", content, err)
	}
}

// G. The runtime's own ephemeral execution state is not under the candidate.
//
// The exclusion above is the boundary for debris the runtime did not place.
// What the runtime DOES place is a separate obligation, and the preferred
// answer to #189 is that it never lands in candidate work at all: the build
// scratch, the assurance checkout and the #241 Git guard are all addressed from
// runtime state, for every attempt identity, and none of them resolves inside
// the workspace a producer writes.
//
// It is asserted for the composed paths rather than for a provider, because the
// property belongs to runtime workspace semantics and must hold whichever
// adapter executes.
func TestRuntimeOwnedEphemeralStateIsDisjointFromTheCandidateWorkspace(t *testing.T) {
	state := t.TempDir()
	attempt := ExecutionAttemptRef{RunID: "run-1", OperationID: "op-1", Attempt: 2}
	candidate := candidateDir(state, attempt.RunID)
	scratch, err := ExecutionScratchDir(state, attempt)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := GitGuardDir(state, attempt)
	if err != nil {
		t.Fatal(err)
	}
	assurance := filepath.Join(state, "runs", attempt.RunID, "assurance", "deadbeef-2")
	for name, dir := range map[string]string{"execution scratch": scratch, "git guard": guard, "assurance checkout": assurance} {
		rel, err := filepath.Rel(candidate, dir)
		if err != nil {
			t.Fatal(err)
		}
		if rel == "." || !strings.HasPrefix(rel, "..") {
			t.Fatalf("%s resolves inside the candidate workspace: %s", name, dir)
		}
	}
	// And the grant refuses to be pointed back in, so a future caller cannot
	// undo the separation by composing the path differently.
	if err := prepareValidationScratch(candidate, filepath.Join(candidate, ".validation-tmp")); err == nil {
		t.Fatal("validation scratch was granted inside the candidate workspace")
	}
}

// H. Mutation proof: the original failure, reproduced by removing the repair.
//
// The repair is the classification plus the exclusion. Removing it means doing
// what runtime/git.go:334 and :348 did - `git add -A --` over everything, then
// a cleanliness probe over the whole workspace - and both halves of the
// production failure then appear on the same fixture: the commit records a
// gitlink whose content no tree holds, and the probe refuses the commit that
// was just written. That refusal is deterministic, which is why it consumed
// every remaining attempt in 352 milliseconds.
func TestRemovingTheRepairReproducesTheOriginalCommitFailure(t *testing.T) {
	w, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// THE UNREPAIRED COMMIT PATH, verbatim.
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", "runtime candidate"); err != nil {
		t.Fatal(err)
	}
	staged, err := gitOutput(w.Dir, "ls-files", "--stage", "--", scratch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(staged, "160000 ") {
		t.Fatalf("the fixture did not reproduce the gitlink the runtime used to record: %q", staged)
	}
	if _, err := gitOutput(w.Dir, "show", "HEAD:"+scratch); err == nil {
		t.Fatal("the fixture did not reproduce a recorded path the tree does not hold")
	}
	status, err := gitOutput(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(status) == "" {
		t.Fatal("the fixture did not reproduce `candidate not clean after runtime commit`")
	}
	// The same condition, unchanged, on the attempt after this one: nothing
	// about retrying could have cleared it.
	repeat, err := gitOutput(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(repeat) != strings.TrimSpace(status) {
		t.Fatalf("the failure was not deterministic: %q then %q", status, repeat)
	}
	// And the repaired path, on the same workspace shape, does not.
	repaired, scratch := killedAttemptWorkspace(t)
	if err := os.WriteFile(filepath.Join(repaired.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := repaired.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatalf("the repaired path still refuses the recovered workspace: %v", err)
	}
	if !contains(result.Excluded, scratch) {
		t.Fatalf("the repaired path did not exclude the scratch: %q", result.Excluded)
	}
}

// A recovered workspace holding ONLY the killed attempt's scratch did not
// change the candidate.
//
// `candidate.changed` used to read the raw workspace status, so inherited
// scratch alone made it true. The commit it then planned had no candidate
// mutation to carry, which is the same deterministic dead end from the other
// direction. Both questions are now asked under one ownership rule.
func TestScratchAloneIsNotACandidateChange(t *testing.T) {
	w, _ := killedAttemptWorkspace(t)
	paths, err := candidateChangedPaths(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("inherited scratch was reported as a candidate change: %q", paths)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("the runtime committed a workspace holding nothing but scratch")
	}
	// The producer edits one file; now it IS a change, and the scratch still
	// is not.
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if paths, err = candidateChangedPaths(w.Dir); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("the candidate change set is not the producer's edit alone: %q", paths)
	}
}
