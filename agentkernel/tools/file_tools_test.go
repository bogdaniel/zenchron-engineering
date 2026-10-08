package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// escapeFixture builds a workspace whose granted root "src" holds every
// symlink shape that could escape it.
func escapeFixture(t *testing.T) *fixture {
	f := newFixture(t, map[string]string{
		"src/a.txt": "inside\n", "secret.txt": "workspace secret\n", "private/s.txt": "workspace secret\n",
		"private/sub/s.txt": "workspace secret\n",
	})
	symlink(t, "private", filepath.Join(f.root, "viadir"))                                   // granted root "viadir/sub" crosses a link
	symlink(t, "private", filepath.Join(f.root, "aliasroot"))                                // granted root linking elsewhere in the workspace
	symlink(t, "../secret.txt", filepath.Join(f.root, "src", "link"))                        // in workspace, outside grant
	symlink(t, filepath.Join(f.outside, "outside.txt"), filepath.Join(f.root, "src", "out")) // absolute, outside workspace
	symlink(t, f.outside, filepath.Join(f.root, "src", "dirlink"))                           // parent directory escape
	symlink(t, f.outside, filepath.Join(f.root, "linked"))                                   // granted root itself a link
	return f
}

var escapePaths = []string{
	"../secret.txt", "/etc/passwd", "src/../secret.txt", "src\\a.txt", "C:/src/a.txt", "c:src", "//host/share/a",
	"src/a.txt\x00", "src/link", "src/out", "src/dirlink/outside.txt", "linked/outside.txt", "aliasroot/s.txt", "viadir/sub/s.txt", "./src/a.txt", "src/",
}

func TestPathGuardsRefuseEscapesWithoutMutation(t *testing.T) {
	f := escapeFixture(t)
	before := digests(t, f.root, f.outside)
	grants := []api.Capability{
		grant("r", api.CapabilityFileRead, "src", "linked", "aliasroot", "viadir/sub"),
		grant("s", api.CapabilityFileSearch, "src", "linked", "aliasroot", "viadir/sub"),
		grant("w", api.CapabilityFileWrite, "src", "linked", "aliasroot", "viadir/sub"),
	}
	env := f.env(api.ModeReadWrite, grants...)
	secretDigest := api.Digest([]byte("workspace secret\n"))
	outsideDigest := api.Digest([]byte("outside secret"))
	for _, p := range escapePaths {
		calls := []api.ToolCall{
			call("read_file", map[string]string{"path": p}),
			call("search", map[string]string{"path": p, "pattern": "secret"}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": secretDigest}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": outsideDigest}),
			call("write_file", map[string]string{"path": p, "content": "pwned", "expected_sha256": Absent}),
			call("apply_patch", map[string]string{"path": p, "expected_sha256": secretDigest, "old_text": "secret", "new_text": "pwned"}),
		}
		for _, c := range calls {
			res, err := f.broker.Dispatch(context.Background(), c, env)
			if err != nil || res.Status == api.ToolOK || res.Mutated {
				t.Errorf("%s %q: got %+v, %v; want refusal or error without mutation", c.Name, p, res, err)
			}
			if strings.Contains(res.Output, "secret") {
				t.Errorf("%s %q leaked content: %q", c.Name, p, res.Output)
			}
		}
	}
	sameDigests(t, before, digests(t, f.root, f.outside))
}

func TestWritePreconditionNeverOverwritesNewerContent(t *testing.T) {
	f := newFixture(t, map[string]string{"src/a.txt": "v1\n"})
	env := f.env(api.ModeReadWrite, grant("w", api.CapabilityFileWrite, "src"))
	dispatch := func(name string, args map[string]string) api.ToolResult {
		t.Helper()
		res, err := f.broker.Dispatch(context.Background(), call(name, args), env)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	v1 := api.Digest([]byte("v1\n"))
	// Someone else writes v2 after the model read v1.
	writeFile(t, filepath.Join(f.root, "src/a.txt"), "v2\n")
	stale := []map[string]string{
		{"path": "src/a.txt", "content": "mine", "expected_sha256": v1},
		{"path": "src/a.txt", "content": "mine", "expected_sha256": Absent},
		{"path": "src/a.txt", "content": "mine", "expected_sha256": "sha256:abc"},
	}
	for _, args := range stale {
		if res := dispatch("write_file", args); res.Status != api.ToolError || res.Mutated {
			t.Fatalf("stale write %v: %+v", args, res)
		}
	}
	if res := dispatch("apply_patch", map[string]string{
		"path": "src/a.txt", "expected_sha256": v1, "old_text": "v", "new_text": "w"}); res.Status != api.ToolError {
		t.Fatalf("stale patch: %+v", res)
	}
	if got := readBack(t, f, "src/a.txt"); got != "v2\n" {
		t.Fatalf("newer content overwritten: %q", got)
	}
	v2 := api.Digest([]byte("v2\n"))
	res := dispatch("apply_patch", map[string]string{"path": "src/a.txt", "expected_sha256": v2, "old_text": "v2", "new_text": "v3"})
	if res.Status != api.ToolOK || !res.Mutated || res.Grant != "w" || !strings.Contains(res.Output, api.Digest([]byte("v3\n"))) {
		t.Fatalf("patch: %+v", res)
	}
	res = dispatch("write_file", map[string]string{"path": "src/new/b.txt", "content": "fresh", "expected_sha256": Absent})
	if res.Status != api.ToolOK || readBack(t, f, "src/new/b.txt") != "fresh" {
		t.Fatalf("create: %+v", res)
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".zk-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestApplyPatchNeedsExactlyOneOccurrence(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "x x\n"})
	env := f.env(api.ModeReadWrite, grant("w", api.CapabilityFileWrite, "."))
	for _, old := range []string{"x", "", "y"} {
		res, err := f.broker.Dispatch(context.Background(), call("apply_patch", map[string]string{
			"path": "a.txt", "expected_sha256": api.Digest([]byte("x x\n")), "old_text": old, "new_text": "z"}), env)
		if err != nil || res.Status != api.ToolError || res.Mutated {
			t.Fatalf("old_text %q: %+v, %v", old, res, err)
		}
	}
	if got := readBack(t, f, "a.txt"); got != "x x\n" {
		t.Fatalf("content changed: %q", got)
	}
}

func TestReadFileReportsDigestAndRange(t *testing.T) {
	content := "one\ntwo\nthree\n"
	f := newFixture(t, map[string]string{"a.txt": content})
	env := f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))
	res, err := f.broker.Dispatch(context.Background(), call("read_file", `{"path":"a.txt","start_line":2,"end_line":2}`), env)
	if err != nil || res.Status != api.ToolOK {
		t.Fatal(res, err)
	}
	want := "path: a.txt\ndigest: " + api.Digest([]byte(content)) + "\nlines: 2-2 of 3\n\ntwo\n"
	if res.Output != want {
		t.Fatalf("output %q, want %q", res.Output, want)
	}
	res, _ = f.broker.Dispatch(context.Background(), call("read_file", `{"path":"a.txt","start_line":9}`), env)
	if res.Status != api.ToolError {
		t.Fatalf("out-of-range read: %+v", res)
	}
}

func TestLargeFileDisclosesExcerptWithExactArtifact(t *testing.T) {
	content := strings.Repeat("0123456789\n", 1000)
	f := newFixture(t, map[string]string{"big.txt": content})
	env := f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))
	env.OutputLimit = 200
	res, err := f.broker.Dispatch(context.Background(), call("read_file", `{"path":"big.txt"}`), env)
	if err != nil || !res.Truncated || res.FullOutput == nil || len(res.Output) > 200 {
		t.Fatalf("got %+v, %v", res, err)
	}
	full, err := f.store.Get(context.Background(), *res.FullOutput)
	if err != nil || !strings.HasSuffix(string(full), content) || !strings.HasPrefix(string(full), res.Output) {
		t.Fatalf("artifact does not hold the full output: %v", err)
	}
}

func TestSearchIsScopedAndBounded(t *testing.T) {
	f := escapeFixture(t)
	writeFile(t, filepath.Join(f.root, "src/sub/b.go"), "package secret\nfunc Find() {}\n")
	env := f.env(api.ModeReadOnly, grant("s", api.CapabilityFileSearch, "src"))
	res, err := f.broker.Dispatch(context.Background(), call("search", `{"path":"src","pattern":"secret|outside"}`), env)
	if err != nil || res.Status != api.ToolOK {
		t.Fatal(res, err)
	}
	// Only the real file matches: symlinks to the workspace secret and the
	// outside directory are not followed.
	if res.Output != "src/sub/b.go:1: package secret\n" {
		t.Fatalf("search output %q", res.Output)
	}
	res, _ = f.broker.Dispatch(context.Background(), call("search", `{"path":"src","pattern":"F.nd()","literal":true}`), env)
	if res.Output != "no matches\n" {
		t.Fatalf("literal search treated as regexp: %q", res.Output)
	}
	res, _ = f.broker.Dispatch(context.Background(), call("search", `{"path":"src","pattern":"("}`), env)
	if res.Status != api.ToolError {
		t.Fatalf("invalid pattern: %+v", res)
	}
}

func TestNewWorkspaceRefusesRelativeRoot(t *testing.T) {
	if _, err := NewWorkspace("relative"); err == nil {
		t.Fatal("relative workspace accepted")
	}
}

func readBack(t *testing.T, f *fixture, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

// TestSameDirRefusesSwappedRoot covers the open-time recheck directly: a race
// that swaps a granted root between the symlink check and the open cannot be
// produced deterministically through Dispatch, so the recheck is driven with
// a held directory that is not the one the grant names.
func TestSameDirRefusesSwappedRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"src/a.txt": "a", "other/b.txt": "b"})
	ws, err := os.OpenRoot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	held, err := ws.OpenRoot("other")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := sameDir(ws, held, "src"); err == nil {
		t.Fatal("a held directory other than the granted root was accepted")
	}
	if err := sameDir(ws, held, "other"); err != nil {
		t.Fatalf("matching root refused: %v", err)
	}
}

// Repository metadata is out of reach of every grant, including a grant over
// the whole workspace: a write to .git/hooks would run code on the host.
func TestGitMetadataIsNeverReachable(t *testing.T) {
	f := newFixture(t, map[string]string{".git/config": "[remote] url=secret\n", "src/a.txt": "inside\n"})
	before := digests(t, f.root, f.outside)
	env := f.env(api.ModeReadWrite,
		grant("r", api.CapabilityFileRead, "."), grant("s", api.CapabilityFileSearch, "."),
		grant("w", api.CapabilityFileWrite, "."))
	for _, p := range []string{".git/config", ".GIT/config", ".git/hooks/pre-commit", "src/.git/x"} {
		for _, c := range []api.ToolCall{
			call("read_file", map[string]string{"path": p}),
			call("write_file", map[string]string{"path": p, "content": "x", "expected_sha256": Absent}),
		} {
			if res, err := f.broker.Dispatch(context.Background(), c, env); err != nil || res.Status == api.ToolOK {
				t.Errorf("%s %q: got %+v, %v; want refusal", c.Name, p, res, err)
			}
		}
	}
	res, err := f.broker.Dispatch(context.Background(), call("search", map[string]string{"path": ".", "pattern": "secret"}), env)
	if err != nil || strings.Contains(res.Output, "secret") {
		t.Errorf("search reached .git: %+v, %v", res, err)
	}
	sameDigests(t, before, digests(t, f.root, f.outside))
}

// TestSearchSkipsGitFiles: a submodule or worktree ".git" is a regular file
// ("gitdir: ..."); search skips any entry named .git, file or directory, in
// any case, so repository metadata is never read or echoed.
func TestSearchSkipsGitFiles(t *testing.T) {
	f := newFixture(t, map[string]string{
		"sub/.git": "gitdir: ../.git/modules/sub-gitsecret\n", "other/.GIT": "gitdir: gitsecret\n", "a.txt": "x\n",
	})
	env := f.env(api.ModeReadOnly, grant("s", api.CapabilityFileSearch, "."))
	res, err := f.broker.Dispatch(context.Background(), call("search", map[string]string{"path": ".", "pattern": "gitdir"}), env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "gitsecret") {
		t.Fatalf("search echoed .git file content: %q", res.Output)
	}
}
