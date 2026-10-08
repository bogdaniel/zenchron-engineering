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
