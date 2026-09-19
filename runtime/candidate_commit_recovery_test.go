package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	//
	// The candidate directory is created FIRST and an outside grant is proven
	// to be accepted, because otherwise this asserts nothing: an unresolvable
	// candidate path fails the same call for a reason that has nothing to do
	// with containment, and the test would pass with the containment check
	// deleted.
	if err := os.MkdirAll(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareValidationScratch(candidate, scratch); err != nil {
		t.Fatalf("a scratch grant outside the candidate workspace was refused: %v", err)
	}
	err = prepareValidationScratch(candidate, filepath.Join(candidate, ".validation-tmp"))
	if err == nil {
		t.Fatal("validation scratch was granted inside the candidate workspace")
	}
	if !strings.Contains(err.Error(), "validation scratch must be disjoint from candidate workspace") {
		t.Fatalf("the refusal is not the containment law: %v", err)
	}
	// And the other direction: a candidate workspace nested inside the grant is
	// the same violation, so neither side can be made to contain the other.
	// This one exists too, for the same reason the one above does.
	inner := filepath.Join(scratch, "inner")
	if err := os.MkdirAll(inner, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareValidationScratch(inner, scratch); err == nil {
		t.Fatal("a candidate workspace inside the validation scratch was accepted")
	} else if !strings.Contains(err.Error(), "validation scratch must be disjoint from candidate workspace") {
		t.Fatalf("the refusal is not the containment law: %v", err)
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

// writeCandidateGitignore installs a candidate-controlled ignore file and makes
// it part of the workspace history, so what follows is about the ignore rule
// rather than about an untracked `.gitignore`.
func writeCandidateGitignore(t *testing.T, w *CandidateWorkspace, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.Dir, ".gitignore"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", "candidate ignore rules"); err != nil {
		t.Fatal(err)
	}
	var err error
	if w.TrustedMetadata, err = gitMetadataDigest(w.Dir); err != nil {
		t.Fatal(err)
	}
}

// 1. An IGNORED nested repository still reaches structural classification.
//
// The ignored-path refusal exists so a candidate-controlled `.gitignore` cannot
// decide what a runtime commit leaves out, and it errored before
// classifyRuntimeDebris ever ran. A killed attempt's scratch is routinely both
// ignored and a real repository, so recovery died on an ignore rule over a path
// the classifier exists to exclude.
func TestAnIgnoredNestedRepositoryIsClassifiedRatherThanRefused(t *testing.T) {
	w := commitGateWorkspace(t)
	writeCandidateGitignore(t, w, "scratch/")
	scratch := workerTestScratchRepository(t, w.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatalf("an ignored nested repository blocked recovery: %v", err)
	}
	if !contains(result.Excluded, scratch) {
		t.Fatalf("the ignored nested repository was not excluded structurally: %q", result.Excluded)
	}
	if content, err := gitOutput(w.Dir, "show", "HEAD:README.md"); err != nil || strings.TrimSpace(content) != "intended edit" {
		t.Fatalf("the candidate edit is not in the commit: %q %v", content, err)
	}
	if tree := commitTree(t, w); strings.Contains(tree, "scratch") || strings.Contains(tree, "160000") {
		t.Fatalf("the ignored scratch entered the candidate tree: %q", tree)
	}
}

// 1 (negative). An ignored ORDINARY file is still refused.
//
// This is what stops the exception becoming ".gitignore decides". The rule is
// structural: being a Git repository is what earns exclusion, and being listed
// in an ignore file earns nothing.
func TestAnIgnoredOrdinaryCandidateFileIsStillRefused(t *testing.T) {
	w := commitGateWorkspace(t)
	writeCandidateGitignore(t, w, "ordinary-file.txt")
	if err := os.WriteFile(filepath.Join(w.Dir, "ordinary-file.txt"), []byte("hidden\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("a candidate-controlled ignore rule removed an ordinary file from the runtime's view")
	} else if !strings.Contains(err.Error(), "ignored candidate file") {
		t.Fatalf("the refusal is not the ignored-file rule: %v", err)
	}
}

// 2. A path the commit will not carry cannot veto the commit by its NAME.
//
// The sensitive-basename refusal is a statement about the object being
// published. An inherited scratch repository called `.env` publishes no bytes,
// so it is not a credential in the candidate - and letting it refuse the commit
// is the #189 dead end wearing a different error message.
func TestAnExcludedNestedRepositoryCannotVetoTheCommitByName(t *testing.T) {
	w := commitGateWorkspace(t)
	scratch := workerTestScratchRepository(t, w.Dir, ".env", true)
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit("runtime candidate", 1<<20)
	if err != nil {
		t.Fatalf("an excluded nested repository vetoed the commit by its name: %v", err)
	}
	if !contains(result.Excluded, scratch) {
		t.Fatalf("the nested repository was not excluded: %q", result.Excluded)
	}
	if content, err := gitOutput(w.Dir, "show", "HEAD:README.md"); err != nil || strings.TrimSpace(content) != "intended edit" {
		t.Fatalf("the candidate edit is not in the commit: %q %v", content, err)
	}
}

// 2 (negative). An ELIGIBLE candidate file with the same name is still refused.
//
// Nothing about the credential boundary on candidate work is weakened; only the
// subject of the question changed.
func TestAnEligibleCredentialShapedPathIsStillRefused(t *testing.T) {
	w := commitGateWorkspace(t)
	workerTestScratchRepository(t, w.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(w.Dir, "id_rsa"), []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("a credential-shaped candidate file was committed")
	} else if !strings.Contains(err.Error(), "sensitive candidate path") {
		t.Fatalf("the refusal is not the sensitive-path gate: %v", err)
	}
}

// 2 (size). The candidate size ceiling is charged for what the commit carries.
//
// The scale is worth being honest about: a nested repository contributes only
// its own directory entry, because Git never descends into one, so the 64 KiB
// underneath it here were never charged even before the split. What changed is
// the SUBJECT of the ceiling, and the ceiling below is derived from the fixture
// so that the change is observable rather than asserted.
func TestTheCandidateSizeCeilingCountsOnlyWhatTheCommitCarries(t *testing.T) {
	w := commitGateWorkspace(t)
	scratch := workerTestScratchRepository(t, w.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(w.Dir, filepath.FromSlash(scratch), "bulk.bin"), make([]byte, 1<<16), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("small\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A ceiling of one byte: the excluded directory entry alone exceeds it on
	// any filesystem, so the two subjects give opposite answers.
	if err := GuardCandidateCommitContent(w.Dir, []string{scratch + "/"}, 1); err == nil {
		t.Fatal("the fixture does not distinguish the two subjects")
	}
	if err := GuardCandidateCommitContent(w.Dir, []string{"README.md"}, 1<<20); err != nil {
		t.Fatalf("an eligible path was refused by the ceiling it fits under: %v", err)
	}
	// A ceiling derived from the fixture rather than guessed: it admits the
	// eligible file and would not admit it plus the excluded directory entry,
	// on any filesystem, so this half fails if the subject regresses.
	readme, err := os.Lstat(filepath.Join(w.Dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Lstat(filepath.Join(w.Dir, filepath.FromSlash(scratch)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", readme.Size()+dir.Size()-1); err != nil {
		t.Fatalf("excluded scratch was charged to the candidate size ceiling: %v", err)
	}
	// The same ceiling, against a file the commit does carry.
	oversized := commitGateWorkspace(t)
	workerTestScratchRepository(t, oversized.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(oversized.Dir, "bulk.bin"), make([]byte, 1<<16), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := oversized.Commit("runtime candidate", 4096); err == nil {
		t.Fatal("an oversized eligible candidate file was committed")
	} else if !strings.Contains(err.Error(), "candidate exceeds size ceiling") {
		t.Fatalf("the refusal is not the size ceiling: %v", err)
	}
}

// 2 (content). The credential-value scan asks about what the commit carries.
//
// Today that is a consistency change rather than a behaviour change, and the
// test says so rather than implying more: the scan reads regular files, a
// nested repository is a directory Git does not descend into, so a value
// underneath one never reached the scan and never reaches the tree either. What
// is asserted is the part that matters - the boundary on candidate work is
// unchanged, and excluded scratch neither refuses the commit nor enters it.
func TestTheCredentialValueScanAsksAboutWhatTheCommitCarries(t *testing.T) {
	const secret = "github_pat_11ABCDEFG0aaaaaaaaaaaa_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	w := commitGateWorkspace(t)
	scratch := workerTestScratchRepository(t, w.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(w.Dir, filepath.FromSlash(scratch), "leaked.txt"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("runtime candidate", 1<<20); err != nil {
		t.Fatalf("a value inside excluded scratch refused the candidate commit: %v", err)
	}
	if _, err := gitOutput(w.Dir, "show", "HEAD:"+scratch+"/leaked.txt"); err == nil {
		t.Fatal("the excluded scratch reached the tree, so unscanned bytes were published")
	}
	// The scan itself is unchanged, and still refuses the value wherever it is
	// asked about a regular file.
	if err := scanPathsForCredentialValues(w.Dir, []string{scratch + "/leaked.txt"}); err == nil {
		t.Fatal("the credential scan stopped recognizing a value")
	}
	// And the same bytes in candidate work are refused by the commit.
	leaking := commitGateWorkspace(t)
	workerTestScratchRepository(t, leaking.Dir, "scratch", true)
	if err := os.WriteFile(filepath.Join(leaking.Dir, "config.go"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := leaking.Commit("runtime candidate", 1<<20); err == nil {
		t.Fatal("a credential value in candidate work was committed")
	}
}

// 4 (commit side). More debris than one journal payload may carry is refused
// BEFORE a commit is written, because the alternative is a commit whose own
// event cannot be recorded.
func TestMoreExcludedPathsThanThePayloadBoundRefuseBeforeTheCommit(t *testing.T) {
	w := commitGateWorkspace(t)
	for i := 0; i <= maxPayloadListItems; i++ {
		workerTestScratchRepository(t, w.Dir, filepath.Join("scratch", "repo-"+strconv.Itoa(i)), true)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("intended edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Commit("runtime candidate", 1<<20)
	if err == nil {
		t.Fatal("more excluded paths than the payload bound were recorded anyway")
	}
	if !strings.Contains(err.Error(), "excluded_paths") || !strings.Contains(err.Error(), "scratch/repo-0") {
		t.Fatalf("the refusal does not name the bound and the paths: %v", err)
	}
	after, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(after) != strings.TrimSpace(before) {
		t.Fatal("the runtime wrote a commit whose own event it could not journal")
	}
}

// 4 (schema side). `excluded_paths` is durable journal content, so the payload
// contract bounds it like every other list: a valid set is accepted, and an
// over-long or over-full one is REFUSED rather than truncated or digested.
//
// Both event types carry the same payload, and a checkpoint is a real
// runtime-owned commit, so both are asserted.
func TestExcludedPathsAreBoundedInTheDurablePayload(t *testing.T) {
	base := CandidateCommittedPayload{
		Commit: "c", Tree: "t", PathCount: 1, PathsDigest: "d",
	}
	many := make([]string, maxPayloadListItems+1)
	for i := range many {
		many[i] = "scratch/repo-" + strconv.Itoa(i)
	}
	for _, event := range []string{EventCandidateCommitted, EventCandidateCheckpointed} {
		validate, ok := eventPayloads[event]
		if !ok {
			t.Fatalf("%s has no payload schema", event)
		}
		for _, c := range []struct {
			name    string
			paths   []string
			refused bool
		}{
			{"none", nil, false},
			{"a valid set", []string{".validation-tmp/assurance/0077b658-1", "t/fixture-origin"}, false},
			{"too many", many, true},
			{"an overlong path", []string{strings.Repeat("a", maxPayloadListItemBytes+1)}, true},
		} {
			t.Run(event+"/"+c.name, func(t *testing.T) {
				payload := base
				payload.ExcludedPaths = c.paths
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				err = validate(encoded)
				if c.refused && err == nil {
					t.Fatal("an unbounded excluded_paths list was accepted into durable state")
				}
				if !c.refused && err != nil {
					t.Fatalf("a bounded excluded_paths list was refused: %v", err)
				}
				if c.refused && !strings.Contains(err.Error(), "excluded_paths") {
					t.Fatalf("the refusal does not name the field: %v", err)
				}
			})
		}
	}
}
