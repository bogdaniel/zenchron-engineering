package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures of the file-size tests commit into throwaway repositories, and
// must not depend on the machine they run on. The governed git runner gives git
// an empty HOME and no config, so a commit succeeds only if git can derive an
// identity on its own - from the system user's name and hostname - which a
// developer's machine usually can and a clean CI runner cannot ("Author identity
// unknown"). So the fixture states its identity explicitly, and forbids git from
// deriving one (user.useConfigOnly), so that a commit that forgot to would fail
// on a developer's machine exactly as it does on CI.

// sizeRepo commits files into a repository (creating it on first use) and
// returns the commit and its tree.
func sizeRepo(t *testing.T, dir string, files map[string]string, message string) (commit, tree string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		mustGit(t, dir, "init")
		// The only two repository config keys the governed runner will write.
		mustGit(t, dir, "config", "user.name", "Size Fixture")
		mustGit(t, dir, "config", "user.email", "size-fixture@example.invalid")
	}
	mustGit(t, dir, "add", "-A", "--")
	mustGit(t, dir, "commit", "-m", message)
	return mustGit(t, dir, "rev-parse", "HEAD"), mustGit(t, dir, "rev-parse", "HEAD^{tree}")
}

// canonicalSizeChecker is the repository's own script, run in dir with no host
// git configuration, identity or environment redirect reaching it: only PATH and
// locale are inherited, and HOME is a fresh directory.
func canonicalSizeChecker(t *testing.T, python, dir string) *exec.Cmd {
	t.Helper()
	command := exec.Command(python, "scripts/check_file_sizes.py")
	command.Dir = dir
	var env []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "HOME=") {
			continue
		}
		env = append(env, entry)
	}
	command.Env = append(env, "HOME="+t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return command
}
