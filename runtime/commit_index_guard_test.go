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

// TestStagedDivergenceIsRefused is the second half of #435, asked without the
// flag refusal in front of it: a staged blob that is not the worktree file the
// gates read is refused, and one that is is listed.
func TestStagedDivergenceIsRefused(t *testing.T) {
	harmless := "package main\n"
	for _, worktree := range []*string{nil, &harmless} {
		w := commitGateWorkspace(t)
		hideStagedContent(t, w, "hidden.go", pemPrivateKeyBlock(), "--skip-worktree", worktree)
		if _, err := stagedCommitPaths(w.Dir); err == nil {
			t.Fatal("a staged blob that differs from the worktree was accepted")
		}
	}
	w := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(w.Dir, "a.go"), []byte(harmless), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "add", "--", "a.go"); err != nil {
		t.Fatal(err)
	}
	if paths, err := stagedCommitPaths(w.Dir); err != nil || len(paths) != 1 || paths[0] != "a.go" {
		t.Fatalf("a staged blob equal to its worktree file was not listed: %q %v", paths, err)
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
