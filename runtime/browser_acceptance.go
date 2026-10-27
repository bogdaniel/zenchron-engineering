package runtime

// Browser acceptance as verified evidence.
//
// A passing Go suite does not prove the dashboard works in a real browser, so
// the claim BrowserAcceptanceClaimID is discharged only by a typed check record
// (verification_check.go) written when the verifier ran the repository's
// browser-driving tests against the exact candidate, in the same network-less
// sandbox as the suite, and every one of them passed.
//
// The browser and Playwright come from the operator-pinned assurance image at
// the fixed paths below. An image without them cannot pass: a missing
// prerequisite, a skipped test, a selected test that produced no result and an
// unreadable run are all inconclusive, and inconclusive is never passing
// evidence. A contract that does not require the claim is verified exactly as
// before.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	// BrowserAcceptanceClaimID is the one claim id the runtime recognizes for
	// this check. A policy that lists it in required_claims (evidence class
	// automated_test) is what turns the check on for a contract.
	BrowserAcceptanceClaimID = "claim-browser-acceptance"

	browserCheckerID  = "browser-acceptance-playwright"
	browserProviderID = "baseline-go-browser"
	browserPackage    = "./cmd/zenchron-engineering"
	// browserManifestFile lists the selected tests. It is read from the BASE
	// revision (see browserWorkspace), like the file-size manifests.
	browserManifestFile     = "browser-acceptance-tests.txt"
	browserPlaywrightPath   = "/opt/zenchron-browser/playwright/index.mjs"
	browserChromePath       = "/opt/zenchron-browser/chrome/chrome"
	browserArtifactsPath    = "/artifacts"
	browserPrerequisiteExit = 3
	maxBrowserSelection     = 16
)

var browserTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]{1,80}$`)

// browserEnvironment is everything the selected tests need to run instead of
// skipping: the two skip gates, the browser and the Playwright module. Which
// variable a test reads is a property of the test, so a source-scan test keeps
// this list in step with the gates in the tests themselves.
var browserEnvironment = []string{
	"ZENCHRON_BROWSER_E2E=1",
	"ZENCHRON_REACT_BROWSER_E2E=1",
	"ZENCHRON_PLAYWRIGHT_MODULE=" + browserPlaywrightPath,
	"ZENCHRON_CHROME_EXECUTABLE=" + browserChromePath,
	"ZENCHRON_E2E_CHROME=" + browserChromePath,
	// The only place the raw-Chrome runner may start Chrome without its own
	// sandbox: this container, whose isolation is the boundary.
	"ZENCHRON_E2E_NO_SANDBOX=1",
}

// readBrowserSelection reads the selected test names from the base revision. A
// manifest that is missing, empty, oversized or holds anything but a plain test
// name refuses verification: a required check that cannot be defined is not
// skipped, and a name is never interpolated into a command unvalidated.
func readBrowserSelection(dir, base string) ([]string, error) {
	text, err := gitOutput(dir, "show", base+":"+browserManifestFile)
	if err != nil {
		return nil, fmt.Errorf("the base revision has no %s: %w", browserManifestFile, err)
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !browserTestName.MatchString(line) || seen[line] {
			return nil, fmt.Errorf("%s names %q, which is not a unique test name", browserManifestFile, line)
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(names) == 0 || len(names) > maxBrowserSelection {
		return nil, fmt.Errorf("%s names %d tests, outside 1..%d", browserManifestFile, len(names), maxBrowserSelection)
	}
	sort.Strings(names)
	return names, nil
}

// browserSelectionDigest identifies the selection and the way it is run.
func browserSelectionDigest(names []string) string {
	sum := sha256.Sum256([]byte(browserPackage + "\x00" + strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])
}

// browserCheckerRevision identifies the implementation that produced a record,
// so a change to how the run is made invalidates earlier records.
func browserCheckerRevision() string {
	digest, _ := Digest(struct{ Name, Version string }{"browser-acceptance", "go-test-json-playwright-v3-base-pinned-tests-container-no-sandbox"})
	return digest
}

// browserAcceptanceCommand is the workload: each selected test runs alone,
// writing its artifacts into its own writable directory, so a test that wrote
// nothing is visible. Exit 3 means the image cannot run a browser at all, which
// is a prerequisite fault and never a verdict.
func browserAcceptanceCommand(names []string) string {
	return "test -f \"$ZENCHRON_PLAYWRIGHT_MODULE\" || exit 3\n" +
		"test -x \"$ZENCHRON_CHROME_EXECUTABLE\" || exit 3\n" +
		"command -v node >/dev/null || exit 3\n" +
		"status=0\nfor name in " + strings.Join(names, " ") + "; do\n" +
		"  mkdir -p " + browserArtifactsPath + "/$name\n" +
		"  ZENCHRON_E2E_ARTIFACTS=" + browserArtifactsPath + "/$name go test -json -count=1 -run \"^$name\\$\" " + browserPackage + " || status=1\n" +
		"done\nexit $status"
}

// browserOutcome is how a browser run ended.
type browserOutcome int

const (
	// browserPassed: every selected test ran and passed.
	browserPassed browserOutcome = iota
	// browserFailed: the candidate was judged, and a test or its build failed.
	browserFailed
	// browserInconclusive: nothing can be concluded about the candidate - the
	// image cannot run a browser, a test skipped or never reported, or the run
	// ended some way that is not a verdict.
	browserInconclusive
)

// classifyBrowserRun reads a `go test -json` stream and the workload's exit
// code. Passing requires a pass event for every selected test and nothing else;
// only exit codes 1 and 2 (the Go tool's own failure codes) are a judgment of
// the candidate, so a missing browser (exit 3), a killed container or anything
// unrecognized stays inconclusive.
func classifyBrowserRun(selection []string, stdout []byte, exitCode int, ran bool) (browserOutcome, CheckResults) {
	results := CheckResults{Total: len(selection)}
	selected := map[string]bool{}
	for _, name := range selection {
		selected[name] = true
	}
	final := map[string]string{}
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var event struct{ Action, Test string }
		if json.Unmarshal(bytes.TrimSpace(line), &event) != nil || !selected[event.Test] {
			continue
		}
		switch event.Action {
		case "pass", "fail", "skip":
			final[event.Test] = event.Action
		}
	}
	for _, action := range final {
		switch action {
		case "pass":
			results.Passed++
		case "fail":
			results.Failed++
		case "skip":
			results.Skipped++
		}
	}
	switch {
	case !ran:
		return browserInconclusive, results
	case exitCode == 0 && results.Passed == results.Total:
		return browserPassed, results
	case exitCode == 0 || results.Skipped > 0:
		return browserInconclusive, results
	case exitCode == 1 || exitCode == 2:
		return browserFailed, results
	}
	return browserInconclusive, results
}

// browserAcceptanceRequired reports whether the contract's compiled required
// claims include the browser claim.
func browserAcceptanceRequired(contract domain.EngineeringWorkContract) bool {
	_, required := contract.RequiredClaims[BrowserAcceptanceClaimID]
	return required
}
