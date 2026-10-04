package runtime

// #437 slice 2: after a runtime commit the provider-writable candidate is no
// longer a content authority. Every reader of the commit - the gates, contract
// reassessment, assurance, #431 recovery and publication - reads the verified
// subject store, which a fetch fills and index-pack re-hashes. The tampering
// below is what a process that outlived its invocation can do inside the
// candidate: rewrite a loose object behind its name, answer a name from an
// alternate, or set index flags and rewrite the worktree.

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// swapLooseObject rewrites the loose object id in dir's object directory with
// content of kind, keeping the name. Git does not re-hash a loose object it
// reads, so every reader of that name now sees content.
func swapLooseObject(t *testing.T, dir, id, kind string, content []byte) {
	t.Helper()
	path := looseObjectPath(dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Replaced, not rewritten in place: a local clone hardlinks objects, and
	// the swap must not reach the repository the candidate was cloned from.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, zlibObject(t, kind, content), 0o400); err != nil {
		t.Fatal(err)
	}
}

func looseObjectPath(dir, id string) string {
	return filepath.Join(dir, ".git", "objects", id[:2], id[2:])
}

func zlibObject(t *testing.T, kind string, content []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	fmt.Fprintf(zw, "%s %d\x00", kind, len(content))
	_, _ = zw.Write(content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func withAfterCommitUpdateRef(t *testing.T, hook func(dir string)) {
	t.Helper()
	afterCommitUpdateRef = hook
	t.Cleanup(func() { afterCommitUpdateRef = nil })
}

const (
	realSource    = "package a // real\n"
	standInSource = "package a // STANDIN\n"
)

// committedSubject is a runtime commit of a.go and b.go, with its subject
// store filled by Commit itself.
func committedSubject(t *testing.T) (*CandidateWorkspace, CommitResult, string) {
	t.Helper()
	w := commitGateWorkspace(t)
	parent := strings.TrimSpace(headOf(t, w))
	mustWrite(t, filepath.Join(w.Dir, "a.go"), realSource)
	mustWrite(t, filepath.Join(w.Dir, "b.go"), "package b\n")
	result, err := w.Commit("subject", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return w, result, parent
}

// TestAssuranceChecksOutTheSubjectStore (R1, R2): a committed blob swapped
// behind its name, the same name answered from an alternate, and post-commit
// index flags with a rewritten worktree are all never read by the verifier's
// checkout. Without a store to read from, the swapped candidate is refused.
func TestAssuranceChecksOutTheSubjectStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, w *CandidateWorkspace, blob string)
	}{
		{"swapped blob", func(t *testing.T, w *CandidateWorkspace, blob string) {
			swapLooseObject(t, w.Dir, blob, "blob", []byte(standInSource))
		}},
		{"alternate answers the name", func(t *testing.T, w *CandidateWorkspace, blob string) {
			alternate := filepath.Join(t.TempDir(), "objects")
			path := filepath.Join(alternate, blob[:2], blob[2:])
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, zlibObject(t, "blob", []byte(standInSource)), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(looseObjectPath(w.Dir, blob)); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(w.Dir, ".git", "objects", "info", "alternates"), alternate+"\n")
		}},
		{"index flags and a rewritten worktree", func(t *testing.T, w *CandidateWorkspace, _ string) {
			fixtureGit(t, w.Dir, "update-index", "--skip-worktree", "a.go")
			fixtureGit(t, w.Dir, "update-index", "--assume-unchanged", "b.go")
			mustWrite(t, filepath.Join(w.Dir, "a.go"), standInSource)
			mustWrite(t, filepath.Join(w.Dir, "b.go"), standInSource)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, result, _ := committedSubject(t)
			blob := mustGit(t, w.Dir, "rev-parse", result.Commit+":a.go")
			tc.tamper(t, w, blob)
			if got := mustGit(t, w.Dir, "cat-file", "blob", blob); got+"\n" != standInSource && tc.name != "index flags and a rewritten worktree" {
				t.Fatalf("the tamper did not take: the candidate reads %q", got)
			}
			checkout := filepath.Join(t.TempDir(), "checkout")
			if err := assuranceCheckout(w.Dir, checkout, result.Commit, result.Tree); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"a.go": realSource, "b.go": "package b\n"} {
				if got, err := os.ReadFile(filepath.Join(checkout, name)); err != nil || string(got) != want {
					t.Fatalf("the verifier checkout holds %q for %s, not the committed bytes: %v", got, name, err)
				}
			}
		})
	}
	t.Run("no store: the swapped candidate is refused", func(t *testing.T) {
		w, result, _ := committedSubject(t)
		if err := os.RemoveAll(subjectStoreDir(w.Dir)); err != nil {
			t.Fatal(err)
		}
		swapLooseObject(t, w.Dir, mustGit(t, w.Dir, "rev-parse", result.Commit+":a.go"), "blob", []byte(standInSource))
		err := assuranceCheckout(w.Dir, filepath.Join(t.TempDir(), "checkout"), result.Commit, result.Tree)
		if err == nil || !strings.Contains(err.Error(), errSubjectUnverified) {
			t.Fatalf("a candidate whose object no longer matches its name was checked out: %v", err)
		}
	})
}

// hideB swaps the commit's root tree in the candidate for one without b.go.
func hideB(t *testing.T, dir, commit string) {
	t.Helper()
	root := mustGit(t, dir, "rev-parse", commit+"^{tree}")
	var listing []string
	for _, line := range strings.Split(mustGit(t, dir, "ls-tree", commit), "\n") {
		if !strings.HasSuffix(line, "\tb.go") {
			listing = append(listing, line)
		}
	}
	standIn, err := runGitInput(dir, []byte(strings.Join(listing, "\n")+"\n"), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runGit(dir, "cat-file", "tree", strings.TrimSpace(string(standIn)))
	if err != nil {
		t.Fatal(err)
	}
	swapLooseObject(t, dir, root, "tree", raw)
	if out := mustGit(t, dir, "diff", "--name-only", commit+"^", commit); out != "a.go" {
		t.Fatalf("the tree swap did not take: the candidate's own diff reads %q", out)
	}
}

// TestKernelPathsReadTheSubjectStore (R1): contract reassessment observes the
// paths the commit holds, not a tree swapped behind its name to hide one.
func TestKernelPathsReadTheSubjectStore(t *testing.T) {
	t.Run("store present: the swap is never read", func(t *testing.T) {
		w, result, parent := committedSubject(t)
		hideB(t, w.Dir, result.Commit)
		paths, err := candidatePaths(w.Dir, parent, result.Commit)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(paths, []string{"a.go", "b.go"}) {
			t.Fatalf("reassessment observed %q, not the committed paths", paths)
		}
	})
	t.Run("no store: the swapped candidate is refused", func(t *testing.T) {
		w, result, parent := committedSubject(t)
		if err := os.RemoveAll(subjectStoreDir(w.Dir)); err != nil {
			t.Fatal(err)
		}
		hideB(t, w.Dir, result.Commit)
		if paths, err := candidatePaths(w.Dir, parent, result.Commit); err == nil || !strings.Contains(err.Error(), errSubjectUnverified) {
			t.Fatalf("a swapped tree was observed: %q %v", paths, err)
		}
	})
}

// TestRecoveryReadsTheSubjectStore (R1): #431 recovery recomputes the recorded
// commit's paths from the subject store, so a subtree swapped in the candidate
// to hide a path is never read and does not change what is adopted.
func TestRecoveryReadsTheSubjectStore(t *testing.T) {
	c := newRecoveryCase(t, func(dir string) error {
		for _, name := range []string{"x.go", "y.go"} {
			if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "pkg", name), []byte("package pkg\n"), 0o600); err != nil {
				return err
			}
		}
		return nil
	})
	subtree := mustGit(t, c.dir, "rev-parse", c.valid.Commit+":pkg")
	standIn, err := runGitInput(c.dir, []byte(mustGit(t, c.dir, "ls-tree", subtree, "x.go")+"\n"), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runGit(c.dir, "cat-file", "tree", strings.TrimSpace(string(standIn)))
	if err != nil {
		t.Fatal(err)
	}
	swapLooseObject(t, c.dir, subtree, "tree", raw)
	if out := mustGit(t, c.dir, "diff", "--name-only", c.fixture.base, c.valid.Commit); strings.Contains(out, "y.go") {
		t.Fatalf("the subtree swap did not take: %q", out)
	}
	result, err := c.recover(c.valid)
	if err != nil {
		t.Fatalf("the recorded commit was not recovered from the subject store: %v", err)
	}
	if pathsDigest(result.Paths) != c.valid.PathsDigest || !slices.Contains(result.Paths, "pkg/y.go") {
		t.Fatalf("recovery recomputed %q from the candidate, not the recorded paths", result.Paths)
	}
}

// TestPublicationPushesFromTheSubjectStore (R1): the push carries the commit's
// own content even when the candidate's object directory holds a stand-in
// behind the committed blob's name. A push from the candidate would send the
// stand-in, which the receiving side refuses.
func TestPublicationPushesFromTheSubjectStore(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.crash(runID, func(call GitHubCall) bool {
		return call.Method == "RefSHA" && strings.HasPrefix(call.Ref, "zenchron/")
	})
	state := fixture.state(runID)
	commit := state.projection.CandidateRevision
	if commit == "" {
		t.Fatal("no candidate commit")
	}
	dir := candidateDir(fixture.stateDir, runID)
	blob := mustGit(t, dir, "rev-parse", commit+":candidate.go")
	swapLooseObject(t, dir, blob, "blob", []byte("package standin\n"))
	fixture.reconcile(runID)
	branch := candidateBranch(runID)
	if got := mustGit(t, fixture.origin, "rev-parse", "refs/heads/"+branch); got != commit {
		t.Fatalf("the published branch is %q, want %s", got, commit)
	}
	if got := mustGit(t, fixture.origin, "cat-file", "blob", blob); got != "package candidate" {
		t.Fatalf("the remote holds %q behind the committed blob's name", got)
	}
}

// TestCommitGatesReadTheVerifiedSubjectStore (R1, before the commit): a
// harmless stand-in planted behind the name of a staged credential blob is
// what the candidate's object directory answers, so gates reading it there
// would pass the commit. The subject store refuses it and nothing is committed.
func TestCommitGatesReadTheVerifiedSubjectStore(t *testing.T) {
	w := commitGateWorkspace(t)
	before := headOf(t, w)
	key := pemPrivateKeyBlock()
	mustWrite(t, filepath.Join(w.Dir, "key.go"), key)
	name, err := runGitInput(w.Dir, []byte(key), "hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	swapLooseObject(t, w.Dir, strings.TrimSpace(string(name)), "blob", []byte("package harmless\n"))
	_, err = w.Commit("planted", 1<<20)
	if err == nil || !strings.Contains(err.Error(), errSubjectUnverified) {
		t.Fatalf("the gates read a planted stand-in instead of refusing it: %v", err)
	}
	if after := headOf(t, w); after != before {
		t.Fatalf("a refused commit moved HEAD from %s to %s", before, after)
	}
}

// TestPostCommitMetadataBaselineIsDerived (R3): a ref or config change made
// between update-ref and the post-commit comparison is an integrity violation
// reported with the commit, and the trusted baseline is the derived one - the
// change is never adopted.
func TestPostCommitMetadataBaselineIsDerived(t *testing.T) {
	t.Run("control", func(t *testing.T) {
		w := commitGateWorkspace(t)
		mustWrite(t, filepath.Join(w.Dir, "a.go"), realSource)
		if _, err := w.Commit("clean", 1<<20); err != nil {
			t.Fatal(err)
		}
		if err := w.AssertIntegrity(); err != nil {
			t.Fatalf("the derived baseline does not match an untouched repository: %v", err)
		}
	})
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, dir string)
	}{
		{"a planted ref", func(t *testing.T, dir string) { fixtureGit(t, dir, "update-ref", "refs/heads/planted", "HEAD") }},
		{"a config change", func(t *testing.T, dir string) { fixtureGit(t, dir, "config", "zenchron.tampered", "yes") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := commitGateWorkspace(t)
			mustWrite(t, filepath.Join(w.Dir, "a.go"), realSource)
			withAfterCommitUpdateRef(t, func(dir string) { tc.tamper(t, dir) })
			result, err := w.Commit("raced", 1<<20)
			var integrity *WorkspaceIntegrityError
			if !errors.As(err, &integrity) || result.Commit == "" || result.Commit != strings.TrimSpace(headOf(t, w)) {
				t.Fatalf("the change was not an integrity violation reported with the commit: %+v %v", result, err)
			}
			if err := w.AssertIntegrity(); err == nil {
				t.Fatal("the post-commit change was adopted into the trusted baseline")
			}
		})
	}
}

// TestAssuranceKeepsCommittedAttributesOnly: committed .gitattributes are
// repository semantics and apply in the verifier checkout; the candidate's
// own .git/info/attributes is provider-writable state and does not.
func TestAssuranceKeepsCommittedAttributesOnly(t *testing.T) {
	w := commitGateWorkspace(t)
	mustWrite(t, filepath.Join(w.Dir, ".gitattributes"), "crlf.txt eol=crlf\n")
	mustWrite(t, filepath.Join(w.Dir, "crlf.txt"), "a\nb\n")
	mustWrite(t, filepath.Join(w.Dir, "a.go"), realSource)
	result, err := w.Commit("attributes", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(w.Dir, ".git", "info", "attributes"), "*.go eol=crlf\n")
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := assuranceCheckout(w.Dir, checkout, result.Commit, result.Tree); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(checkout, "crlf.txt")); string(got) != "a\r\nb\r\n" {
		t.Fatalf("the committed eol attribute was not applied: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(checkout, "a.go")); string(got) != realSource {
		t.Fatalf("the candidate's info/attributes reached the checkout: %q", got)
	}
}

// TestAssuranceCheckoutSharesNoObjectFileWithTheStore: the verifier may write
// its checkout, so no object file there may be the subject store's own inode.
func TestAssuranceCheckoutSharesNoObjectFileWithTheStore(t *testing.T) {
	w, result, _ := committedSubject(t)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := assuranceCheckout(w.Dir, checkout, result.Commit, result.Tree); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(subjectStoreDir(w.Dir), "objects")
	compared := 0
	err := filepath.WalkDir(store, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(store, path)
		mine, errMine := os.Stat(path)
		theirs, errTheirs := os.Stat(filepath.Join(checkout, ".git", "objects", rel))
		if errMine != nil || errTheirs != nil {
			return nil
		}
		compared++
		if os.SameFile(mine, theirs) {
			return fmt.Errorf("%s is shared with the verifier checkout", rel)
		}
		return nil
	})
	if err != nil || compared == 0 {
		t.Fatalf("compared %d object files: %v", compared, err)
	}
}

// TestAForeignHeadIsNeverTheCandidatesAncestor: an external pull request head
// is recognised as the runtime's own only from the subject store. A graft or a
// commit object swapped in the candidate that would make a foreign commit look
// like the candidate's parent must not suppress candidate.external_changed.
func TestAForeignHeadIsNeverTheCandidatesAncestor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, dir, candidate, foreign string)
	}{
		{"a graft", func(t *testing.T, dir, candidate, foreign string) {
			mustWrite(t, filepath.Join(dir, ".git", "info", "grafts"), candidate+" "+foreign+"\n")
		}},
		// Git verifies a commit named on the command line, not the parents it
		// walks to, so the candidate's parent is swapped for one that names
		// the foreign commit as a parent of its own.
		{"a swapped parent commit object", func(t *testing.T, dir, candidate, foreign string) {
			parent := mustGit(t, dir, "rev-parse", candidate+"^")
			raw := mustGit(t, dir, "cat-file", "commit", parent)
			tree, rest, _ := strings.Cut(raw, "\n")
			swapLooseObject(t, dir, parent, "commit", []byte(tree+"\nparent "+foreign+"\n"+rest+"\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPhase8Fixture(t)
			runID := fixture.start()
			fixture.reconcile(runID)
			state := fixture.state(runID)
			candidate, number := state.projection.CandidateRevision, state.projection.PullRequest.Number
			if candidate == "" || number == 0 {
				t.Fatalf("the run did not publish: candidate %q, pull request %d", candidate, number)
			}
			dir := candidateDir(fixture.stateDir, runID)
			foreign := strings.TrimSpace(string(mustBytes(t)(runGitInput(dir, []byte("foreign\n"), "commit-tree", fixture.base+"^{tree}"))))
			tc.tamper(t, dir, candidate, foreign)
			if out, err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", foreign, candidate).CombinedOutput(); err != nil {
				t.Fatalf("the tamper did not take: plain git does not see the foreign head as an ancestor: %v %s", err, out)
			}
			fixture.inject(func(GitHubCall) error {
				pr := fixture.forge.PullRequests[number]
				pr.HeadSHA = foreign
				fixture.forge.PullRequests[number] = pr
				return nil
			})
			fixture.reconcile(runID)
			if got := fixture.state(runID).projection.ObservedExternalHead; got != foreign {
				t.Fatalf("a foreign head passed as the candidate's own: observed external head %q", got)
			}
		})
	}
}

// TestRuntimeGitIgnoresGrafts: through the runtime runner a graft cannot make
// an unrelated commit an ancestor.
func TestRuntimeGitIgnoresGrafts(t *testing.T) {
	w := commitGateWorkspace(t)
	head := strings.TrimSpace(headOf(t, w))
	foreign := strings.TrimSpace(string(mustBytes(t)(runGitInput(w.Dir, []byte("foreign\n"), "commit-tree", head+"^{tree}"))))
	mustWrite(t, filepath.Join(w.Dir, ".git", "info", "grafts"), head+" "+foreign+"\n")
	if out, err := exec.Command("git", "-C", w.Dir, "merge-base", "--is-ancestor", foreign, head).CombinedOutput(); err != nil {
		t.Fatalf("the graft did not take for plain git: %v %s", err, out)
	}
	if _, err := runGit(w.Dir, "merge-base", "--is-ancestor", foreign, head); err == nil {
		t.Fatal("a graft made an unrelated commit an ancestor through the runtime runner")
	}
}

func mustBytes(t *testing.T) func([]byte, error) []byte {
	return func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}
