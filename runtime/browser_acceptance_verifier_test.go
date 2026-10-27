package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recordingBrowserDocker is a scripted Docker that remembers every container it
// was asked to create.
type recordingBrowserDocker struct {
	*scriptedDocker
	creates [][]string
	starts  int
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
	}
	return d.scriptedDocker.Output(ctx, name, args, dir, env, grace)
}

// exportedTree is what the workload streams out for these files: the manifest of
// everything written, then the retained files as a tar.
func exportedTree(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var manifest bytes.Buffer
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		manifest.WriteString(hex.EncodeToString(sum[:]) + "  ./" + name + "\n")
		if err := writer.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return exportedSections(manifest.String()+browserManifestCount+" "+strconv.Itoa(len(names))+"\n", base64.StdEncoding.EncodeToString(archive.Bytes()))
}

// exportedSections frames a manifest and an encoded archive as the workload does.
func exportedSections(manifest, encoded string) []byte {
	var out bytes.Buffer
	out.WriteString("\n" + browserManifestBegin + "\n" + manifest + browserManifestEnd + "\n")
	out.WriteString(browserArtifactsBegin + "\n")
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded + "\n" + browserArtifactsEnd + "\n")
	return out.Bytes()
}
func screenshotsForEveryTest() map[string][]byte {
	files := map[string][]byte{}
	for _, name := range browserSelection {
		files[name+"/screenshot.png"] = []byte("png " + name)
	}
	return files
}

// browserStdout is what the workload prints: the test events, then the export.
func browserStdout(t *testing.T, actions map[string]string, files map[string][]byte) []byte {
	t.Helper()
	return append(goTestJSON(actions), exportedTree(t, files)...)
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
func newBrowserDocker(starts, failed map[int][]byte, exits map[int]int) *recordingBrowserDocker {
	return &recordingBrowserDocker{scriptedDocker: &scriptedDocker{startOutputs: starts, failedOutputs: failed, startExits: exits}}
}
func passingBrowserDocker(t *testing.T) *recordingBrowserDocker {
	return newBrowserDocker(map[int][]byte{3: browserStdout(t, allBrowserTests("pass"), screenshotsForEveryTest())}, nil, nil)
}
func TestAPassingBrowserRunRecordsExactBoundEvidence(t *testing.T) {
	result, err, _ := browserRun(t, passingBrowserDocker(t), true)
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
// runners read, a per-test artifact path on a bounded in-container tmpfs, and a
// read-only candidate. No host path is writable.
func TestTheBrowserContainerGetsTheEnvironmentAndBoundedArtifactSpaceTheTestsNeed(t *testing.T) {
	_, _, docker := browserRun(t, passingBrowserDocker(t), true)
	if len(docker.creates) < 3 {
		t.Fatalf("expected preparation, suite and browser containers, saw %d", len(docker.creates))
	}
	create := strings.Join(docker.creates[len(docker.creates)-1], " ")
	for _, want := range []string{
		"ZENCHRON_BROWSER_E2E=1", "ZENCHRON_REACT_BROWSER_E2E=1",
		"ZENCHRON_PLAYWRIGHT_MODULE=" + browserPlaywrightPath, "ZENCHRON_CHROME_EXECUTABLE=" + browserChromePath, "ZENCHRON_E2E_CHROME=" + browserChromePath,
		"--tmpfs " + browserArtifactsTmpfs, "--network none", "--read-only", "--cap-drop ALL", "no-new-privileges",
		"ZENCHRON_E2E_ARTIFACTS=" + browserArtifactsPath + "/$name",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("the browser container lacks %q:\n%s", want, create)
		}
	}
	if !strings.Contains(create, "dst=/candidate,readonly") {
		t.Errorf("the candidate is not read-only in the browser container:\n%s", create)
	}
	if strings.Contains(create, "dst="+browserArtifactsPath) || strings.Contains(create, "--privileged") || strings.Contains(create, "--cap-add") || strings.Contains(create, "--network host") {
		t.Errorf("the browser container gained a host mount or widened the boundary:\n%s", create)
	}
}
func TestAMissingBrowserFailsClosed(t *testing.T) {
	docker := newBrowserDocker(nil, nil, map[int]int{3: browserPrerequisiteExit})
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
	for name, output := range map[string][]byte{
		"a skipped test":               browserStdout(t, skipped, screenshotsForEveryTest()),
		"a test that reported nothing": browserStdout(t, silent, screenshotsForEveryTest()),
		"no tests at all":              append([]byte("ok\n"), exportedTree(t, screenshotsForEveryTest())...),
	} {
		result, err, _ := browserRun(t, newBrowserDocker(map[int][]byte{3: output}, nil, nil), true)
		if err == nil || result.Passed || len(browserChecks(result)) != 0 {
			t.Errorf("%s became evidence: passed=%v err=%v checks=%+v", name, result.Passed, err, result.Checks)
		}
	}
}

// Passing tests that left no artifacts did not demonstrably drive a browser.
func TestAPassingRunWithoutArtifactsIsNotEvidence(t *testing.T) {
	partial := screenshotsForEveryTest()
	delete(partial, browserSelection[1]+"/screenshot.png")
	for name, files := range map[string]map[string][]byte{"no files at all": {}, "one test with none": partial} {
		docker := newBrowserDocker(map[int][]byte{3: browserStdout(t, allBrowserTests("pass"), files)}, nil, nil)
		result, err, _ := browserRun(t, docker, true)
		if err == nil || result.Passed || len(browserChecks(result)) != 0 || !strings.Contains(err.Error(), "no artifacts") {
			t.Errorf("%s: tests that wrote no artifacts became evidence: passed=%v err=%v checks=%+v", name, result.Passed, err, result.Checks)
		}
	}
}
func TestAFailedBrowserRunBlocksTheCandidate(t *testing.T) {
	failing := allBrowserTests("pass")
	failing[browserSelection[2]] = "fail"
	docker := newBrowserDocker(nil, map[int][]byte{3: browserStdout(t, failing, screenshotsForEveryTest())}, map[int]int{3: 1})
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
	result, _, docker := browserRun(t, newBrowserDocker(nil, nil, map[int]int{2: 1}), true)
	if result.Passed || len(browserChecks(result)) != 0 || docker.starts != 2 {
		t.Fatalf("a candidate whose suite failed was sent to the browser: starts=%d %+v", docker.starts, result.Checks)
	}
	result, err, docker := browserRun(t, newBrowserDocker(nil, nil, nil), false)
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
	_, _, docker := browserRun(t, passingBrowserDocker(t), true)
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
	dir, base, commit, tree := browserRepo(t)
	// Every object the request names exists, but the checkout's HEAD is not the
	// exact candidate, so it is not the verified checkout.
	mustGit(t, dir, "checkout", "-q", "--detach", base)
	docker := newBrowserDocker(nil, nil, nil)
	verifier := scriptedVerifier(t, docker.scriptedDocker, t.TempDir())
	verifier.Sandbox.Executor = docker
	result, err := verifier.withBrowserAcceptance(context.Background(), AssuranceRequest{
		RunID: "run-1", Attempt: 1, Commit: commit, Tree: tree, CheckoutDir: dir,
		Contract: Ref{ID: "contract", Revision: "1"}, BrowserAcceptanceBase: base,
	}, AssuranceResult{Passed: true})
	if err == nil || result.Passed || result.FailureClass != FailureAssurancePrerequisite || len(docker.creates) != 0 || len(browserChecks(result)) != 0 {
		t.Fatalf("a checkout that is not the exact candidate was used: passed=%v class=%v err=%v creates=%d", result.Passed, result.FailureClass, err, len(docker.creates))
	}
}

// The page-state waits are bounded and still fail: a state that settles late
// passes, a state that never arrives fails after the bound with the value the
// page held, so waiting cannot hide a genuine failure.
func TestTheBrowserWaitsSettleLateStateAndStillFailWhenItNeverArrives(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	helper, err := filepath.Abs("../scripts/browser_waits.mjs")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "probe.mjs")
	body := `import { eventually, expectText } from ` + strconvQuote("file://"+helper) + `;
const page = { waitForTimeout: (ms) => new Promise((resolve) => setTimeout(resolve, ms)) };
let reads = 0;
await eventually(page, async () => ++reads >= 3, true, "late state", 2000);
const fail = async (run) => { const started = Date.now(); try { await run(); return "passed"; } catch (e) { return (Date.now() - started < 3000 ? "failed-bounded:" : "failed-slow:") + e.message.replace(/\s+/g, " ").slice(0, 120); } };
console.log("never", await fail(() => eventually(page, async () => false, true, "never arrives", 300)));
const stuck = { filter: () => ({ waitFor: async () => { throw new Error("timeout"); } }), innerText: async () => "Reading runtime state…" };
console.log("text", await fail(() => expectText(stuck, /walkthrough-product/, 300)));
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "never failed-bounded:") || !strings.Contains(text, "text failed-bounded:") || !strings.Contains(text, "Reading runtime state") {
		t.Fatalf("a wait hid a failure, ran unbounded or lost the page's own value:\n%s", text)
	}
}
func strconvQuote(s string) string { return strconv.Quote(s) }
