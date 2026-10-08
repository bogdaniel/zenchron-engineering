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

// handoffIDFor is the fixture's one consistent mapping from a unit id to the
// handoff identity its admitted output carries, shared between
// workUnitInput (what a contract records) and sourcesFor (what a live
// lookup currently reports) so the two agree unless a test deliberately
// diverges them.
func handoffIDFor(unitID string) string { return "handoff-" + unitID }

func workUnitInput(unitID string, c CommitResult) orchestration.WorkUnitInput {
	return orchestration.WorkUnitInput{
		UnitID: unitID,
		UnitOutput: orchestration.UnitOutput{
			HandoffID: handoffIDFor(unitID), RunID: "run-" + unitID,
			CandidateRevision: c.Commit, CandidateTree: c.Tree,
			Outcome: "completed", Summary: "did the thing",
		},
	}
}

// sourcesFor builds an IntegrationSources from a unit id -> producer
// workspace directory map, reporting each unit's CURRENT handoff id as
// handoffIDFor(unitID) - agreeing with workUnitInput, so ordinary tests
// exercise a live lookup that confirms currency rather than one that always
// trivially passes it.
func sourcesFor(dirs map[string]string) IntegrationSources {
	return func(unitID string) (IntegrationSource, bool) {
		dir, ok := dirs[unitID]
		if !ok {
			return IntegrationSource{}, false
		}
		return IntegrationSource{Dir: dir, HandoffID: handoffIDFor(unitID)}, true
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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

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
// workspace clean at its base - including discarding the FIRST input's
// clean merge once the second one conflicts (finding: partial integration
// must never be retained).
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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

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
	// Left clean: no leftover merge state, and HEAD is back at the base -
	// "a"'s own clean merge (the first plan step) was discarded too, not
	// only "b"'s conflicting attempt.
	if _, err := os.Stat(filepath.Join(integrationWS.Dir, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("a blocked attempt left a merge in progress: %v", err)
	}
	assertWorkspaceAtRevision(t, integrationWS.Dir, base)
}

// TestIntegrateInputsSucceedsAfterExplicitRecovery proves scenario E
// end-to-end, consistent with finding 1's fix: a workspace a crash left mid
// plan (an earlier step's merge already committed) is refused by
// IntegrateInputs itself - see TestIntegrateInputsRefusesDivergedWorkspace -
// never silently composed over. Only once the runtime's EXISTING
// FailureWorkspaceIntegrity recovery (RestoreTrusted) has explicitly
// restored it does a fresh attempt proceed, and it composes cleanly rather
// than producing a second, competing integration of the same input set.
func TestIntegrateInputsSucceedsAfterExplicitRecovery(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	// Simulate the crash: an earlier attempt's first plan step already
	// committed, then nothing recorded it.
	if _, err := runGit(integrationWS.Dir, "fetch", "--no-tags", a.Dir, aCommit.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(integrationWS.Dir, "merge", "--no-ff", "--no-edit", aCommit.Commit); err != nil {
		t.Fatal(err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("expected the crash-diverged workspace to be refused before explicit recovery")
	}

	// The explicit recovery route: never called by IntegrateInputs itself.
	if err := integrationWS.RestoreTrusted(); err != nil {
		t.Fatal(err)
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("status = %s, want %s (reason=%q conflict=%+v)", result.Status, integration.StatusIntegrated, result.Reason, result.Conflict)
	}
}

// TestIntegrateInputsRefusesDivergedWorkspace proves finding 1: a workspace
// whose Git metadata no longer matches its trusted baseline for a reason
// other than a leftover (non-head-moving) conflict - standing in for a crash
// after an earlier plan step already committed, or genuine tampering - is
// refused rather than silently restored and composed over. This function
// must never call RestoreTrusted on an unexplained divergence; only the
// runtime's existing FailureWorkspaceIntegrity route does, with real
// operation provenance.
func TestIntegrateInputsRefusesDivergedWorkspace(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	// Advance HEAD past base WITHOUT going through IntegrateInputs - standing
	// in for an earlier crashed attempt's own successful intermediate merge
	// commit, which IS a Git-metadata divergence (unlike a mere leftover
	// conflict).
	if _, err := runGit(integrationWS.Dir, "fetch", "--no-tags", a.Dir, aCommit.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(integrationWS.Dir, "merge", "--no-ff", "--no-edit", aCommit.Commit); err != nil {
		t.Fatal(err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("silently composed over a workspace whose Git metadata had diverged from its trusted baseline")
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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": bNew.Dir})

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusInvalidated {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusInvalidated)
	}
	// "a" (step 1) merged cleanly before "b" (step 2) invalidated; that
	// progress must not survive this outcome.
	assertWorkspaceAtRevision(t, integrationWS.Dir, base)
}

// TestIntegrateInputsInvalidatesSupersededHandoff proves finding 4: an input
// whose named commit remains perfectly fetchable is still invalidated once
// the live source reports a DIFFERENT current handoff id for that unit -
// standing in for #472 having admitted a newer handoff after this contract
// was built. Fetchability alone never establishes currency.
func TestIntegrateInputsInvalidatesSupersededHandoff(t *testing.T) {
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
	dirs := map[string]string{"a": a.Dir, "b": b.Dir}
	sources := func(unitID string) (IntegrationSource, bool) {
		dir, ok := dirs[unitID]
		if !ok {
			return IntegrationSource{}, false
		}
		handoff := handoffIDFor(unitID)
		if unitID == "b" {
			// b's commit is perfectly fetchable from this very directory -
			// only the live handoff identity says it is no longer current.
			handoff = "handoff-b-superseding"
		}
		return IntegrationSource{Dir: dir, HandoffID: handoff}, true
	}

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusInvalidated {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusInvalidated)
	}
	if !strings.Contains(result.Reason, "\"b\"") {
		t.Fatalf("invalidation reason does not name the superseded input: %q", result.Reason)
	}
	// "a" (step 1) merged cleanly before "b" (step 2) invalidated; that
	// progress must not survive this outcome.
	assertWorkspaceAtRevision(t, integrationWS.Dir, base)
}

// TestIntegrateInputsPropagatesNonConflictMergeFailure proves finding 3: a
// Git merge failure that leaves no unmerged path is not a conflict and must
// not be reported as StatusBlocked; it is a real failure this build cannot
// explain away.
func TestIntegrateInputsPropagatesNonConflictMergeFailure(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	// An uncommitted, unstaged local change to a path the first merge step
	// also touches: Git refuses the merge outright ("local changes would be
	// overwritten"), before it ever gets far enough to leave an unmerged
	// path - the hallmark that distinguishes this from a real conflict.
	if err := os.WriteFile(filepath.Join(integrationWS.Dir, "a.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("a non-conflict merge failure was silently accepted or classified as a conflict")
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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

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
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

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
