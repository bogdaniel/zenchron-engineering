package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// goTestJSON renders a `go test -json` stream in which each named test ended
// with the given action.
func goTestJSON(actions map[string]string) []byte {
	var out strings.Builder
	for _, name := range browserSelection {
		if action, ok := actions[name]; ok {
			fmt.Fprintf(&out, "{\"Action\":\"run\",\"Test\":%q}\n{\"Action\":%q,\"Test\":%q}\n", name, action, name)
		}
	}
	return []byte(out.String())
}

func allBrowserTests(action string) map[string]string {
	actions := map[string]string{}
	for _, name := range browserSelection {
		actions[name] = action
	}
	return actions
}

func TestOnlyACompleteRunOfEverySelectedTestIsBrowserEvidence(t *testing.T) {
	oneSkipped := allBrowserTests("pass")
	oneSkipped[browserSelection[1]] = "skip"
	oneFailed := allBrowserTests("pass")
	oneFailed[browserSelection[2]] = "fail"
	oneSilent := allBrowserTests("pass")
	delete(oneSilent, browserSelection[0])
	for name, tc := range map[string]struct {
		stdout   []byte
		exit     int
		ran      bool
		want     browserOutcome
		passed   int
		skipped  int
		failedTs int
	}{
		"every test passed":                {goTestJSON(allBrowserTests("pass")), 0, true, browserPassed, 4, 0, 0},
		"a test failed":                    {goTestJSON(oneFailed), 1, true, browserFailed, 3, 0, 1},
		"a test skipped, exit still zero":  {goTestJSON(oneSkipped), 0, true, browserInconclusive, 3, 1, 0},
		"every test skipped":               {goTestJSON(allBrowserTests("skip")), 0, true, browserInconclusive, 0, 4, 0},
		"a selected test never reported":   {goTestJSON(oneSilent), 0, true, browserInconclusive, 3, 0, 0},
		"no output at all":                 {nil, 0, true, browserInconclusive, 0, 0, 0},
		"the image has no browser":         {nil, browserPrerequisiteExit, true, browserInconclusive, 0, 0, 0},
		"a build failure is judged":        {[]byte("FAIL\n"), 2, true, browserFailed, 0, 0, 0},
		"a killed container is no verdict": {nil, 137, true, browserInconclusive, 0, 0, 0},
		"the sandbox could not run":        {nil, 0, false, browserInconclusive, 0, 0, 0},
		"passes but a nonzero exit":        {goTestJSON(allBrowserTests("pass")), 1, true, browserFailed, 4, 0, 0},
	} {
		outcome, results := classifyBrowserRun(tc.stdout, tc.exit, tc.ran)
		if outcome != tc.want || results.Passed != tc.passed || results.Skipped != tc.skipped || results.Failed != tc.failedTs {
			t.Errorf("%s: outcome=%d results=%+v", name, outcome, results)
		}
	}
}

func browserContract() domain.EngineeringWorkContract {
	contract := dischargeContract()
	delete(contract.RequiredClaims, FileSizePolicyClaimID)
	contract.RequiredClaims[BrowserAcceptanceClaimID] = domain.RequiredClaim{EvidenceClass: AssuranceEvidenceClass}
	contract.Obligations = map[string]domain.Requirement{
		"browser-acceptance": {Statement: "browser", RequiredClaims: []string{BrowserAcceptanceClaimID}, Material: true},
	}
	return contract
}

func goodBrowserCheck() VerificationCheck {
	return VerificationCheck{
		Kind: CheckKindBrowserAcceptance, Checker: browserCheckerID, CheckerRevision: browserCheckerRevision(),
		Outcome: CheckOutcomePassed, Commit: dischargeCommit, Tree: dischargeTree, Selection: browserSelectionDigest(),
		Results: &CheckResults{Total: 4, Passed: 4}, Environment: "docker-network-none@sha256:img", Artifact: "baseline-go-browser/attempt-1",
	}
}

func TestAGenericGreenAssuranceCannotDischargeTheBrowserClaim(t *testing.T) {
	contract := browserContract()
	claims := bundleClaimsFor(t, journalFor(t, nil), dischargeBase, contract)
	if _, discharged := claims[BrowserAcceptanceClaimID]; discharged {
		t.Fatal("a passing suite with no browser record discharged the browser claim")
	}
	if _, discharged := claims["claim-validation"]; !discharged {
		t.Fatal("the generic claim stopped being discharged by a generic suite")
	}
}

func TestOnlyACurrentCompletePassingBrowserRecordProvesTheClaim(t *testing.T) {
	contract := browserContract()
	mutate := func(change func(*VerificationCheck)) []VerificationCheck {
		check := goodBrowserCheck()
		change(&check)
		return []VerificationCheck{check}
	}
	if _, ok := bundleClaimsFor(t, journalFor(t, []VerificationCheck{goodBrowserCheck()}), dischargeBase, contract)[BrowserAcceptanceClaimID]; !ok {
		t.Fatal("a current, complete, passing browser record did not discharge the claim")
	}
	for name, checks := range map[string][]VerificationCheck{
		"a failed run":             mutate(func(c *VerificationCheck) { c.Outcome = CheckOutcomeFailed }),
		"another commit":           mutate(func(c *VerificationCheck) { c.Commit = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2" }),
		"another tree":             mutate(func(c *VerificationCheck) { c.Tree = "8888888888888888888888888888888888888888" }),
		"another checker":          mutate(func(c *VerificationCheck) { c.Checker = "other" }),
		"a stale checker revision": mutate(func(c *VerificationCheck) { c.CheckerRevision = "old" }),
		"another test selection":   mutate(func(c *VerificationCheck) { c.Selection = strings.Repeat("0", 64) }),
		"a skipped test":           mutate(func(c *VerificationCheck) { c.Results = &CheckResults{Total: 4, Passed: 3, Skipped: 1} }),
		"a failed test":            mutate(func(c *VerificationCheck) { c.Results = &CheckResults{Total: 4, Passed: 3, Failed: 1} }),
		"a test that never ran":    mutate(func(c *VerificationCheck) { c.Results = &CheckResults{Total: 4, Passed: 3} }),
		"no tests selected":        mutate(func(c *VerificationCheck) { c.Results = &CheckResults{} }),
		"no results":               mutate(func(c *VerificationCheck) { c.Results = nil }),
		"no environment":           mutate(func(c *VerificationCheck) { c.Environment = "" }),
		"no artifact":              mutate(func(c *VerificationCheck) { c.Artifact = "" }),
		"the file-size kind":       mutate(func(c *VerificationCheck) { c.Kind = CheckKindFileSizePolicy }),
	} {
		if _, ok := bundleClaimsFor(t, journalFor(t, checks), dischargeBase, contract)[BrowserAcceptanceClaimID]; ok {
			t.Errorf("%s discharged the browser claim", name)
		}
	}
}

func TestABrowserRecordMustBeCompleteToBeStored(t *testing.T) {
	if err := validateVerificationChecks([]VerificationCheck{goodBrowserCheck()}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*VerificationCheck){
		"no selection":   func(c *VerificationCheck) { c.Selection = "" },
		"no results":     func(c *VerificationCheck) { c.Results = nil },
		"no environment": func(c *VerificationCheck) { c.Environment = "" },
		"no artifact":    func(c *VerificationCheck) { c.Artifact = "" },
	} {
		check := goodBrowserCheck()
		change(&check)
		if validateVerificationChecks([]VerificationCheck{check}) == nil {
			t.Errorf("a browser record with %s was accepted", name)
		}
	}
}

func TestABrowserRequirementThatAppearsChangesTheAssuranceBinding(t *testing.T) {
	listing := domain.EngineeringPolicy{Rules: map[string]domain.PolicyRule{"r": {Effect: domain.PolicyEffect{
		RequiredClaims: &map[string]domain.RequiredClaim{BrowserAcceptanceClaimID: {EvidenceClass: AssuranceEvidenceClass}},
	}}}}
	rebuild := func(contract domain.EngineeringWorkContract) func() (domain.EngineeringWorkContract, error) {
		return func() (domain.EngineeringWorkContract, error) { return contract, nil }
	}
	want := "|browser-acceptance@" + browserCheckerRevision()
	if got := browserBindingSuffixFor(listing, rebuild(browserContract())); got != want {
		t.Fatalf("a newly required check does not change the binding: %q", got)
	}
	for name, got := range map[string]string{
		"a policy that never lists the claim": browserBindingSuffixFor(domain.EngineeringPolicy{}, rebuild(browserContract())),
		"a contract compiled without it":      browserBindingSuffixFor(listing, rebuild(dischargeContract())),
		"a run with no runtime attached":      (&runState{}).browserBindingSuffix(),
		"a contract that cannot be rebuilt": browserBindingSuffixFor(listing, func() (domain.EngineeringWorkContract, error) {
			return domain.EngineeringWorkContract{}, context.Canceled
		}),
	} {
		if got != "" {
			t.Errorf("%s changed the binding: %q", name, got)
		}
	}
}
