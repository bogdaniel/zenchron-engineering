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
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	// BrowserAcceptanceClaimID is the one claim id the runtime recognizes for
	// this check. A policy that lists it in required_claims (evidence class
	// automated_test) is what turns the check on for a contract.
	BrowserAcceptanceClaimID = "claim-browser-acceptance"

	browserCheckerID        = "browser-acceptance-playwright"
	browserProviderID       = "baseline-go-browser"
	browserPackage          = "./cmd/zenchron-engineering"
	browserPlaywrightPath   = "/opt/zenchron-browser/playwright/index.mjs"
	browserChromePath       = "/opt/zenchron-browser/chrome/chrome"
	browserPrerequisiteExit = 3
)

// browserSelection is the closed set of tests that drive a real browser through
// a real, isolated controller. It is owned by the runtime, so a candidate cannot
// narrow what counts; the digest of it is part of every record.
var browserSelection = []string{
	"TestBrowserWorkspace",
	"TestDashboardDescribedObjective",
	"TestDashboardFeatureWalkthrough",
	"TestReactEngineeringOSGovernedWorkspace",
}

// browserSelectionDigest identifies the selection and the way it is run.
func browserSelectionDigest() string {
	names := append([]string(nil), browserSelection...)
	sort.Strings(names)
	sum := sha256.Sum256([]byte(browserPackage + "\x00" + strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])
}

// browserCheckerRevision identifies the implementation and the selection that
// produced a record, so a change to either invalidates earlier records.
func browserCheckerRevision() string {
	digest, _ := Digest(struct{ Name, Version, Selection string }{"browser-acceptance", "go-test-json-playwright-v1", browserSelectionDigest()})
	return digest
}

// browserAcceptanceCommand is the workload. Exit 3 means the image cannot run a
// browser at all, which is a prerequisite fault and never a verdict.
func browserAcceptanceCommand() string {
	return "test -f \"$ZENCHRON_PLAYWRIGHT_MODULE\" || exit 3\n" +
		"test -x \"$ZENCHRON_CHROME_EXECUTABLE\" || exit 3\n" +
		"command -v node >/dev/null || exit 3\n" +
		"go test -json -count=1 -run '^(" + strings.Join(browserSelection, "|") + ")$' " + browserPackage
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
func classifyBrowserRun(stdout []byte, exitCode int, ran bool) (browserOutcome, CheckResults) {
	results := CheckResults{Total: len(browserSelection)}
	selected := map[string]bool{}
	for _, name := range browserSelection {
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
