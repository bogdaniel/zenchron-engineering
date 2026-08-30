package runtime

// These tests poison the host environment with t.Setenv and then prove that a
// brokered Git capability is unaffected. Every poisoned value points at a real
// decoy repository, config file, or executable script, so a leak would change
// observable behaviour or leave a marker file behind rather than merely being
// present in a string.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v: %v %s", args, err, out)
	}
}

// decoyGitTargets builds everything a leaked host environment could redirect a
// brokered Git operation onto: a second repository with its own uncommitted
// change, a global config that changes diff output and installs an external
// diff, and executable askpass/ssh helpers that leave marker files.
type decoys struct {
	repo, gitDir, globalConfig, askpass, sshCommand, authSock string
	markers                                                   []string
}

func buildDecoys(t *testing.T) decoys {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "decoy-repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "decoy.txt"), []byte("decoy-base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "init")
	fixtureGit(t, repo, "add", "-A")
	fixtureGit(t, repo, "-c", "user.email=d@example.com", "-c", "user.name=d", "commit", "-m", "decoy")
	// An uncommitted decoy change, so a leaked GIT_DIR/GIT_WORK_TREE produces a
	// diff that is obviously not the candidate's.
	if err := os.WriteFile(filepath.Join(repo, "decoy.txt"), []byte("decoy-leak-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}

	marker := func(name string) (string, string) {
		path := filepath.Join(base, name+".marker")
		script := filepath.Join(base, name+".sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\n: > "+path+"\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return script, path
	}
	askpass, askpassMarker := marker("askpass")
	ssh, sshMarker := marker("ssh")
	external, externalMarker := marker("global-external-diff")

	config := filepath.Join(base, "decoy-gitconfig")
	// diff.noprefix makes a leak of the global config observable even when the
	// external diff is independently refused by --no-ext-diff.
	body := "[diff]\n\tnoprefix = true\n\texternal = " + external + "\n[core]\n\tpager = " + external + "\n"
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return decoys{
		repo:         repo,
		gitDir:       filepath.Join(repo, ".git"),
		globalConfig: config,
		askpass:      askpass,
		sshCommand:   ssh,
		authSock:     filepath.Join(base, "agent.sock"),
		markers:      []string{askpassMarker, sshMarker, externalMarker},
	}
}

func (d decoys) poison(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_DIR", d.gitDir)
	t.Setenv("GIT_WORK_TREE", d.repo)
	t.Setenv("GIT_CONFIG_GLOBAL", d.globalConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", d.globalConfig)
	t.Setenv("GIT_SSH_COMMAND", d.sshCommand)
	t.Setenv("GIT_ASKPASS", d.askpass)
	t.Setenv("SSH_ASKPASS", d.askpass)
	t.Setenv("SSH_AUTH_SOCK", d.authSock)
	t.Setenv("GIT_EXTERNAL_DIFF", d.askpass)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(d.globalConfig))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(d.gitDir, "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(d.gitDir, "objects"))
}

func (d decoys) assertNoMarkers(t *testing.T, stage string) {
	t.Helper()
	for _, m := range d.markers {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("%s ran a host-configured helper program: %s exists", stage, m)
		}
	}
}

// TestTrustedGitEnvironmentIsBuiltFromScratch asserts the exact environment.
// The key set is compared exactly, so an inherited host variable of any name
// fails this test rather than only the names enumerated below.
func TestTrustedGitEnvironmentIsBuiltFromScratch(t *testing.T) {
	d := buildDecoys(t)
	d.poison(t)
	t.Setenv("GITHUB_TOKEN", "github_pat_gitrunner9c3")
	t.Setenv("HOME", d.repo)

	env := trustedGitEnv("/runtime/home", "/runtime")
	seen := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		if _, dup := seen[key]; dup {
			t.Fatalf("duplicate environment entry for %q", key)
		}
		seen[key] = value
	}
	want := map[string]string{
		"PATH":                    trustedPATH,
		"HOME":                    "/runtime/home",
		"LC_ALL":                  "C",
		"LANG":                    "C",
		"TZ":                      "UTC",
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_SYSTEM":       "/dev/null",
		"GIT_CONFIG_GLOBAL":       "/dev/null",
		"GIT_ATTR_NOSYSTEM":       "1",
		"GIT_TERMINAL_PROMPT":     "0",
		"GIT_PAGER":               "cat",
		"PAGER":                   "cat",
		"GIT_LITERAL_PATHSPECS":   "1",
		"GIT_CEILING_DIRECTORIES": "/runtime",
		"GIT_OPTIONAL_LOCKS":      "0",
	}
	var got, expected []string
	for k := range seen {
		got = append(got, k)
	}
	for k := range want {
		expected = append(expected, k)
	}
	sort.Strings(got)
	sort.Strings(expected)
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("trusted git environment is not built from scratch:\n got %v\nwant %v", got, expected)
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("trusted git environment %s = %q, want %q", k, seen[k], v)
		}
	}
	// No poisoned value may appear anywhere, under any name.
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{d.repo, d.globalConfig, d.askpass, d.sshCommand, d.authSock, "github_pat_gitrunner9c3"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("host value %q leaked into the trusted git environment: %v", leaked, env)
		}
	}
}

func TestBrokeredGitIgnoresPoisonedHostEnvironment(t *testing.T) {
	broker, outside := toolBrokerFixture(t)
	d := buildDecoys(t)
	if err := os.WriteFile(filepath.Join(broker.CandidateDir, "hello.txt"), []byte("candidate-modified-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d.poison(t)

	diff, err := broker.Diff(nil)
	if err != nil {
		t.Fatalf("brokered diff failed under a poisoned environment: %v", err)
	}
	// A leaked GIT_DIR/GIT_WORK_TREE would diff the decoy repository instead.
	if !strings.Contains(diff, "candidate-modified-9c3") {
		t.Fatalf("brokered diff did not observe the candidate workspace: %q", diff)
	}
	if strings.Contains(diff, "decoy-leak-9c3") || strings.Contains(diff, "decoy.txt") {
		t.Fatalf("brokered diff observed the decoy repository: %q", diff)
	}
	// A leaked GIT_CONFIG_GLOBAL would drop the a/ b/ prefixes (diff.noprefix).
	if !strings.Contains(diff, "a/hello.txt") || !strings.Contains(diff, "b/hello.txt") {
		t.Fatalf("brokered diff was shaped by host global Git config: %q", diff)
	}
	d.assertNoMarkers(t, "brokered diff")

	patch := "diff --git a/poisoned.txt b/poisoned.txt\nnew file mode 100644\n--- /dev/null\n+++ b/poisoned.txt\n@@ -0,0 +1 @@\n+applied-in-candidate-9c3\n"
	if err := broker.ApplyPatch([]byte(patch)); err != nil {
		t.Fatalf("brokered patch failed under a poisoned environment: %v", err)
	}
	applied, err := os.ReadFile(filepath.Join(broker.CandidateDir, "poisoned.txt"))
	if err != nil || string(applied) != "applied-in-candidate-9c3\n" {
		t.Fatalf("brokered patch did not write into the candidate workspace: %v %q", err, applied)
	}
	// A leaked GIT_WORK_TREE would have written into the decoy worktree.
	for _, elsewhere := range []string{filepath.Join(d.repo, "poisoned.txt"), filepath.Join(outside, "poisoned.txt")} {
		if _, err := os.Stat(elsewhere); err == nil {
			t.Fatalf("brokered patch wrote outside the candidate workspace: %s", elsewhere)
		}
	}
	if _, err := os.ReadFile(filepath.Join(d.repo, "decoy.txt")); err != nil {
		t.Fatal(err)
	}
	d.assertNoMarkers(t, "brokered patch")
}

// TestBrokeredDiffNeverRunsAnExternalDiffProgram covers the config-supplied
// drivers a stripped environment does not reach: diff.external in the
// repository's own config, and a .gitattributes diff driver with a command and
// a textconv. All three would create a marker file if they ran.
func TestBrokeredDiffNeverRunsAnExternalDiffProgram(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	base := t.TempDir()
	markers := map[string]string{}
	for _, name := range []string{"repo-external", "attr-command", "attr-textconv"} {
		path := filepath.Join(base, name+".marker")
		script := filepath.Join(base, name+".sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\n: > "+path+"\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		markers[name] = path
		switch name {
		case "repo-external":
			fixtureGit(t, broker.CandidateDir, "config", "diff.external", script)
		case "attr-command":
			fixtureGit(t, broker.CandidateDir, "config", "diff.leaky.command", script)
		case "attr-textconv":
			fixtureGit(t, broker.CandidateDir, "config", "diff.leaky.textconv", script)
		}
	}
	if err := os.WriteFile(filepath.Join(broker.CandidateDir, ".gitattributes"), []byte("hello.txt diff=leaky\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broker.CandidateDir, "hello.txt"), []byte("external-diff-probe-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := buildDecoys(t)
	d.poison(t)

	for _, scope := range [][]string{nil, {"hello.txt"}} {
		diff, err := broker.Diff(scope)
		if err != nil {
			t.Fatalf("brokered diff %v failed: %v", scope, err)
		}
		if !strings.Contains(diff, "+external-diff-probe-9c3") {
			t.Fatalf("brokered diff %v produced no unified diff: %q", scope, diff)
		}
		for name, path := range markers {
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("brokered diff %v ran the %s program", scope, name)
			}
		}
		d.assertNoMarkers(t, "brokered diff")
	}
}

// TestBrokeredPatchIsPreflightedBeforeItTouchesTheWorkspace covers the dry run.
// git apply is itself all-or-nothing, so the workspace assertion alone would
// hold without --check; the error identifies which stage refused the patch, so
// this also asserts that the refusal happened during preflight, before the
// runtime took the candidate mutation lock.
func TestBrokeredPatchIsPreflightedBeforeItTouchesTheWorkspace(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	partial := "diff --git a/hello.txt b/hello.txt\n--- a/hello.txt\n+++ b/hello.txt\n@@ -1 +1 @@\n-candidate-content-9c3\n+first-hunk-9c3\n" +
		"diff --git a/sub/other.txt b/sub/other.txt\n--- a/sub/other.txt\n+++ b/sub/other.txt\n@@ -1 +1 @@\n-does-not-exist\n+second-hunk-9c3\n"
	err := broker.ApplyPatch([]byte(partial))
	if err == nil {
		t.Fatal("a patch that cannot fully apply was accepted")
	}
	if !strings.Contains(err.Error(), "does not apply cleanly") {
		t.Fatalf("patch was not refused by the preflight dry run: %v", err)
	}
	unchanged, readErr := os.ReadFile(filepath.Join(broker.CandidateDir, "hello.txt"))
	if readErr != nil || string(unchanged) != "candidate-content-9c3\n" {
		t.Fatalf("a refused patch still mutated the workspace: %v %q", readErr, unchanged)
	}
}
