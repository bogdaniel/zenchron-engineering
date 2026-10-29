package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// browserRepo builds a repository whose BASE commit carries the browser
// manifest, a trusted browser test and a trusted script, and whose CANDIDATE
// commit weakens the test and the script and adds a test of its own.
func browserRepo(t *testing.T) (dir, base, commit, tree string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "checkout")
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
	base = initFixtureRepo(t, dir, "go.mod", "module x\n")
	write(browserManifestFile, "# trusted\n"+strings.Join(browserSelection, "\n")+"\n")
	write("cmd/zenchron-engineering/browser_e2e_test.go", "package main\n// TRUSTED TEST\n")
	write("scripts/browser_e2e.mjs", "// TRUSTED SCRIPT\n")
	write("app/app.go", "package app\n// base app\n")
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "base with trusted browser tests")
	baseCommit, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	write("cmd/zenchron-engineering/browser_e2e_test.go", "package main\n// WEAKENED: always passes\n")
	write("cmd/zenchron-engineering/extra_test.go", "package main\n// candidate-added test\n")
	write("scripts/browser_e2e.mjs", "// WEAKENED SCRIPT: process.exit(0)\n")
	write("app/app.go", "package app\n// candidate app change\n")
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "candidate weakens the browser tests")
	head, _ := gitOutput(dir, "rev-parse", "HEAD")
	candidateTree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(baseCommit), strings.TrimSpace(head), strings.TrimSpace(candidateTree)
}

// A candidate that weakens, empties or adds browser tests cannot change what
// runs: the workspace holds the candidate's application and the BASE's tests.
func TestACandidateCannotWeakenTheBrowserTestsItIsVerifiedBy(t *testing.T) {
	dir, base, _, tree := browserRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	surface, err := browserWorkspace(dir, base, tree, workspace)
	if err != nil || surface == "" {
		t.Fatalf("surface=%q err=%v", surface, err)
	}
	read := func(path string) string {
		data, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil {
			return "<absent>"
		}
		return string(data)
	}
	if got := read("cmd/zenchron-engineering/browser_e2e_test.go"); !strings.Contains(got, "TRUSTED TEST") {
		t.Fatalf("the candidate's weakened test ran: %q", got)
	}
	if got := read("scripts/browser_e2e.mjs"); !strings.Contains(got, "TRUSTED SCRIPT") {
		t.Fatalf("the candidate's weakened script ran: %q", got)
	}
	if got := read("cmd/zenchron-engineering/extra_test.go"); got != "<absent>" {
		t.Fatalf("a test the candidate added joined the run: %q", got)
	}
	if info, err := os.Stat(filepath.Join(workspace, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("the workspace has no mountpoint for the masked .git directory: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(workspace, ".git")); len(entries) != 0 {
		t.Fatalf("repository metadata reached the browser workspace: %v", entries)
	}
	if got := read("app/app.go"); !strings.Contains(got, "candidate app change") {
		t.Fatalf("the application under test is not the candidate's: %q", got)
	}
	// The record names exactly which test sources ran, and it does not move when
	// only the candidate's own test files change.
	again, err := browserWorkspace(dir, base, tree, filepath.Join(t.TempDir(), "again"))
	if err != nil || again != surface {
		t.Fatalf("the surface digest is not stable: %q %q %v", surface, again, err)
	}
}
func TestTheBrowserManifestComesFromTheBaseAndMustBeWellFormed(t *testing.T) {
	dir, base, commit, _ := browserRepo(t)
	if names, err := readBrowserSelection(dir, base); err != nil || len(names) != len(browserSelection) {
		t.Fatalf("names=%v err=%v", names, err)
	}
	// A revision with no manifest refuses; it is never an empty, permissive list.
	if _, err := readBrowserSelection(dir, strings.TrimSpace(mustGitOutput(t, dir, "rev-list", "--max-parents=0", commit))); err == nil {
		t.Fatal("a base without a manifest was accepted")
	}
	for name, content := range map[string]string{
		"a command injected through a name": "TestOK; rm -rf /\n",
		"a path":                            "../TestOK\n",
		"a duplicate":                       "TestOK\nTestOK\n",
		"an empty manifest":                 "# nothing\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, browserManifestFile), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		mustGit(t, dir, "add", ".")
		mustGit(t, dir, "commit", "-q", "-m", name)
		head := strings.TrimSpace(mustGitOutput(t, dir, "rev-parse", "HEAD"))
		if _, err := readBrowserSelection(dir, head); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// hostile adds a commit on top of the browser fixture's candidate that applies
// a candidate-controlled change, and returns the tree to materialize.
func hostile(t *testing.T, dir string, change func(write func(path, content string))) string {
	t.Helper()
	change(func(path, content string) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	mustGit(t, dir, "add", "-A", "-f")
	mustGit(t, dir, "commit", "-q", "-m", "hostile candidate")
	return strings.TrimSpace(mustGitOutput(t, dir, "rev-parse", "HEAD^{tree}"))
}
func directoryListing(t *testing.T, dir string) map[string]string {
	t.Helper()
	listing := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(dir, path)
		if entry.IsDir() {
			listing[relative+"/"] = ""
			return nil
		}
		data, _ := os.ReadFile(path)
		listing[relative] = string(data)
		return nil
	})
	return listing
}

// A candidate that puts a symbolic link, or a plain file, where a trusted
// directory belongs cannot redirect what the controller writes or deletes.
func TestACandidateLinkCannotRedirectTheHostSideOverlay(t *testing.T) {
	for name, replace := range map[string]string{"cmd/zenchron-engineering": "link", "scripts": "link", "cmd": "link", "scripts-file": "file"} {
		t.Run(name, func(t *testing.T) {
			dir, base, _, _ := browserRepo(t)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "keep_test.go"), []byte("outside, must survive"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(outside, "zenchron-engineering"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "zenchron-engineering", "browser_e2e_test.go"), []byte("outside test, must survive"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := directoryListing(t, outside)
			target := strings.TrimSuffix(name, "-file")
			tree := hostile(t, dir, func(write func(path, content string)) {
				if err := os.RemoveAll(filepath.Join(dir, target)); err != nil {
					t.Fatal(err)
				}
				if replace == "file" {
					write(target, "a plain file where a directory belongs")
					return
				}
				if err := os.Symlink(outside, filepath.Join(dir, target)); err != nil {
					t.Skip(err)
				}
			})
			workspace := filepath.Join(t.TempDir(), "workspace")
			if _, err := browserWorkspace(dir, base, tree, workspace); err != nil {
				t.Fatal(err)
			}
			if after := directoryListing(t, outside); !reflect.DeepEqual(before, after) {
				t.Fatalf("a file outside the scratch workspace was created, changed or removed:\nbefore %v\nafter  %v", before, after)
			}
			for _, trusted := range []string{"cmd/zenchron-engineering", "scripts"} {
				info, err := os.Lstat(filepath.Join(workspace, trusted))
				if err != nil || !info.IsDir() {
					t.Fatalf("%s is not a real directory in the workspace: %v", trusted, err)
				}
			}
			if data, _ := os.ReadFile(filepath.Join(workspace, "cmd/zenchron-engineering/browser_e2e_test.go")); !strings.Contains(string(data), "TRUSTED TEST") {
				t.Fatalf("the base's test is not what the workspace holds: %q", data)
			}
		})
	}
}

// The workspace is exact Git-object bytes: attributes the candidate sets for
// archive export change nothing.
func TestCandidateAttributesCannotChangeTheMaterializedBytes(t *testing.T) {
	dir, base, _, _ := browserRepo(t)
	const substituted = "$Format:%H$ and $Format:%an$\n"
	tree := hostile(t, dir, func(write func(path, content string)) {
		write(".gitattributes", "* export-ignore\nsubst.txt export-subst\n* text eol=crlf\n")
		write("subst.txt", substituted)
		write("kept.txt", "line one\nline two\n")
	})
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := browserWorkspace(dir, base, tree, workspace); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"subst.txt": substituted, "kept.txt": "line one\nline two\n", ".gitattributes": "* export-ignore\nsubst.txt export-subst\n* text eol=crlf\n"} {
		got, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || string(got) != want {
			t.Errorf("%s was not materialized byte for byte: %q %v", path, got, err)
		}
	}
}
func TestTheOverlayIsComposedFromExactObjects(t *testing.T) {
	dir, base, _, tree := browserRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := browserWorkspace(dir, base, tree, workspace); err != nil {
		t.Fatal(err)
	}
	// Every file in the workspace is a blob of the candidate's tree or of the
	// base's surface, byte for byte.
	want := map[string]string{}
	for _, entry := range mustTreeEntries(t, dir, tree) {
		if !isBrowserSurface(entry.path) {
			want[entry.path] = entry.object
		}
	}
	for _, entry := range mustTreeEntries(t, dir, base) {
		if isBrowserSurface(entry.path) {
			want[entry.path] = entry.object
		}
	}
	for path, object := range want {
		data, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || blobID(string(data)) != object {
			t.Errorf("%s is not the object %s: %v", path, object, err)
		}
	}
	entries := 0
	_ = filepath.WalkDir(workspace, func(path string, entry os.DirEntry, err error) error {
		relative, _ := filepath.Rel(workspace, path)
		if err == nil && !entry.IsDir() && !strings.HasPrefix(relative, ".git") {
			entries++
		}
		return err
	})
	if entries != len(want) {
		t.Errorf("the workspace holds %d files, the exact objects name %d", entries, len(want))
	}
}
func mustTreeEntries(t *testing.T, dir, revision string) []browserTreeEntry {
	t.Helper()
	entries, err := browserTreeEntries(dir, revision)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
