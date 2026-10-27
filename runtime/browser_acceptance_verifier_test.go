package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

// recordingBrowserDocker is a scripted Docker that also remembers the container
// it was asked to create and, on the browser run, lets a test act as the
// container writing into the artifact mount.
type recordingBrowserDocker struct {
	*scriptedDocker
	creates [][]string
	starts  int
	// onBrowserStart runs as the browser container, given the host directory the
	// runtime mounted at /artifacts.
	onBrowserStart func(artifacts string)
}

func (d *recordingBrowserDocker) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	return d.Output(ctx, name, args, dir, env, grace)
}

func (d *recordingBrowserDocker) Output(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	if len(args) > 0 && args[0] == "create" {
		d.creates = append(d.creates, append([]string(nil), args...))
	}
	if len(args) > 0 && args[0] == "start" {
		d.starts++
		if d.starts == 3 && d.onBrowserStart != nil && len(d.creates) > 0 {
			for i, arg := range d.creates[len(d.creates)-1] {
				if arg == "--mount" && strings.Contains(d.creates[len(d.creates)-1][i+1], "dst="+browserArtifactsPath) {
					src := strings.TrimPrefix(strings.Split(d.creates[len(d.creates)-1][i+1], ",")[1], "src=")
					d.onBrowserStart(src)
				}
			}
		}
	}
	return d.scriptedDocker.Output(ctx, name, args, dir, env, grace)
}

func writeArtifactsForEveryTest(artifacts string) {
	for _, name := range browserSelection {
		_ = os.MkdirAll(filepath.Join(artifacts, name), 0700)
		_ = os.WriteFile(filepath.Join(artifacts, name, "screenshot.png"), []byte("png "+name), 0600)
	}
}

func browserRun(t *testing.T, docker *recordingBrowserDocker, withClaim bool) (AssuranceResult, error, *recordingBrowserDocker) {
	t.Helper()
	dir, base, commit, tree := browserRepo(t)
	verifier := scriptedVerifier(t, docker.scriptedDocker, t.TempDir())
	verifier.Sandbox.Executor = docker
	request := AssuranceRequest{
		RunID: "run-1", Attempt: 1, Commit: commit, Tree: tree, CheckoutDir: dir,
		Contract: Ref{ID: "contract", Revision: "1"},
	}
	if withClaim {
		request.BrowserAcceptanceBase = base
	}
	result, err := verifier.Assure(context.Background(), request)
	return result, err, docker
}

func browserChecks(result AssuranceResult) []VerificationCheck {
	var checks []VerificationCheck
	for _, check := range result.Checks {
		if check.Kind == CheckKindBrowserAcceptance {
			checks = append(checks, check)
		}
	}
	return checks
}

func newBrowserDocker(starts map[int][]byte, exits map[int]int, onStart func(string)) *recordingBrowserDocker {
	return &recordingBrowserDocker{scriptedDocker: &scriptedDocker{startOutputs: starts, startExits: exits}, onBrowserStart: onStart}
}

func TestAPassingBrowserRunRecordsExactBoundEvidence(t *testing.T) {
	docker := newBrowserDocker(map[int][]byte{3: goTestJSON(allBrowserTests("pass"))}, nil, writeArtifactsForEveryTest)
	result, err, _ := browserRun(t, docker, true)
	if err != nil || !result.Passed {
		t.Fatalf("a fully passing browser run failed: passed=%v err=%v", result.Passed, err)
	}
	checks := browserChecks(result)
	if len(checks) != 1 || checks[0].Outcome != CheckOutcomePassed || checks[0].Results.Passed != len(browserSelection) {
		t.Fatalf("no passing record: %+v", result.Checks)
	}
	c := checks[0]
	if c.Commit == "" || c.Tree == "" || c.BaseRevision == "" || c.Selection != browserSelectionDigest(browserSelection) || c.Surface == "" || c.ArtifactsSHA256 == "" ||
		c.CheckerRevision != browserCheckerRevision() || !strings.HasPrefix(c.Environment, "docker-network-none@") || !strings.Contains(c.Artifact, browserProviderID) {
		t.Fatalf("the record is not bound to its candidate, base, tests, environment and artifacts: %+v", c)
	}
	if err := validateVerificationChecks(result.Checks); err != nil {
		t.Fatalf("the verifier wrote a record the journal would refuse: %v", err)
	}
}

// The container is handed what the tests actually need, and nothing that widens
// the existing boundary: both skip gates, the browser under both names the
// runners read, a per-test writable artifact path, a read-only candidate.
func TestTheBrowserContainerGetsTheEnvironmentAndArtifactMountTheTestsNeed(t *testing.T) {
	docker := newBrowserDocker(map[int][]byte{3: goTestJSON(allBrowserTests("pass"))}, nil, writeArtifactsForEveryTest)
	_, _, docker = browserRun(t, docker, true)
	if len(docker.creates) < 3 {
		t.Fatalf("expected preparation, suite and browser containers, saw %d", len(docker.creates))
	}
	create := strings.Join(docker.creates[len(docker.creates)-1], " ")
	for _, want := range []string{
		"ZENCHRON_BROWSER_E2E=1", "ZENCHRON_REACT_BROWSER_E2E=1",
		"ZENCHRON_PLAYWRIGHT_MODULE=" + browserPlaywrightPath, "ZENCHRON_CHROME_EXECUTABLE=" + browserChromePath, "ZENCHRON_E2E_CHROME=" + browserChromePath,
		"dst=" + browserArtifactsPath, "--network none", "--read-only", "--cap-drop ALL", "no-new-privileges",
		"ZENCHRON_E2E_ARTIFACTS=" + browserArtifactsPath + "/$name",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("the browser container lacks %q:\n%s", want, create)
		}
	}
	if !strings.Contains(create, "dst=/candidate,readonly") {
		t.Errorf("the candidate is not read-only in the browser container:\n%s", create)
	}
	if strings.Contains(create, "--privileged") || strings.Contains(create, "--cap-add") || strings.Contains(create, "--network host") {
		t.Errorf("the browser container widened the boundary:\n%s", create)
	}
}

func TestAMissingBrowserFailsClosed(t *testing.T) {
	docker := newBrowserDocker(nil, map[int]int{3: browserPrerequisiteExit}, writeArtifactsForEveryTest)
	result, err, _ := browserRun(t, docker, true)
	if err == nil || result.Passed || result.FailureClass != FailureAssurancePrerequisite || len(browserChecks(result)) != 0 {
		t.Fatalf("an image without Chrome or Playwright produced evidence: passed=%v class=%v err=%v checks=%+v", result.Passed, result.FailureClass, err, result.Checks)
	}
}

func TestASkippedOrSilentBrowserRunNeverPasses(t *testing.T) {
	skipped := allBrowserTests("pass")
	skipped[browserSelection[0]] = "skip"
	silent := allBrowserTests("pass")
	delete(silent, browserSelection[3])
	for name, output := range map[string][]byte{"a skipped test": goTestJSON(skipped), "a test that reported nothing": goTestJSON(silent), "no tests at all": []byte("ok\n")} {
		docker := newBrowserDocker(map[int][]byte{3: output}, nil, writeArtifactsForEveryTest)
		result, err, _ := browserRun(t, docker, true)
		if err == nil || result.Passed || len(browserChecks(result)) != 0 {
			t.Errorf("%s became evidence: passed=%v err=%v checks=%+v", name, result.Passed, err, result.Checks)
		}
	}
}

// Passing tests that left no artifacts did not demonstrably drive a browser.
func TestAPassingRunWithoutArtifactsIsNotEvidence(t *testing.T) {
	docker := newBrowserDocker(map[int][]byte{3: goTestJSON(allBrowserTests("pass"))}, nil, nil)
	result, err, _ := browserRun(t, docker, true)
	if err == nil || result.Passed || len(browserChecks(result)) != 0 || !strings.Contains(err.Error(), "no artifacts") {
		t.Fatalf("tests that wrote no artifacts became evidence: passed=%v err=%v checks=%+v", result.Passed, err, result.Checks)
	}
}

func TestAFailedBrowserRunBlocksTheCandidate(t *testing.T) {
	docker := newBrowserDocker(nil, map[int]int{3: 1}, writeArtifactsForEveryTest)
	result, err, _ := browserRun(t, docker, true)
	checks := browserChecks(result)
	if err != nil || result.Passed || result.FailureClass != FailureVerification || len(checks) != 1 || checks[0].Outcome != CheckOutcomeFailed {
		t.Fatalf("a failed browser run was not judged: passed=%v class=%v err=%v checks=%+v", result.Passed, result.FailureClass, err, result.Checks)
	}
	if result.ArtifactRef == "" || result.FailureSignature == "" {
		t.Fatalf("the failure carries no transcript for remediation: %+v", result)
	}
}

func TestBrowserAcceptanceIsNotRunWhenTheSuiteFailsOrIsNotRequired(t *testing.T) {
	failing := newBrowserDocker(nil, map[int]int{2: 1}, nil)
	result, _, docker := browserRun(t, failing, true)
	if result.Passed || len(browserChecks(result)) != 0 || docker.starts != 2 {
		t.Fatalf("a candidate whose suite failed was sent to the browser: starts=%d %+v", docker.starts, result.Checks)
	}
	goOnly := newBrowserDocker(nil, nil, nil)
	result, err, docker := browserRun(t, goOnly, false)
	if err != nil || !result.Passed || len(result.Checks) != 0 || docker.starts != 2 {
		t.Fatalf("work with no browser obligation changed behavior: starts=%d err=%v %+v", docker.starts, err, result.Checks)
	}
}

func TestARequiredBrowserCheckWithNoBaseRefusesVerification(t *testing.T) {
	if err := browserBaseRefusal(browserContract(), ""); err == nil {
		t.Fatal("a required browser check with no base revision was skipped")
	}
	if err := browserBaseRefusal(browserContract(), dischargeBase); err != nil {
		t.Fatal(err)
	}
	if err := browserBaseRefusal(dischargeContract(), ""); err != nil {
		t.Fatalf("a contract that does not require the check was refused: %v", err)
	}
}

// The skip gates are properties of the tests, so the environment the verifier
// sets is checked against the gates in the test sources themselves.
func TestEveryBrowserSkipGateIsSatisfiedByTheVerifierEnvironment(t *testing.T) {
	gate := regexp.MustCompile(`Getenv\("(ZENCHRON_[A-Z0-9_]+)"\)\s*!=\s*"1"`)
	set := map[string]bool{}
	for _, entry := range browserEnvironment {
		if name, value, ok := strings.Cut(entry, "="); ok && value == "1" {
			set[name] = true
		}
	}
	sources, err := filepath.Glob("../cmd/zenchron-engineering/*_test.go")
	if err != nil || len(sources) == 0 {
		t.Fatalf("no test sources found: %v", err)
	}
	found := 0
	for _, source := range sources {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "TestBrowserWorkspace") && !strings.Contains(string(data), "TestReactEngineeringOSGovernedWorkspace") && !strings.Contains(string(data), "TestDashboardFeatureWalkthrough") {
			continue
		}
		for _, match := range gate.FindAllStringSubmatch(string(data), -1) {
			found++
			if !set[match[1]] {
				t.Errorf("%s skips unless %s=1, which the browser container does not set", filepath.Base(source), match[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("found no skip gates; the scan is looking in the wrong place")
	}
}

// Chrome's own sandbox stays on everywhere except the network-less assurance
// container: the opt-in variable is set for the browser run only, not for the
// preparation or the Go suite, and the container's isolation is not widened to
// get there.
func TestTheNoSandboxOptInReachesOnlyTheBrowserContainer(t *testing.T) {
	docker := newBrowserDocker(map[int][]byte{3: goTestJSON(allBrowserTests("pass"))}, nil, writeArtifactsForEveryTest)
	_, _, docker = browserRun(t, docker, true)
	if len(docker.creates) != 3 {
		t.Fatalf("expected preparation, suite and browser containers, saw %d", len(docker.creates))
	}
	for index, create := range docker.creates {
		text := strings.Join(create, " ")
		has := strings.Contains(text, "ZENCHRON_E2E_NO_SANDBOX=1")
		if (index == 2) != has {
			t.Errorf("container %d: no-sandbox opt-in present=%v\n%s", index, has, text)
		}
	}
	browser := strings.Join(docker.creates[2], " ")
	for _, want := range []string{"--network none", "--read-only", "--cap-drop ALL", "no-new-privileges", "dst=/candidate,readonly"} {
		if !strings.Contains(browser, want) {
			t.Errorf("the browser container lost %q:\n%s", want, browser)
		}
	}
	for _, widened := range []string{"--privileged", "--cap-add", "seccomp=unconfined", "seccomp:unconfined", "--security-opt apparmor", "--userns", "--network host", "--pid host"} {
		if strings.Contains(browser, widened) {
			t.Errorf("the browser container widened the isolation with %q:\n%s", widened, browser)
		}
	}
}

// The raw Chrome runner starts Chrome with its own sandbox unless the opt-in is
// set. This runs the real script against a stand-in Chrome that records its
// arguments.
func TestTheRawChromeRunnerKeepsItsSandboxUnlessOptedIn(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	root := t.TempDir()
	recorded := filepath.Join(root, "argv")
	chrome := filepath.Join(root, "chrome")
	if err := os.WriteFile(chrome, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+recorded+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../scripts/browser_e2e.mjs")
	if err != nil {
		t.Fatal(err)
	}
	argv := func(extra ...string) string {
		_ = os.Remove(recorded)
		command := exec.Command(node, script)
		command.Env = append([]string{
			"PATH=" + os.Getenv("PATH"), "ZENCHRON_E2E_URL=http://127.0.0.1:1", "ZENCHRON_E2E_CHROME=" + chrome,
			"ZENCHRON_E2E_ARTIFACTS=" + filepath.Join(root, "artifacts"), "TMPDIR=" + root,
		}, extra...)
		_ = command.Run() // the stand-in exits at once; only its arguments matter
		data, err := os.ReadFile(recorded)
		if err != nil {
			t.Fatalf("the runner never started Chrome: %v", err)
		}
		return string(data)
	}
	if got := argv(); strings.Contains(got, "--no-sandbox") || !strings.Contains(got, "--headless=new") {
		t.Fatalf("Chrome lost its sandbox by default:\n%s", got)
	}
	if got := argv("ZENCHRON_E2E_NO_SANDBOX=0"); strings.Contains(got, "--no-sandbox") {
		t.Fatalf("a value other than 1 turned the sandbox off:\n%s", got)
	}
	if got := argv("ZENCHRON_E2E_NO_SANDBOX=1"); !strings.Contains(got, "--no-sandbox") {
		t.Fatalf("the opt-in did not reach Chrome:\n%s", got)
	}
}

// The browser step reads the assurance checkout only after proving it is the
// exact candidate; a checkout at any other commit is not read and starts no
// container.
func TestABrowserCheckoutThatIsNotTheExactCandidateIsNotRead(t *testing.T) {
	dir, base, commit, _ := browserRepo(t)
	docker := newBrowserDocker(nil, nil, nil)
	verifier := scriptedVerifier(t, docker.scriptedDocker, t.TempDir())
	verifier.Sandbox.Executor = docker
	result, err := verifier.withBrowserAcceptance(context.Background(), AssuranceRequest{
		RunID: "run-1", Attempt: 1, Commit: commit, Tree: "9999999999999999999999999999999999999999", CheckoutDir: dir,
		Contract: Ref{ID: "contract", Revision: "1"}, BrowserAcceptanceBase: base,
	}, AssuranceResult{Passed: true})
	if err == nil || result.Passed || result.FailureClass != FailureAssurancePrerequisite || len(docker.creates) != 0 || len(browserChecks(result)) != 0 {
		t.Fatalf("a checkout that is not the exact candidate was used: passed=%v class=%v err=%v creates=%d", result.Passed, result.FailureClass, err, len(docker.creates))
	}
}
