package runtime

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// markerProgram writes a real executable whose only job is to leave evidence.
// Every "did not execute" assertion in this file is backed by one of these, so
// a leak is observable rather than merely assumed.
func markerProgram(t *testing.T, dir, name, marker string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf 'ran\\n' >> " + marker + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoMarkerRan(t *testing.T, markerDir string) {
	t.Helper()
	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("runtime Git executed repository- or environment-controlled programs: %v", names)
	}
}

func initFixtureRepo(t *testing.T, dir, file, content string) string {
	t.Helper()
	if _, err := runGit("", "init", dir); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(head)
}

// TestRepositoryGitIgnoresAmbientEnvironmentPoisoning drives a real clone,
// status, commit, fetch, rebase, checkout, reset and clean while the host
// environment points every redirectable Git variable at a real decoy
// repository and every program hook at a real marker executable.
func TestRepositoryGitIgnoresAmbientEnvironmentPoisoning(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	decoy := filepath.Join(root, "decoy")
	base := initFixtureRepo(t, origin, "README.md", "real\n")
	decoyHead := initFixtureRepo(t, decoy, "DECOY.md", "decoy\n")

	markerDir := filepath.Join(root, "markers")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(markerDir, 0700); err != nil {
		t.Fatal(err)
	}
	ssh := markerProgram(t, binDir, "ssh-marker", filepath.Join(markerDir, "ssh"))
	askpass := markerProgram(t, binDir, "askpass-marker", filepath.Join(markerDir, "askpass"))
	editor := markerProgram(t, binDir, "editor-marker", filepath.Join(markerDir, "editor"))
	external := markerProgram(t, binDir, "diff-marker", filepath.Join(markerDir, "diff"))
	helper := markerProgram(t, binDir, "credential-marker", filepath.Join(markerDir, "credential"))
	pager := markerProgram(t, binDir, "pager-marker", filepath.Join(markerDir, "pager"))
	proxy := markerProgram(t, binDir, "proxy-marker", filepath.Join(markerDir, "proxy"))
	// A fake `git` first on the ambient PATH: if the runner resolved its binary
	// against the inherited PATH instead of the trusted one, this would run.
	fakeBin := filepath.Join(root, "fakebin")
	markerProgram(t, fakeBin, "git", filepath.Join(markerDir, "fake-git"))

	// A poisoned hook directory and a poisoned clone/init template, both real.
	poisonHooks := filepath.Join(root, "poison-hooks")
	for _, hook := range []string{"pre-commit", "post-checkout", "post-merge", "post-rewrite", "post-applypatch"} {
		markerProgram(t, poisonHooks, hook, filepath.Join(markerDir, "hook-"+hook))
	}
	poisonTemplate := filepath.Join(root, "poison-template")
	markerProgram(t, filepath.Join(poisonTemplate, "hooks"), "post-checkout", filepath.Join(markerDir, "template-hook"))

	poisonConfig := filepath.Join(root, "poison.gitconfig")
	config := "[core]\n\thooksPath = " + poisonHooks + "\n\tsshCommand = " + ssh +
		"\n\tpager = " + pager + "\n\tgitProxy = " + proxy + "\n\taskpass = " + askpass +
		"\n\teditor = " + editor + "\n" +
		"[init]\n\ttemplateDir = " + poisonTemplate + "\n" +
		"[commit]\n\tgpgSign = true\n[tag]\n\tgpgSign = true\n" +
		"[gpg]\n\tprogram = " + helper + "\n" +
		"[diff]\n\texternal = " + external + "\n" +
		"[filter \"poison\"]\n\tclean = " + external + "\n\tsmudge = " + external + "\n" +
		"[credential]\n\thelper = " + helper + "\n" +
		"[protocol]\n\tallow = always\n" +
		"[url \"ext::sh -c " + external + "\"]\n\tinsteadOf = https://\n" +
		"[fetch]\n\trecurseSubmodules = true\n"
	if err := os.WriteFile(poisonConfig, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	poisonHome := filepath.Join(root, "poison-home")
	if err := os.MkdirAll(poisonHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poisonHome, ".gitconfig"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]string{
		"HOME":                             poisonHome,
		"PATH":                             fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_CONFIG_HOME":                  poisonHome,
		"GIT_DIR":                          filepath.Join(decoy, ".git"),
		"GIT_WORK_TREE":                    decoy,
		"GIT_INDEX_FILE":                   filepath.Join(decoy, ".git", "poison-index"),
		"GIT_OBJECT_DIRECTORY":             filepath.Join(decoy, ".git", "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(decoy, ".git", "objects"),
		"GIT_NAMESPACE":                    "poison",
		"GIT_CONFIG_GLOBAL":                poisonConfig,
		"GIT_CONFIG_SYSTEM":                poisonConfig,
		"GIT_CONFIG_NOSYSTEM":              "0",
		"GIT_ATTR_NOSYSTEM":                "0",
		"GIT_TEMPLATE_DIR":                 poisonTemplate,
		"GIT_SSH":                          ssh,
		"GIT_SSH_COMMAND":                  ssh,
		"GIT_ASKPASS":                      askpass,
		"SSH_ASKPASS":                      askpass,
		"SSH_AUTH_SOCK":                    filepath.Join(root, "agent.sock"),
		"GIT_EXTERNAL_DIFF":                external,
		"GIT_PROXY_COMMAND":                proxy,
		"GIT_EDITOR":                       editor,
		"GIT_SEQUENCE_EDITOR":              editor,
		"GIT_PAGER":                        pager,
		"PAGER":                            pager,
		"GIT_TERMINAL_PROMPT":              "1",
		"GIT_LITERAL_PATHSPECS":            "0",
		"GIT_OPTIONAL_LOCKS":               "1",
		"GIT_COMMITTER_NAME":               "poison",
		"GIT_AUTHOR_NAME":                  "poison",
	} {
		t.Setenv(name, value)
	}

	// clone
	w, err := CreateCandidateClone(filepath.Join(root, "state"), "run", origin, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(w.Dir, "README.md")); err != nil || string(got) != "real\n" {
		t.Fatalf("clone followed a poisoned repository: %q %v", got, err)
	}
	// status / metadata inspection
	if err := os.WriteFile(filepath.Join(w.Dir, "candidate.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := changedPaths(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "candidate.txt" {
		t.Fatalf("status was redirected to another index or work tree: %q", paths)
	}
	// commit
	result, err := w.Commit("candidate", 4096)
	if err != nil {
		t.Fatal(err)
	}
	// CreateAssuranceCheckout lives in a frozen file and reaches Git only
	// through runGit, so hardening runGit is what covers it. Prove that.
	assurance := filepath.Join(root, "assurance")
	if err := CreateAssuranceCheckout(w.Dir, assurance, result.Commit, result.Tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(assurance, "candidate.txt")); err != nil {
		t.Fatalf("assurance checkout did not bind the candidate tree: %v", err)
	}
	// fetch a real advance, then rebase and checkout
	branch, err := gitOutput(origin, "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "other.txt"), []byte("advance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-m", "advance"); err != nil {
		t.Fatal(err)
	}
	if err := w.FetchBase("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Rebase("origin/" + strings.TrimSpace(branch)); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "checkout", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	// reset --hard and clean -fdx
	if err := os.WriteFile(filepath.Join(w.Dir, "junk.txt"), []byte("junk\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.RestoreTrusted(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("clean did not act on the runtime workspace")
	}

	// The decoy is untouched: no ref moved, no file changed, no index written.
	if head, err := runDecoyGit(t, decoy, "rev-parse", "HEAD"); err != nil || head != decoyHead {
		t.Fatalf("poisoned GIT_DIR was honoured: %q %v", head, err)
	}
	if got, err := os.ReadFile(filepath.Join(decoy, "DECOY.md")); err != nil || string(got) != "decoy\n" {
		t.Fatalf("poisoned GIT_WORK_TREE was honoured: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(decoy, ".git", "poison-index")); !os.IsNotExist(err) {
		t.Fatal("poisoned GIT_INDEX_FILE was honoured")
	}
	assertNoMarkerRan(t, markerDir)

	// Nothing from the poisoned configuration reached the runtime-owned
	// repository config that the integrity baseline covers.
	local, err := gitOutput(w.Dir, "config", "--list", "--local")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"hookspath", "filter.", "credential.", "gpgsign=true", "sshcommand", "insteadof"} {
		if strings.Contains(strings.ToLower(local), forbidden) {
			t.Fatalf("poisoned configuration reached repository config: %q", local)
		}
	}
}

// runDecoyGit inspects the decoy with an explicitly clean environment so the
// assertion itself cannot be fooled by the poisoning the test installed.
func runDecoyGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	binary, err := gitBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = repositoryGitEnv(home, home, "")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// TestRepositoryGitNeverRunsRepositoryControlledPrograms covers the other half
// of the execution surface: programs a *repository* can nominate, through its
// own hooks directory, its own config, and .gitattributes-selected drivers.
func TestRepositoryGitNeverRunsRepositoryControlledPrograms(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	base := initFixtureRepo(t, origin, "README.md", "real\n")
	markerDir := filepath.Join(root, "markers")
	if err := os.MkdirAll(markerDir, 0700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	driver := markerProgram(t, binDir, "driver", filepath.Join(markerDir, "driver"))

	w, err := CreateCandidateClone(filepath.Join(root, "state"), "run", origin, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Real hooks in the repository's own hook directory.
	for _, hook := range []string{"pre-commit", "post-commit", "post-checkout", "post-merge", "post-rewrite", "pre-auto-gc"} {
		markerProgram(t, filepath.Join(w.Dir, ".git", "hooks"), hook, filepath.Join(markerDir, "hook-"+hook))
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "candidate.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("candidate", 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(w.Dir, "checkout", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := w.RestoreTrusted(); err != nil {
		t.Fatal(err)
	}
	assertNoMarkerRan(t, markerDir)

	// The runtime refuses to write a config key outside its own allowlist, so
	// it cannot be talked into installing an execution path itself.
	if _, err := runGit(w.Dir, "config", "core.hooksPath", filepath.Join(root, "elsewhere")); err == nil {
		t.Fatal("runtime wrote a security-sensitive repository config key")
	}
	if _, err := runGit(w.Dir, "config", "--global", "user.name", "x"); err == nil {
		t.Fatal("runtime wrote configuration outside the repository")
	}

	// A repository that carries its own execution config is refused outright,
	// including on the recovery path, which has no integrity baseline.
	attributes := filepath.Join(w.Dir, ".gitattributes")
	if err := os.WriteFile(attributes, []byte("* filter=evil diff=evil\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(w.Dir, ".git", "config")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, injected := range []string{
		"\n[filter \"evil\"]\n\tclean = " + driver + "\n\tsmudge = " + driver + "\n",
		"\n[diff \"evil\"]\n\tcommand = " + driver + "\n",
		"\n[core]\n\thooksPath = " + binDir + "\n",
		"\n[credential]\n\thelper = " + driver + "\n",
		"\n[url \"ext::sh -c " + driver + "\"]\n\tinsteadOf = https://\n",
		// Git accepts a key on the section-header line; so must the check.
		"\n[core] hooksPath = " + binDir + "\n",
	} {
		if err := os.WriteFile(configPath, append(append([]byte{}, original...), []byte(injected)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(w.Dir, "status", "--porcelain=v1"); err == nil {
			t.Fatalf("accepted repository config granting execution: %q", injected)
		}
		if err := w.RestoreTrusted(); err == nil {
			t.Fatalf("recovery path accepted repository config granting execution: %q", injected)
		}
	}
	if err := os.WriteFile(configPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	assertNoMarkerRan(t, markerDir)
}

func TestGovernedRemoteFailsClosed(t *testing.T) {
	local := t.TempDir()
	for _, remote := range []string{
		"", "   ", "origin",
		"ssh://git@github.com/o/r.git",
		"git@github.com:o/r.git",
		"git://github.com/o/r.git",
		"http://github.com/o/r.git",
		"file:///etc",
		"ext::sh -c 'touch /tmp/pwned'",
		"https://github.com/o/r/extra",
		"https://github.com/o",
		"https://user:token@github.com/o/r.git",
		"https://evil.example/o/r.git",
		"https://github.com:8443/o/r.git",
		"--upload-pack=/bin/sh",
		"-u/bin/sh",
		"./relative/path",
		filepath.Join(local, "missing"),
	} {
		if identity, err := GovernedRemote(remote); err == nil {
			t.Fatalf("accepted ungoverned remote %q as %+v", remote, identity)
		}
	}
	https, err := GovernedRemote("https://github.com/owner/name.git")
	if err != nil || https.Transport() != "https" {
		t.Fatalf("rejected the governed remote: %v %v", https, err)
	}
	file, err := GovernedRemote(local)
	if err != nil || file.Transport() != "file" {
		t.Fatalf("rejected the governed local repository: %v %v", file, err)
	}
}

func TestRemoteOperationsBindToTheGovernedRemote(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	other := filepath.Join(root, "other")
	base := initFixtureRepo(t, origin, "README.md", "real\n")
	initFixtureRepo(t, other, "OTHER.md", "other\n")

	if _, err := CreateCandidateClone(filepath.Join(root, "s1"), "run", "ssh://git@github.com/o/r.git", base, nil); err == nil {
		t.Fatal("cloned an ungoverned remote")
	}
	w, err := CreateCandidateClone(filepath.Join(root, "state"), "run", origin, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.Remote != origin {
		t.Fatalf("workspace did not record its governed remote: %q", w.Remote)
	}
	// A remote bound to one identity refuses to clone another source.
	governed, err := GovernedRemote(origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteGit("", governed, nil).run("clone", "--no-checkout", other, filepath.Join(root, "wrong")); err == nil {
		t.Fatal("network operation followed a source other than the governed remote")
	}
	// A workspace whose configured remote no longer matches the governed one
	// cannot fetch.
	drifted := w
	drifted.Remote = other
	if _, err := drifted.boundRemote("origin"); err == nil {
		t.Fatal("fetch bound to a remote other than the governed one")
	}
	if _, err := w.boundRemote("https://github.com/other/repo.git"); err == nil {
		t.Fatal("fetch accepted a caller-supplied remote URL")
	}
	// The local profile can never open a network transport by itself.
	if _, err := runGit(w.Dir, "fetch", "origin"); err == nil {
		t.Fatal("local profile performed a fetch")
	}
	if _, err := runGit("", "clone", "https://github.com/o/r.git", filepath.Join(root, "net")); err == nil {
		t.Fatal("local profile opened a network transport")
	}
	for _, args := range [][]string{{"push", "origin", "HEAD"}, {"pull"}, {"ls-remote", origin}, {"submodule", "update", "--init"}} {
		if _, err := runGit(w.Dir, args...); err == nil {
			t.Fatalf("accepted refused subcommand %v", args)
		}
	}
}

type fixedCredential struct {
	username, secret string
	seen             []string
}

func (c *fixedCredential) Credential(identity RemoteIdentity) (string, string, error) {
	c.seen = append(c.seen, identity.URL)
	return c.username, c.secret, nil
}

// TestRemoteCredentialSeamAuthenticatesWithoutLeaking uses a local HTTP Git
// endpoint that demands Basic auth. No real credential and no real host is
// involved, and the secret must never appear in argv, the child environment,
// or the resulting repository.
func TestRemoteCredentialSeamAuthenticatesWithoutLeaking(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	initFixtureRepo(t, origin, "README.md", "real\n")
	served := filepath.Join(root, "served.git")
	if _, err := runGit("", "clone", "--bare", origin, served); err != nil {
		t.Skipf("cannot build a served repository: %v", err)
	}
	if _, err := runGit(served, "update-server-info"); err != nil {
		t.Fatal(err)
	}

	const user, secret = "x-access-token", "s3cr3t-not-a-real-token"
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
	var authenticated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="zenchron"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		http.FileServer(http.Dir(served)).ServeHTTP(w, r)
	}))
	defer server.Close()

	requireExecutableTemp(t)

	credentials := &fixedCredential{username: user, secret: secret}
	identity := RemoteIdentity{URL: server.URL + "/", transport: transportInsecureHTTP}
	destination := filepath.Join(root, "clone")
	runner := RepositoryGitRunner{
		Local:  controlPolicy(),
		Remote: &RemotePolicy{Identity: identity, Credentials: credentials},
	}
	if _, err := runner.run("clone", "--no-checkout", identity.URL, destination); err != nil {
		t.Fatalf("credentialed clone failed: %v", err)
	}
	if !authenticated.Load() || len(credentials.seen) == 0 {
		t.Fatal("the credential seam was not the thing that authenticated")
	}
	// The same operation without the credential capability must fail closed.
	bare := RepositoryGitRunner{Local: controlPolicy(), Remote: &RemotePolicy{Identity: identity}}
	if _, err := bare.run("clone", "--no-checkout", identity.URL, filepath.Join(root, "clone2")); err == nil {
		t.Fatal("authenticated without the credential capability")
	}

	// The secret is nowhere the candidate, an artifact, or an event can see it.
	config, err := os.ReadFile(filepath.Join(destination, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), secret) {
		t.Fatal("credential landed in repository config")
	}
	for _, entry := range repositoryGitEnv("/h", "/t", "/a") {
		if strings.Contains(entry, secret) {
			t.Fatalf("credential reachable through the environment: %q", entry)
		}
	}
	for _, arg := range runner.runtimeConfigOverrides("/hooks", transportInsecureHTTP) {
		if strings.Contains(arg, secret) {
			t.Fatalf("credential reachable through argv: %q", arg)
		}
	}
	// Nothing of the askpass program survives the call.
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "zenchron-repo-git-") {
			t.Fatalf("runtime git scratch directory leaked: %s", e.Name())
		}
	}
}

// requireExecutableTemp skips a test that has to RUN a program it wrote into
// the temp filesystem. This runtime's own assurance sandbox mounts /tmp noexec
// on purpose - only the Go toolchain gets an exec-capable writable mount - so
// inside it these tests cannot execute their own fixture. Skipping names that
// condition instead of reporting the sandbox's boundary as a defect in the
// credential seam, and the tests still run wherever exec is permitted, which
// includes the host and CI.
//
// assertTempExecutable is the same check the runtime itself makes before the
// credential seam depends on an askpass program, so the test skips exactly
// when the runtime would refuse.
func requireExecutableTemp(t *testing.T) {
	t.Helper()
	if err := assertTempExecutable(t.TempDir()); err != nil {
		t.Skipf("this environment does not permit executing a program from the temp filesystem: %v", err)
	}
}

func TestAskpassProgramIsPrivateAndAnswersOnlyGit(t *testing.T) {
	requireExecutableTemp(t)
	dir := t.TempDir()
	path, err := writeAskpass(dir, "user'name", "se'cret")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("askpass program is not private: %v", info.Mode().Perm())
	}
	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ": "user'name",
		"Password for 'https://github.com': ": "se'cret",
	} {
		out, err := exec.Command(path, prompt).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("askpass answered %q with %q (%v)", prompt, out, err)
		}
	}
}

func TestRepositoryGitEnvironmentIsBuiltFromScratch(t *testing.T) {
	t.Setenv("GIT_DIR", "/decoy/.git")
	t.Setenv("SSH_AUTH_SOCK", "/decoy/agent")
	t.Setenv("EDITOR", "/decoy/editor")
	env := repositoryGitEnv("/home", "/template", "")
	names := repositoryGitEnvNames(env)
	for _, excluded := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_EXTERNAL_DIFF",
		"GIT_DIFF_OPTS", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS",
		"SSH_AUTH_SOCK", "GIT_PROXY_COMMAND", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR",
		"EDITOR", "XDG_CONFIG_HOME", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
		"GIT_CREDENTIAL_HELPER",
	} {
		if _, ok := names[excluded]; ok {
			t.Fatalf("%s is present in a from-scratch environment", excluded)
		}
	}
	for key, want := range map[string]string{
		"PATH": trustedPATH, "HOME": "/home", "LC_ALL": "C", "LANG": "C", "TZ": "UTC",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": "/dev/null",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_ATTR_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat",
		"GIT_OPTIONAL_LOCKS": "0", "GIT_LITERAL_PATHSPECS": "1",
		"GIT_TEMPLATE_DIR": "/template",
	} {
		if names[key] != want {
			t.Fatalf("%s = %q, want %q", key, names[key], want)
		}
	}
	if len(env) != len(names) {
		t.Fatal("duplicate environment entries")
	}
	if _, ok := repositoryGitEnvNames(repositoryGitEnv("/home", "/template", "/askpass"))["GIT_ASKPASS"]; !ok {
		t.Fatal("an authorized credential must reach the runtime Git process")
	}
}

func repositoryGitEnvNames(env []string) map[string]string {
	names := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		names[key] = value
	}
	return names
}

// ---------------------------------------------------------------------------
// Per-runtime credential authority
// ---------------------------------------------------------------------------

// scopedCredential is an operator credential bound to exactly one governed
// remote. It records every resolution and refuses any other identity, so a
// credential reached from the wrong runtime is both visible here and fatal to
// the Git operation that reached it.
type scopedCredential struct {
	remote, secret string
	resolved       []string
}

func (c *scopedCredential) Credential(identity RemoteIdentity) (string, string, error) {
	c.resolved = append(c.resolved, identity.URL)
	if identity.URL != c.remote {
		return "", "", fmt.Errorf("credential is not authorized for %q", identity.URL)
	}
	return "x-access-token", c.secret, nil
}

// TestTwoRuntimesNeverCrossUseGitCredentials builds two runtimes over two
// repositories with two independent credential providers and drives each
// runtime's REAL repository-control network path - the runtime-owned clone and
// the runtime-owned base fetch. Nothing here inspects a struct field: the
// assertion is which provider object a running git subprocess actually asked
// for a secret, and which governed remote it was asked about.
//
// This is the test a package-global credential seam cannot pass. With a global,
// neither runtime's operations resolve the provider it was constructed with, so
// the "did not resolve its own credential" assertions fail.
func TestTwoRuntimesNeverCrossUseGitCredentials(t *testing.T) {
	// Both runtimes drive a REAL credentialed clone, which needs an askpass
	// program the temp filesystem must be able to run.
	requireExecutableTemp(t)

	a, b := newPhase8Fixture(t), newPhase8Fixture(t)
	if a.deps.Remote.URL == b.deps.Remote.URL {
		t.Fatal("the two runtimes must govern different repositories")
	}
	credentialA := &scopedCredential{remote: a.deps.Remote.URL, secret: "secret-for-runtime-a"}
	credentialB := &scopedCredential{remote: b.deps.Remote.URL, secret: "secret-for-runtime-b"}
	a.deps.Credentials, b.deps.Credentials = credentialA, credentialB
	a.runtime, b.runtime = a.newRuntime(a.deps), b.newRuntime(b.deps)

	drive := func(f *phase8Fixture) {
		t.Helper()
		runID := f.start()
		f.crash(runID, func(call GitHubCall) bool {
			return call.Method == "RefSHA" && strings.HasPrefix(call.Ref, "zenchron/")
		})
		workspace, err := f.runtime.workspace(f.state(runID))
		if err != nil {
			t.Fatal(err)
		}
		if err := workspace.FetchBase("origin"); err != nil {
			t.Fatalf("a runtime could not authorize its own base fetch: %v", err)
		}
	}

	drive(a)
	if len(credentialA.resolved) == 0 {
		t.Fatal("runtime A's Git operations never resolved the credential it was constructed with")
	}
	if len(credentialB.resolved) != 0 {
		t.Fatalf("runtime A's Git operations resolved runtime B's credential: %v", credentialB.resolved)
	}

	resolvedByA := len(credentialA.resolved)
	drive(b)
	if len(credentialB.resolved) == 0 {
		t.Fatal("runtime B's Git operations never resolved the credential it was constructed with")
	}
	if len(credentialA.resolved) != resolvedByA {
		t.Fatalf("runtime B's Git operations resolved runtime A's credential: %v", credentialA.resolved[resolvedByA:])
	}

	// Each authority was only ever asked about the repository it governs.
	for _, resolution := range credentialA.resolved {
		if resolution != a.deps.Remote.URL {
			t.Fatalf("runtime A's credential was resolved for an ungoverned remote %q", resolution)
		}
	}
	for _, resolution := range credentialB.resolved {
		if resolution != b.deps.Remote.URL {
			t.Fatalf("runtime B's credential was resolved for an ungoverned remote %q", resolution)
		}
	}
}

// TestCredentialSeamNamesANoexecTempDirectory is the regression for the defect
// the #29 adoption assurance surfaced: GIT_ASKPASS is a program, and a host
// that mounts its temp filesystem noexec cannot run it. Git reports that as
// "could not read Username", which points at the credential - the one thing
// that is not wrong. The runtime must name the filesystem instead.
func TestCredentialSeamNamesANoexecTempDirectory(t *testing.T) {
	// A noexec mount is a filesystem flag: it cannot be created portably in a
	// test, and root does not bypass it either. Substituting the exec is the
	// only way to reach the branch, so that is what this does.
	original := runExecProbe
	runExecProbe = func(string) error { return fmt.Errorf("permission denied") }
	defer func() { runExecProbe = original }()

	err := assertTempExecutable(t.TempDir())
	if err == nil {
		t.Fatal("a temp directory that cannot run a program must be refused")
	}
	// The refusal must be about execution and must name the directory an
	// operator can change, not about a credential.
	for _, want := range []string{"does not permit execution", "TMPDIR", os.TempDir()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "credential unavailable") {
		t.Fatalf("an execution failure must not be reported as a credential failure: %v", err)
	}
}
