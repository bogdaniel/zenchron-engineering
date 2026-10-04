package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hideStagedContent stages content at path, sets an index flag on it, and then
// leaves the worktree as worktree says (nil deletes the file): the #435 shape,
// where what the commit would carry is not what the gates would read.
func hideStagedContent(t *testing.T, w *CandidateWorkspace, path, content, flag string, worktree *string) {
	t.Helper()
	full := filepath.Join(w.Dir, path)
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "add", "--", path); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "update-index", flag, "--", path); err != nil {
		t.Fatal(err)
	}
	var err error
	if worktree == nil {
		err = os.Remove(full)
	} else {
		err = os.WriteFile(full, []byte(*worktree), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestCommitGateRefusesIndexFlaggedContent is #435: a flag-hidden blob is
// refused, by name, and nothing is committed.
func TestCommitGateRefusesIndexFlaggedContent(t *testing.T) {
	harmless := "package main\n"
	pem := "package main\n\n/*\n" + pemPrivateKeyBlock() + "\n*/\n"
	cases := []struct {
		name, content, flag string
		worktree            *string
		ceiling             int64
	}{
		{"skip-worktree, file deleted", pem, "--skip-worktree", nil, 1 << 20},
		{"skip-worktree, file rewritten", pem, "--skip-worktree", &harmless, 1 << 20},
		{"assume-unchanged, file rewritten", pem, "--assume-unchanged", &harmless, 1 << 20},
		{"size ceiling, file deleted", strings.Repeat("x", 4096), "--skip-worktree", nil, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := commitGateWorkspace(t)
			before, err := gitOutput(w.Dir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			hideStagedContent(t, w, "hidden.go", tc.content, tc.flag, tc.worktree)
			if err := os.WriteFile(filepath.Join(w.Dir, "visible.go"), []byte(harmless), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = w.Commit("hides content", tc.ceiling)
			if err == nil || !strings.Contains(err.Error(), `index-flagged candidate path "hidden.go"`) {
				t.Fatalf("the commit gate did not refuse the index-flagged path: %v", err)
			}
			if after, _ := gitOutput(w.Dir, "rev-parse", "HEAD"); after != before {
				t.Fatalf("a refused commit moved HEAD from %s to %s", before, after)
			}
		})
	}
}

// TestCommitGateScansContentBehindAHiddenGitlink: status names only the
// directory a staged gitlink became, but the commit carries its files, so the
// credential gate must read those files.
func TestCommitGateScansContentBehindAHiddenGitlink(t *testing.T) {
	w := commitGateWorkspace(t)
	hidden := workerTestScratchRepository(t, w.Dir, "nested", false)
	if _, err := runGit(w.Dir, "add", "--", hidden); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(w.Dir, hidden)
	if err := os.Rename(filepath.Join(nested, ".git"), filepath.Join(nested, "dot-git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "key.go"), []byte(pemPrivateKeyBlock()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := w.Commit("hides a key behind a gitlink", 1<<20)
	var material *CredentialMaterialError
	if !asCredentialMaterial(err, &material) || material.Path != hidden+"/key.go" {
		t.Fatalf("the credential behind a hidden gitlink was not refused by name: %v", err)
	}
}

func utf16LE(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i], 0)
	}
	return out
}

// encodingWorkspace declares notes.txt as working-tree-encoding=UTF-16LE,
// either in a committed .gitattributes or in .git/info/attributes, so `git add`
// converts the UTF-16 worktree file into a UTF-8 blob.
func encodingWorkspace(t *testing.T, committed bool) *CandidateWorkspace {
	t.Helper()
	w := commitGateWorkspace(t)
	rule := []byte("notes.txt working-tree-encoding=UTF-16LE\n")
	if !committed {
		if err := os.MkdirAll(filepath.Join(w.Dir, ".git", "info"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.Dir, ".git", "info", "attributes"), rule, 0o600); err != nil {
			t.Fatal(err)
		}
		return w
	}
	if err := os.WriteFile(filepath.Join(w.Dir, ".gitattributes"), rule, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", ".gitattributes"}, {"commit", "--no-gpg-sign", "-m", "attributes"}} {
		if _, err := runGit(w.Dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := gitMetadataDigest(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	w.TrustedMetadata = metadata
	return w
}

var encodingPlacements = map[string]bool{"committed .gitattributes": true, ".git/info/attributes": false}

// TestCommitRefusesWorkingTreeEncodedCredential is review finding B1 on #436:
// the worktree holds UTF-16 that no byte scan recognizes, the blob holds the
// UTF-8 key. Nothing may be committed.
func TestCommitRefusesWorkingTreeEncodedCredential(t *testing.T) {
	for name, committed := range encodingPlacements {
		t.Run(name, func(t *testing.T) {
			w := encodingWorkspace(t, committed)
			before, _ := gitOutput(w.Dir, "rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(w.Dir, "notes.txt"), utf16LE(pemPrivateKeyBlock()+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Commit("encoded key", 1<<20); err == nil {
				t.Fatal("a working-tree-encoded credential was committed")
			}
			if after, _ := gitOutput(w.Dir, "rev-parse", "HEAD"); after != before {
				t.Fatalf("a refused commit moved HEAD from %s to %s", before, after)
			}
		})
	}
}

// TestStagedGateJudgesStagedBytes asks the gates alone, with no backstop
// behind them, about blobs whose worktree file says something else: the
// encoded key (B1), a worktree swapped after staging (the TOCTOU), and a size
// that only the blob has.
func TestStagedGateJudgesStagedBytes(t *testing.T) {
	stageThenSwap := func(t *testing.T, w *CandidateWorkspace, path string, staged, worktree []byte) {
		t.Helper()
		full := filepath.Join(w.Dir, path)
		if err := os.WriteFile(full, staged, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(w.Dir, "add", "--", path); err != nil {
			t.Fatal(err)
		}
		if worktree != nil {
			if err := os.WriteFile(full, worktree, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	gate := func(t *testing.T, w *CandidateWorkspace, maxBytes int64) error {
		t.Helper()
		paths, blobs, err := stagedCommitPaths(w.Dir)
		if err != nil {
			t.Fatal(err)
		}
		return guardStagedContent(w.Dir, paths, blobs, maxBytes)
	}
	wantValue := func(t *testing.T, err error, path string) {
		t.Helper()
		var material *CredentialMaterialError
		if !asCredentialMaterial(err, &material) || material.Kind != CredentialMaterialValue || material.Path != path {
			t.Fatalf("the gate did not refuse the staged credential in %s: %v", path, err)
		}
	}
	for name, committed := range encodingPlacements {
		t.Run("encoded key via "+name, func(t *testing.T) {
			w := encodingWorkspace(t, committed)
			stageThenSwap(t, w, "notes.txt", utf16LE(pemPrivateKeyBlock()+"\n"), nil)
			wantValue(t, gate(t, w, 1<<20), "notes.txt")
		})
	}
	t.Run("worktree swapped after staging", func(t *testing.T) {
		w := commitGateWorkspace(t)
		stageThenSwap(t, w, "key.go", []byte(pemPrivateKeyBlock()), []byte("package main\n"))
		wantValue(t, gate(t, w, 1<<20), "key.go")
	})
	t.Run("size only the blob has", func(t *testing.T) {
		w := commitGateWorkspace(t)
		stageThenSwap(t, w, "bulk.txt", []byte(strings.Repeat("x", 4096)), []byte("x"))
		if err := gate(t, w, 1024); err == nil || !strings.Contains(err.Error(), "size ceiling") {
			t.Fatalf("the size ceiling did not judge the staged blob: %v", err)
		}
	})
	t.Run("clean staged change passes", func(t *testing.T) {
		w := commitGateWorkspace(t)
		stageThenSwap(t, w, "a.go", []byte("package main\n"), nil)
		if err := gate(t, w, 1<<20); err != nil {
			t.Fatalf("a clean staged change was refused: %v", err)
		}
	})
}

// TestWorktreeBackstopRefusesDivergence: the worktree later steps read must be
// the committed bytes exactly, raw - so a harmless converted file is refused
// too, which is the documented fail-closed cost for eol/encoding repositories.
func TestWorktreeBackstopRefusesDivergence(t *testing.T) {
	backstop := func(t *testing.T, w *CandidateWorkspace) error {
		t.Helper()
		_, blobs, err := stagedCommitPaths(w.Dir)
		if err != nil {
			t.Fatal(err)
		}
		return refuseWorktreeDivergence(w.Dir, blobs)
	}
	stage := func(t *testing.T, w *CandidateWorkspace, path string, content []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(w.Dir, path), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(w.Dir, "add", "--", path); err != nil {
			t.Fatal(err)
		}
	}
	w := commitGateWorkspace(t)
	stage(t, w, "a.go", []byte("package main\n"))
	if err := backstop(t, w); err != nil {
		t.Fatalf("an identical worktree was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a.go"), []byte("package other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backstop(t, w); err == nil {
		t.Fatal("a rewritten worktree file was accepted")
	}
	if err := os.Remove(filepath.Join(w.Dir, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := backstop(t, w); err == nil {
		t.Fatal("a deleted worktree file was accepted")
	}
	for name, committed := range encodingPlacements {
		t.Run("converted via "+name, func(t *testing.T) {
			w := encodingWorkspace(t, committed)
			stage(t, w, "notes.txt", utf16LE("harmless\n"))
			if err := backstop(t, w); err == nil {
				t.Fatal("a converted worktree file hash-matched its blob")
			}
		})
	}
}

// TestStagedLinkEntriesAreRefused: a symlink entry, new or a mode change on a
// tracked file, carries a link target rather than file bytes the gates judge.
func TestStagedLinkEntriesAreRefused(t *testing.T) {
	for _, path := range []string{"link", "README.md"} {
		t.Run(path, func(t *testing.T) {
			w := commitGateWorkspace(t)
			id, err := runGitInput(w.Dir, []byte("../outside"), "hash-object", "-w", "--stdin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(w.Dir, "update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(string(id))+","+path); err != nil {
				t.Fatal(err)
			}
			if _, _, err := stagedCommitPaths(w.Dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("a staged symlink entry was accepted: %v", err)
			}
		})
	}
}

func headOf(t *testing.T, w *CandidateWorkspace) string {
	t.Helper()
	head, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// TestCommitRefusesAMissingStagedBlob: a blob that cannot be read is refused
// before anything is committed, not left for a later reader to trip over.
func TestCommitRefusesAMissingStagedBlob(t *testing.T) {
	w := commitGateWorkspace(t)
	before := headOf(t, w)
	if err := os.WriteFile(filepath.Join(w.Dir, "gone.go"), []byte("package gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "add", "--", "gone.go"); err != nil {
		t.Fatal(err)
	}
	id, err := gitOutput(w.Dir, "rev-parse", ":gone.go")
	if err != nil {
		t.Fatal(err)
	}
	id = strings.TrimSpace(id)
	// Removing the object alone is not enough: `add -A` would write it again
	// from the worktree. An empty object file exists, so nothing rewrites it,
	// and it cannot be read.
	object := filepath.Join(w.Dir, ".git", "objects", id[:2], id[2:])
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	// The verified subject store (#437) refuses it before the gates read it.
	_, err = w.Commit("missing blob", 1<<20)
	if err == nil || !strings.Contains(err.Error(), errSubjectUnverified) {
		t.Fatalf("a missing staged blob was not refused by the gate: %v", err)
	}
	if after := headOf(t, w); after != before {
		t.Fatalf("a refused commit moved HEAD from %s to %s", before, after)
	}
}

// TestCommitCarriesAStagedDeletion: a deletion has no blob, so neither the
// gates nor the backstop may refuse it.
func TestCommitCarriesAStagedDeletion(t *testing.T) {
	w := commitGateWorkspace(t)
	if err := os.Remove(filepath.Join(w.Dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "added.go"), []byte("package added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("delete and add", 1<<20); err != nil {
		t.Fatalf("a staged deletion was refused: %v", err)
	}
	tree, err := gitOutput(w.Dir, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil || strings.TrimSpace(tree) != "added.go" {
		t.Fatalf("the commit did not carry the deletion and the addition: %q %v", tree, err)
	}
}

// TestCommitCredentialVerdictsOnStagedBlobs: over the scan ceiling is
// inconclusive (the blob is never read), and a token under a cache-shaped
// path is a value - no pathname exempts candidate content.
func TestCommitCredentialVerdictsOnStagedBlobs(t *testing.T) {
	cases := []struct {
		name, path string
		content    []byte
		kind       CredentialMaterialKind
		detail     string
	}{
		{"over the scan ceiling", "bulk.bin", make([]byte, credentialScanFileLimit+1), CredentialMaterialInconclusive, "file exceeds the deterministic scan ceiling"},
		{"token under a cache-shaped path", ".test-cache/38/3836bcb5017074e3b9d9e26170671e008544641fe4552451a3736b807d22cf4b-d", []byte(githubClassicTokenValue()), CredentialMaterialValue, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := commitGateWorkspace(t)
			before := headOf(t, w)
			full := filepath.Join(w.Dir, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := w.Commit("verdict", 1<<30)
			var material *CredentialMaterialError
			if !asCredentialMaterial(err, &material) || material.Kind != tc.kind || material.Path != tc.path || material.Detail != tc.detail {
				t.Fatalf("want %s refusal of %s, got %v", tc.kind, tc.path, err)
			}
			if after := headOf(t, w); after != before {
				t.Fatalf("a refused commit moved HEAD from %s to %s", before, after)
			}
		})
	}
}

// TestBackstopRefusesANewlinePath: --stdin-paths cannot name such a path, so it
// is refused by name rather than silently compared against something else.
func TestBackstopRefusesANewlinePath(t *testing.T) {
	w := commitGateWorkspace(t)
	name := "two\nlines.go"
	if err := os.WriteFile(filepath.Join(w.Dir, name), []byte("package two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "add", "--", name); err != nil {
		t.Fatal(err)
	}
	_, blobs, err := stagedCommitPaths(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	err = refuseWorktreeDivergence(w.Dir, blobs)
	if err == nil || !strings.Contains(err.Error(), `"two\nlines.go" cannot be compared`) {
		t.Fatalf("a newline path was not refused by name: %v", err)
	}
}
