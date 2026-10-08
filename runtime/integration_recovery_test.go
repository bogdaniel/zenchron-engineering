package runtime

// Regression tests for the PR #541 re-review's three remaining correctness
// findings: partial integration surviving a non-conflict failure, Git
// ancestry classification, and conflict-path parsing. integration_test.go
// owns the core composition scenarios (A/B/D/F/I/J); this file owns proving
// each of those three fixes specifically.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/integration"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestIntegrateInputsDiscardsPartialProgressOnInvalidatedInput proves that a
// non-conflict failure on a later plan step - here, an unreadable input -
// discards an EARLIER step's own successful merge, not only a merge the
// failing step itself attempted. Before this fix, IntegrateInputs reset the
// workspace only on a reported textual conflict, so this exact shape (A
// merges, B is unreadable) left A's commit standing as the workspace's HEAD.
func TestIntegrateInputsDiscardsPartialProgressOnInvalidatedInput(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	// b's source workspace never produced the commit the contract names.
	b := cloneAt(t, root, "run-b", origin, base)

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	unreadable := workUnitInput("b", CommitResult{Commit: strings.Repeat("0", 40), Tree: strings.Repeat("0", 40)})
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
	assertWorkspaceAtRevision(t, integrationWS.Dir, base)
}

// TestIntegrateInputsInvalidatesNonDescendantInput proves the Git-ancestry
// half of the fix: an input from a history entirely disjoint from the
// contract's base - a genuine, Git-confirmed "no", not an error - is
// invalidated through the SAME exit-status-based classification
// LocalGitAncestry already proves elsewhere in this package, reused here
// rather than a second, string-sniffing implementation.
func TestIntegrateInputsInvalidatesNonDescendantInput(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")

	// b's commit shares NO history with origin at all.
	unrelatedRoot := filepath.Join(root, "unrelated-origin")
	if _, err := runGit("", "init", unrelatedRoot); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.invalid"},
	} {
		if _, err := runGit(unrelatedRoot, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(unrelatedRoot, "b.txt"), []byte("beta\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(unrelatedRoot, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(unrelatedRoot, "commit", "-m", "unrelated beta"); err != nil {
		t.Fatal(err)
	}
	unrelatedHead, err := gitOutput(unrelatedRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedHead = strings.TrimSpace(unrelatedHead)
	unrelatedTree, err := gitOutput(unrelatedRoot, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedTree = strings.TrimSpace(unrelatedTree)

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	disjoint := workUnitInput("b", CommitResult{Commit: unrelatedHead, Tree: unrelatedTree})
	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), disjoint})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": unrelatedRoot})

	result, err := IntegrateInputs(&integrationWS, contract, sources)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != integration.StatusInvalidated {
		t.Fatalf("status = %s, want %s", result.Status, integration.StatusInvalidated)
	}
	if !strings.Contains(result.Reason, "descendant") {
		t.Fatalf("invalidation reason does not name the ancestry failure: %q", result.Reason)
	}
	assertWorkspaceAtRevision(t, integrationWS.Dir, base)
}

// TestIntegrateInputsTextualConflictPreservesUnusualFilenames proves the
// conflict-path parsing fix: a conflicted path with a non-ASCII byte is
// Git's line-oriented `--name-only` output C-quotes it
// (`"caf\303\251.txt"`, the escaped bytes, not the real two-byte "é"), and a
// remediation touching the REAL path would then be refused as out of scope.
// Reading `-z` output instead must report the exact real path.
func TestIntegrateInputsTextualConflictPreservesUnusualFilenames(t *testing.T) {
	root, origin, base := integrationFixture(t)
	unusual := "café.txt"
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, unusual, "a-version\n", "a changes "+unusual)
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, unusual, "b-version\n", "b changes "+unusual)

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
	if len(result.Conflict.Paths) != 1 || result.Conflict.Paths[0] != unusual {
		t.Fatalf("conflict paths = %q, want the exact unquoted path %q", result.Conflict.Paths, unusual)
	}
	// Round-trip: a remediation that changes exactly the real path is in
	// scope, which would be impossible if Paths held the quoted spelling.
	if err := integration.VerifyRemediationScope(*result.Conflict, []string{unusual}); err != nil {
		t.Fatalf("a remediation to the real conflicted path was refused: %v", err)
	}
}

// TestIntegrateInputsRefusesLegitimatelyAdvancedWorkspace proves finding 2
// from the second re-review: ws.BaseRevision and AssertIntegrity alone
// cannot prove a workspace is still at the contract's base. An AUTHORIZED
// commit - not a crash, not tampering - correctly refreshes TrustedMetadata
// along with HEAD, so AssertIntegrity alone would pass; IntegrateInputs
// must still refuse once it checks live HEAD against the contract's base
// directly.
func TestIntegrateInputsRefusesLegitimatelyAdvancedWorkspace(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	// Commit directly on &integrationWS, not through the commitFile helper:
	// that helper takes its workspace BY VALUE, so a commit through it never
	// updates the caller's own TrustedMetadata - fine for a producer
	// workspace this suite only ever reads Dir from afterward, but this test
	// specifically needs integrationWS's own TrustedMetadata refreshed, to
	// prove AssertIntegrity alone is insufficient.
	if err := os.WriteFile(filepath.Join(integrationWS.Dir, "unrelated.txt"), []byte("legitimate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := integrationWS.Commit("authorized unrelated commit", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := integrationWS.AssertIntegrity(); err != nil {
		t.Fatalf("fixture's own authorized commit should pass AssertIntegrity: %v", err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("composed on top of a workspace a legitimate commit had already advanced past the contract's base")
	}
}

// TestIntegrateInputsNeverDeletesLegitimateMaterialBeforeAnyMutation proves
// finding 1 from the second re-review: a workspace that already holds
// legitimate untracked material unrelated to this attempt is refused
// outright, before anything this call does could ever put that material at
// risk - never silently discarded by the destructive RestoreTrusted path a
// later failure might otherwise trigger.
func TestIntegrateInputsNeverDeletesLegitimateMaterialBeforeAnyMutation(t *testing.T) {
	root, origin, base := integrationFixture(t)
	a := cloneAt(t, root, "run-a", origin, base)
	aCommit := commitFile(t, a, "a.txt", "alpha\n", "add alpha")
	b := cloneAt(t, root, "run-b", origin, base)
	bCommit := commitFile(t, b, "b.txt", "beta\n", "add beta")

	integrationWS := cloneAt(t, root, "run-integrate", origin, base)
	legitimate := filepath.Join(integrationWS.Dir, "legitimate-scratch.txt")
	if err := os.WriteFile(legitimate, []byte("do not delete\n"), 0600); err != nil {
		t.Fatal(err)
	}

	contract, err := integration.NewContract("graph-1", "integrate", base,
		orchestration.WorkUnitInputs{workUnitInput("a", aCommit), workUnitInput("b", bCommit)})
	if err != nil {
		t.Fatal(err)
	}
	sources := sourcesFor(map[string]string{"a": a.Dir, "b": b.Dir})

	if _, err := IntegrateInputs(&integrationWS, contract, sources); err == nil {
		t.Fatal("composed against a dirty workspace instead of refusing it outright")
	}
	if _, err := os.Stat(legitimate); err != nil {
		t.Fatalf("a refused attempt deleted pre-existing material: %v", err)
	}
}

// assertWorkspaceAtRevision fails the test unless dir's HEAD is exactly
// revision.
func assertWorkspaceAtRevision(t *testing.T, dir, revision string) {
	t.Helper()
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head = strings.TrimSpace(head); head != revision {
		t.Fatalf("workspace is at %s, want it discarded back to %s", short12(head), short12(revision))
	}
}
