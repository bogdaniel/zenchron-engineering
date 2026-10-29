package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

const (
	gitConfig = "[remote] url=gitsecret\n"
	gitHook   = "#!/bin/sh\necho hooksecret\n"
)

// aliasFixture is a workspace whose .git is reachable through in-workspace
// links that never name ".git" themselves.
func aliasFixture(t *testing.T) *fixture {
	f := newFixture(t, map[string]string{".git/config": gitConfig, ".git/hooks/pre-commit": gitHook, "src/a.txt": "inside\n"})
	symlink(t, ".git", filepath.Join(f.root, "meta"))              // directory alias
	symlink(t, ".git/config", filepath.Join(f.root, "cfg"))        // file alias
	symlink(t, "../.git", filepath.Join(f.root, "src", "x"))       // nested directory alias
	symlink(t, "a.txt", filepath.Join(f.root, "src", "alias.txt")) // stays inside the grant
	return f
}

// assertAliasesRefused dispatches every file tool at every path, expecting a
// refusal that mutates nothing and echoes no .git content, then checks that
// no file inside or outside the workspace changed. searchSkips accepts a
// search that skips the path ("no matches") instead of refusing it.
func assertAliasesRefused(t *testing.T, f *fixture, searchSkips bool, paths []string) {
	t.Helper()
	before := digests(t, f.root, f.outside)
	env := f.env(api.ModeReadWrite,
		grant("r", api.CapabilityFileRead, "."), grant("s", api.CapabilityFileSearch, "."),
		grant("w", api.CapabilityFileWrite, "."))
	configDigest, hookDigest := api.Digest([]byte(gitConfig)), api.Digest([]byte(gitHook))
	for _, p := range paths {
		calls := []api.ToolCall{
			call("read_file", map[string]string{"path": p}),
			call("read_files", map[string][]string{"paths": {p}}),
			call("search", map[string]string{"path": p, "pattern": "secret"}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": configDigest}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": hookDigest}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": Absent}),
			call("apply_patch", map[string]string{"path": p, "expected_sha256": configDigest, "old_text": "gitsecret", "new_text": "pwned"}),
			call("apply_patch", map[string]string{"path": p, "expected_sha256": hookDigest, "old_text": "hooksecret", "new_text": "pwned"}),
		}
		for _, c := range calls {
			res, err := f.broker.Dispatch(context.Background(), c, env)
			skipped := searchSkips && c.Name == "search" && res.Output == "no matches\n"
			if err != nil || (res.Status == api.ToolOK && !skipped) || res.Mutated {
				t.Errorf("%s %q: got %+v, %v; want refusal without mutation", c.Name, p, res, err)
			}
			if strings.Contains(res.Output+res.Error, "secret") {
				t.Errorf("%s %q leaked .git content: %q %q", c.Name, p, res.Output, res.Error)
			}
		}
	}
	res, err := f.broker.Dispatch(context.Background(), call("search", map[string]string{"path": ".", "pattern": "secret"}), env)
	if err != nil || res.Status != api.ToolOK || res.Output != "no matches\n" {
		t.Errorf("whole-workspace search reached .git through an alias: %+v, %v", res, err)
	}
	sameDigests(t, before, digests(t, f.root, f.outside))
}

// A symlink in any component below the granted root is refused, so a link
// that never names ".git" cannot alias repository metadata for reads,
// searches, writes, patches or file creation.
func TestSymlinkAliasToGitMetadataIsRefused(t *testing.T) {
	assertAliasesRefused(t, aliasFixture(t), false, []string{
		"meta", "meta/config", "meta/hooks/pre-commit", "meta/hooks/post-checkout",
		"cfg", "src/x", "src/x/config", "src/x/hooks/pre-commit", "src/x/hooks/post-checkout",
	})
}

func TestSymlinkInsideGrantIsRefused(t *testing.T) {
	f := aliasFixture(t)
	env := f.env(api.ModeReadWrite, grant("r", api.CapabilityFileRead, "src"), grant("w", api.CapabilityFileWrite, "src"))
	for _, c := range []api.ToolCall{
		call("read_file", map[string]string{"path": "src/alias.txt"}),
		call("write_file", map[string]string{"path": "src/alias.txt", "content": "x", "expected_sha256": api.Digest([]byte("inside\n"))}),
	} {
		res, err := f.broker.Dispatch(context.Background(), c, env)
		if err != nil || res.Status == api.ToolOK || res.Mutated || !strings.Contains(res.Error, "symlink") {
			t.Fatalf("%s through in-grant symlink: %+v, %v", c.Name, res, err)
		}
	}
	if got := readBack(t, f, "src/a.txt"); got != "inside\n" {
		t.Fatalf("link target changed: %q", got)
	}
}

// TestCheckOpenedRefusesSwappedFile drives the post-open recheck directly: a
// swap of the final component between refuseSymlinks and the open cannot be
// produced deterministically through Dispatch, so the file is opened, its name
// is then replaced, and the recheck must see that the name no longer holds it.
func TestCheckOpenedRefusesSwappedFile(t *testing.T) {
	f := aliasFixture(t)
	writeFile(t, filepath.Join(f.root, "src/b.txt"), "b\n")
	gr, err := os.OpenRoot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	for _, swap := range []func(tmp string){
		func(tmp string) { symlink(t, "../.git/config", tmp) },
		func(tmp string) { writeFile(t, tmp, "other\n") },
	} {
		opened, err := gr.Open("src/b.txt")
		if err != nil {
			t.Fatal(err)
		}
		info, err := opened.Stat()
		opened.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := checkOpened(gr, "src/b.txt", info); err != nil {
			t.Fatalf("unswapped file refused: %v", err)
		}
		tmp := filepath.Join(f.root, "src/swap")
		swap(tmp)
		if err := os.Rename(tmp, filepath.Join(f.root, "src/b.txt")); err != nil {
			t.Fatal(err)
		}
		if err := checkOpened(gr, "src/b.txt", info); err == nil {
			t.Fatal("a file whose name was swapped after the open was accepted")
		}
		if err := os.Remove(filepath.Join(f.root, "src/b.txt")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.root, "src/b.txt"), "b\n")
	}
}
