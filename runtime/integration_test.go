package runtime

// Controlled-repository tests for #475's deterministic composition
// (IntegrateInputs). Every fixture is a real, local Git repository; nothing
// here reaches a network or a provider, matching the fake/controlled setup
// the rest of the #470 Git tests already use (git_test.go,
// git_authority_test.go).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/integration"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// integrationFixture is one origin repository with a committed base, ready
// for CreateCandidateClone the same way every other candidate test builds
// one.
func integrationFixture(t *testing.T) (root, origin, base string) {
	t.Helper()
	requireGitFixture(t)
	root = t.TempDir()
	origin = filepath.Join(root, "origin")
	if _, err := runGit("", "init", origin); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.invalid"},
	} {
		if _, err := runGit(origin, args...); err != nil {
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
	head, err := gitOutput(origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return root, origin, strings.TrimSpace(head)
}

// cloneAt is CreateCandidateClone with the test's fatal-on-error boilerplate
// removed.
func cloneAt(t *testing.T, root, runID, origin, revision string) CandidateWorkspace {
	t.Helper()
	w, err := CreateCandidateClone(filepath.Join(root, "state"), runID, origin, revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// commitFile writes one file and commits it, returning the resulting
// CommitResult.
func commitFile(t *testing.T, w CandidateWorkspace, name, body, message string) CommitResult {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.Dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := w.Commit(message, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func workUnitInput(unitID string, c CommitResult) orchestration.WorkUnitInput {
	return orchestration.WorkUnitInput{
		UnitID: unitID,
		UnitOutput: orchestration.UnitOutput{
			HandoffID: "handoff-" + unitID, RunID: "run-" + unitID,
			CandidateRevision: c.Commit, CandidateTree: c.Tree,
			Outcome: "completed", Summary: "did the thing",
		},
	}
}

// TestIntegrateInputsCleanMerge proves requirement/scenario A: two
// independent, non-conflicting changes integrate into one exact candidate.
func TestIntegrateInputsCleanMerge(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		switch unitID {
		case "a":
			return a.Dir, true
		case "b":
			return b.Dir, true
		default:
			return "", false
		}
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("status = %s, want %s (reason=%q conflict=%+v)", result.Status, integration.StatusIntegrated, result.Reason, result.Conflict)
	}
	if result.Candidate == nil {
		t.Fatal("integrated result carries no candidate")
	}
	// Scenario J: the new subject is its own, never one of the inputs'.
	if result.Candidate.Revision == aCommit.Commit || result.Candidate.Revision == bCommit.Commit {
		t.Fatalf("integrated candidate %s reuses an upstream commit instead of naming a new subject", short12(result.Candidate.Revision))
	}
	for _, name := range []string{"README.md", "a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(integrationWS.Dir, name)); err != nil {
			t.Fatalf("integrated workspace is missing %s: %v", name, err)
		}
	}
	if err := integrationWS.AssertIntegrity(); err != nil {
		t.Fatalf("integration left the workspace's trusted metadata baseline stale: %v", err)
	}
}

// TestIntegrateInputsTextualConflict proves scenario B: a textual conflict
// produces a bounded, deterministic blocked result, and leaves the
// workspace clean at its base.
func TestIntegrateInputsTextualConflict(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "shared.txt", "a-version\n", "a changes shared.txt")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "shared.txt", "b-version\n", "b changes shared.txt")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return b.Dir, true
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusBlocked {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusBlocked)
	}
	if result.Conflict == nil || result.Conflict.Kind != integration.ConflictTextual {
		t.Fatalf("conflict = %+v, want a textual conflict", result.Conflict)
	}
	if len(result.Conflict.Paths) != 1 || result.Conflict.Paths[0] != "shared.txt" {
		t.Fatalf("conflict paths = %v, want [shared.txt]", result.Conflict.Paths)
	}
	// Left clean: no leftover merge state, and HEAD is still the base.
	if _, err := os.Stat(filepath.Join(integrationWS.Dir, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("a blocked attempt left a merge in progress: %v", err)
	}
	head, err := gitOutput(integrationWS.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(head) != base {
		t.Fatalf("a blocked attempt did not leave the workspace at its base: got %s, want %s", short12(strings.TrimSpace(head)), short12(base))
	}
}

// TestIntegrateInputsResumesAfterLeftoverMerge proves scenario E: a merge a
// prior attempt left mid-flight - standing in for a crash between detecting
// the conflict and aborting it - does not block or duplicate a later,
// successful attempt.
func TestIntegrateInputsResumesAfterLeftoverMerge(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)

	// Simulate the leftover: a real conflicting merge, left exactly where a
	// crash between detection and abort would leave it.
	conflicting := cloneAt(t, root, "run-conflict", origin, base)
	commitFile(t, conflicting, "a.txt", "conflicting\n", "conflicting change")
	if _, err := runGit(integrationWS.Dir, "fetch", "--no-tags", a.Dir, aCommit.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(integrationWS.Dir, "merge", "--no-ff", "--no-edit", aCommit.Commit); err != nil {
		t.Fatal(err)
	}
	conflictCommit, err := gitOutput(conflicting.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(integrationWS.Dir, "fetch", "--no-tags", conflicting.Dir, strings.TrimSpace(conflictCommit)); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(integrationWS.Dir, "merge", "--no-ff", "--no-edit", strings.TrimSpace(conflictCommit)); err == nil {
		t.Fatal("expected the staged collision to conflict")
	}
	if _, err := os.Stat(filepath.Join(integrationWS.Dir, ".git", "MERGE_HEAD")); err != nil {
		t.Fatalf("fixture did not leave a merge in progress: %v", err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return b.Dir, true
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("status = %s, want %s (reason=%q conflict=%+v)", result.Status, integration.StatusIntegrated, result.Reason, result.Conflict)
	}
	if _, err := os.Stat(filepath.Join(integrationWS.Dir, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("resumed attempt left a merge in progress: %v", err)
	}
}

// TestIntegrateInputsInvalidatesUnreadableInput proves scenario I: an input
// this attempt cannot read at its admitted subject fails closed as
// invalidated rather than guessed at or silently skipped.
func TestIntegrateInputsInvalidatesUnreadableInput(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	// b's source workspace never produced the commit the contract names -
	// standing in for an object that was never written, or has since been
	// garbage collected.
	b := cloneAt(t, root, "run-b", origin, base)

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	unreadable := workUnitInput("b", CommitResult{Commit: fmt.Sprintf("%040d", 1), Tree: fmt.Sprintf("%040d", 2)})
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), unreadable})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return b.Dir, true
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusInvalidated {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusInvalidated)
	}
	if !strings.Contains(result.Reason, "\"b\"") {
		t.Fatalf("invalidation reason does not name the unreadable input: %q", result.Reason)
	}
}

// TestIntegrateInputsInvalidatesStaleUpstream proves scenario F: an upstream
// input whose only remaining source workspace no longer carries the exact
// commit this attempt was activated against invalidates rather than
// silently substituting whatever that workspace carries now.
func TestIntegrateInputsInvalidatesStaleUpstream(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")

	// b's recorded input names an old commit, but the source this attempt is
	// given for "b" is the workspace AFTER it was superseded: the old
	// commit is unreachable from it, standing in for a replaced upstream
	// subject.
	bOld := cloneAt(t, root, "run-b-old", origin, base)
	staleCommit := commitFile(t, bOld, "b.txt", "old\n", "old beta")
	bNew := cloneAt(t, root, "run-b-new", origin, base)
	commitFile(t, bNew, "b.txt", "new\n", "replaced beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", staleCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return bNew.Dir, true
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusInvalidated {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusInvalidated)
	}
}

// TestIntegrateInputsRefusesNonCanonicalBase proves requirement 4: a
// workspace not actually at the contract's stated base is refused rather
// than merged from wherever it happens to be.
func TestIntegrateInputsRefusesNonCanonicalBase(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	// The integration workspace is cloned at A's commit, not the shared base
	// - from A's own workspace, since origin never received that commit.
	integrationWS := cloneAt(t, root, "run-integrate", a.Dir, aCommit.Commit)
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return b.Dir, true
	}

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("accepted an integration workspace that was not at the contract's verified base")
	}
}

// TestIntegrateInputsIsOrderIndependent proves requirement/scenario D at the
// Git level: composing [a,b] and [b,a] produces the identical tree.
func TestIntegrateInputsIsOrderIndependent(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")
	sources := func(unitID string) (string, bool) {
		if unitID == "a" {
			return a.Dir, true
		}
		return b.Dir, true
	}

	forward := cloneAt(t, root, "run-integrate-forward", origin, base)
	forwardContract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	forwardResult, err := IntegrateInputs(&forward, forwardContract, sources)
	if err != nil {
		t.Fatal(err)
	}

	backward := cloneAt(t, root, "run-integrate-backward", origin, base)
	backwardContract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("b", bCommit), workUnitInput("a", aCommit)})
	if err != nil {
		t.Fatal(err)
	}
	backwardResult, err := IntegrateInputs(&backward, backwardContract, sources)
	if err != nil {
		t.Fatal(err)
	}

	if forwardResult.Status != integration.StatusIntegrated || backwardResult.Status != integration.StatusIntegrated {
		t.Fatalf("expected both orderings to integrate cleanly: forward=%+v backward=%+v", forwardResult, backwardResult)
	}
	if forwardResult.Candidate.Tree != backwardResult.Candidate.Tree {
		t.Fatalf("arrival order changed the integrated tree: %s vs %s",
			short12(forwardResult.Candidate.Tree), short12(backwardResult.Candidate.Tree))
	}
}
