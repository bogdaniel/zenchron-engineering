package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializeTreeWritesRawObjectBytes is defect AB.
//
// The build source used to be produced by `git archive | tar`, which was wrong
// twice over: it ran two programs found on the ambient PATH, and archive is a
// PRESENTATION of a tree rather than the tree itself. export-ignore drops
// paths, export-subst rewrites content, and a smudge filter transforms bytes on
// checkout. A build approved against tree T could therefore be compiled from
// something that is not T while every later check still named T.
//
// The fixture below arms all three attacks and proves none of them lands,
// because nothing is checked out and no external program is consulted: the
// objects are read through the controlled Git seam and written by Go.
func TestMaterializeTreeWritesRawObjectBytes(t *testing.T) {
	dir := t.TempDir()
	initFixtureRepo(t, dir, "keep.txt", "kept\n")

	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// export-ignore would drop this path from an archive; export-subst would
	// rewrite the $Format:%H$ token; the filter attribute would run a clean
	// program on `git add` and a smudge program on checkout.
	write(".gitattributes", "secret.txt export-ignore\nstamped.txt export-subst\nfiltered.txt filter=hostile\n")
	write("secret.txt", "this file must survive materialization\n")
	write("stamped.txt", "revision $Format:%H$\n")
	write("filtered.txt", "unfiltered bytes\n")
	write("nested/deep.txt", "nested\n")
	if err := os.Symlink("keep.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "keep.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "-q", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	tree := adoptedGit(t, dir, "rev-parse", "HEAD^{tree}")

	into := t.TempDir()
	deps := AdoptedBuildDeps{Git: gitOutput}.withDefaults()
	if err := materializeTree(deps, dir, tree, into); err != nil {
		t.Fatalf("the exact tree could not be materialized: %v", err)
	}

	// export-ignore did not drop it.
	if _, err := os.Stat(filepath.Join(into, "secret.txt")); err != nil {
		t.Fatalf("an export-ignore attribute removed a tracked path from the build source: %v", err)
	}
	// export-subst did not rewrite it.
	stamped, err := os.ReadFile(filepath.Join(into, "stamped.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stamped) != "revision $Format:%H$\n" {
		t.Fatalf("an export-subst attribute rewrote build source content: %q", stamped)
	}
	// The filter attribute changed nothing, because nothing was checked out.
	filtered, err := os.ReadFile(filepath.Join(into, "filtered.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(filtered) != "unfiltered bytes\n" {
		t.Fatalf("a filter attribute transformed build source content: %q", filtered)
	}
	// Modes and symlinks survive as the tree records them.
	info, err := os.Stat(filepath.Join(into, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Fatalf("an executable blob lost its mode: %v", info.Mode())
	}
	target, err := os.Readlink(filepath.Join(into, "link.txt"))
	if err != nil || target != "keep.txt" {
		t.Fatalf("a symlink was not materialized as one: %q / %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(into, "nested", "deep.txt")); err != nil {
		t.Fatalf("a nested path was not materialized: %v", err)
	}
	// And no Git metadata came along.
	if _, err := os.Stat(filepath.Join(into, ".git")); err == nil {
		t.Fatal("materialization brought Git metadata into the build source")
	}
}

// TestMaterializeTreeIgnoresAmbientHostState proves the proof side is held to
// the same standard as the build side: a hostile PATH and injected Git
// configuration cannot influence the measurement that claims the checkout
// equals the trusted tree.
func TestMaterializeTreeIgnoresAmbientHostState(t *testing.T) {
	dir := t.TempDir()
	initFixtureRepo(t, dir, "file.txt", "trusted content\n")
	tree := adoptedGit(t, dir, "rev-parse", "HEAD^{tree}")

	// A directory holding programs named `git` and `tar` that would corrupt
	// the result if either were ever resolved from PATH.
	hostile := t.TempDir()
	for _, name := range []string{"git", "tar"} {
		script := "#!/bin/sh\necho HOSTILE >&2\nexit 0\n"
		if err := os.WriteFile(filepath.Join(hostile, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", hostile)

	// Git configuration injection, in all the shapes Git accepts.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.autocrlf")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(hostile, "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(hostile, "gitconfig-system"))
	t.Setenv("GIT_ATTR_NOSYSTEM", "")
	t.Setenv("GIT_EXEC_PATH", hostile)
	injected := "[filter \"hostile\"]\n\tsmudge = sed s/trusted/hostile/\n[core]\n\tautocrlf = true\n"
	for _, name := range []string{"gitconfig", "gitconfig-system"} {
		if err := os.WriteFile(filepath.Join(hostile, name), []byte(injected), 0600); err != nil {
			t.Fatal(err)
		}
	}

	into := t.TempDir()
	deps := AdoptedBuildDeps{}.withDefaults()
	if err := materializeTree(deps, dir, tree, into); err != nil {
		t.Fatalf("materialization did not survive a hostile ambient environment: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(into, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "trusted content\n" {
		t.Fatalf("ambient host state changed the build source: %q", content)
	}
}

// TestMaterializeTreeRefusesWhatItCannotMaterialize: a submodule's content is
// not in this tree, and a path that escapes the checkout is not a path.
func TestMaterializeTreeRefusesWhatItCannotMaterialize(t *testing.T) {
	for name, path := range map[string]string{
		"absolute":     "/etc/passwd",
		"parent":       "../outside",
		"git metadata": ".git/config",
		"empty":        "",
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			if err := safeTreePath(path); err == nil {
				t.Fatalf("tree path %q was accepted", path)
			}
		})
	}
	if err := safeTreePath("cmd/zenchron-engineering/main.go"); err != nil {
		t.Fatalf("an ordinary tracked path was refused: %v", err)
	}

	// The object id is recomputed, so content that is not the object the tree
	// named cannot be written under its name.
	if blobID("kept\n") == blobID("something else\n") {
		return
	}
	dir := t.TempDir()
	initFixtureRepo(t, dir, "keep.txt", "kept\n")
	tree := adoptedGit(t, dir, "rev-parse", "HEAD^{tree}")
	deps := AdoptedBuildDeps{Git: func(d string, args ...string) (string, error) {
		if len(args) == 3 && args[0] == "cat-file" {
			return "something else\n", nil
		}
		return gitOutput(d, args...)
	}}.withDefaults()
	if err := materializeTree(deps, dir, tree, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "is not the tree it claims") {
		t.Fatalf("a mismatched object was accepted: %v", err)
	}
}
