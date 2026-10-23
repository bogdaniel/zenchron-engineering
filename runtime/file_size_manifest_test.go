package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// NEGATIVE PARITY. The same manifests are given to the real
// scripts/check_file_sizes.py and to the Go verifier. Every manifest the
// canonical checker refuses must be refused by Go too, with the manifest and the
// entry named, and every manifest it accepts must be accepted: a later entry for
// one file must never silently widen an approved limit.
func TestManifestsTheCanonicalCheckerRefusesAreRefusedByTheVerifierToo(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	script, err := os.ReadFile(filepath.Join("..", "scripts", "check_file_sizes.py"))
	if err != nil {
		t.Fatal(err)
	}
	// exc.go (1300 lines) is within the 1400 exception below; base.go (1100) is
	// within its 1100 ceiling. So the ONLY way either checker refuses is the
	// manifest itself.
	const goodBaseline, goodExceptions = "base.go\t1100\n", "exc.go\t1400\treason\n"
	cases := []struct {
		name, baseline, exceptions string
		refused                    bool
		mention                    string
	}{
		{"valid manifests", goodBaseline, goodExceptions, false, ""},
		{"valid in both manifests", "base.go\t1100\nexc.go\t1300\n", goodExceptions, false, ""},
		{"comments and blanks", "# note\n\n" + goodBaseline, "\n# note\n" + goodExceptions, false, ""},
		{"duplicate exception path (1400 then 5000)", goodBaseline, "exc.go\t1400\ta\nexc.go\t5000\tb\n", true, "duplicate"},
		{"duplicate baseline path", "base.go\t1100\nbase.go\t9000\n", goodExceptions, true, "duplicate"},
		{"absolute path", goodBaseline, "/exc.go\t1400\treason\n", true, "repository-relative"},
		{"parent component", goodBaseline, "../exc.go\t1400\treason\n", true, "repository-relative"},
		{"dot component", goodBaseline, "./exc.go\t1400\treason\n", true, "repository-relative"},
		{"doubled slash", goodBaseline, "sub//exc.go\t1400\treason\n", true, "repository-relative"},
		{"trailing slash", goodBaseline, "exc.go/\t1400\treason\n", true, "repository-relative"},
		{"too few columns", goodBaseline, "exc.go\t1400\n", true, "malformed"},
		{"too many columns", "base.go\t1100\textra\n", goodExceptions, true, "malformed"},
		{"empty field", goodBaseline, "exc.go\t\treason\n", true, "malformed"},
		{"limit at the cap", goodBaseline, "exc.go\t1000\treason\n", true, "above 1000"},
		{"limit not an integer", goodBaseline, "exc.go\t14oo\treason\n", true, "above 1000"},
		{"limit with a leading zero", goodBaseline, "exc.go\t01400\treason\n", true, "above 1000"},
		{"limit with a sign", goodBaseline, "exc.go\t+1400\treason\n", true, "above 1000"},
		{"exception not above the baseline ceiling", "exc.go\t1400\n", goodExceptions, true, "above its ceiling"},
	}
	for _, test := range cases {
		dir := t.TempDir()
		base, tree := sizeRepo(t, dir, map[string]string{
			"scripts/check_file_sizes.py": string(script),
			"file-size-baseline.tsv":      test.baseline,
			"file-size-exceptions.tsv":    test.exceptions,
			"exc.go":                      manyLines(1300),
			"base.go":                     manyLines(1100),
		}, "fixture")
		command := exec.Command(python, "scripts/check_file_sizes.py")
		command.Dir = dir
		var stderr bytes.Buffer
		command.Stderr = &stderr
		pythonErr := command.Run()
		pythonRefused := pythonErr != nil && strings.Contains(stderr.String(), "FAIL file-size check")

		violations, goErr := CheckFileSizePolicy(dir, base, tree)
		if pythonRefused != test.refused {
			t.Errorf("%s: the fixture itself is wrong, python refused=%v want %v (%s)", test.name, pythonRefused, test.refused, stderr.String())
			continue
		}
		if test.refused {
			if goErr == nil {
				t.Errorf("%s: the canonical checker refused the manifest and the Go verifier accepted it (%+v)", test.name, violations)
				continue
			}
			if !strings.Contains(goErr.Error(), test.mention) || !strings.Contains(goErr.Error(), "file-size-") {
				t.Errorf("%s: the refusal does not name the manifest and the problem: %v", test.name, goErr)
			}
			continue
		}
		if goErr != nil || len(violations) != 0 {
			t.Errorf("%s: the canonical checker accepted the manifests and Go refused or flagged them: %v %+v", test.name, goErr, violations)
		}
	}
}

func TestADuplicateEntryNamesItsPathAndManifest(t *testing.T) {
	_, err := parseFileSizeManifest(fileSizeExceptionFile, "a.go\t1200\tfirst\nb.go\t1300\tother\na.go\t5000\tlater\n", 3)
	if err == nil || !strings.Contains(err.Error(), `duplicate file-size-exceptions.tsv entry: "a.go"`) {
		t.Fatalf("a duplicate entry was not refused with its path and manifest: %v", err)
	}
	limits, err := parseFileSizeManifest(fileSizeBaselineFile, "a.go\t1200\r\nb.go\t1300\r\n", 2)
	if err != nil || limits["a.go"] != 1200 || limits["b.go"] != 1300 {
		t.Fatalf("a CRLF manifest, which the checker reads, was refused: %v %v", limits, err)
	}
	if _, err := parseFileSizeManifest(fileSizeBaselineFile, "a.go\t1200\n\xff\xfe\n", 2); err == nil {
		t.Fatal("a manifest that is not valid UTF-8 was accepted")
	}
}

// A release that changes the checker makes every earlier record stale, so the
// operation binding must change with it or the candidate is stranded; and it
// must change nothing else, spend the ordinary attempts, and stay stable when
// the checker did not change.
func TestACheckerRevisionChangeMakesVerificationEligibleWithoutRenewingBudgets(t *testing.T) {
	listing := domain.EngineeringPolicy{Rules: map[string]domain.PolicyRule{"r": {Effect: domain.PolicyEffect{
		RequiredClaims: &map[string]domain.RequiredClaim{FileSizePolicyClaimID: {EvidenceClass: AssuranceEvidenceClass}},
	}}}}
	required := func() (domain.EngineeringWorkContract, error) { return dischargeContract(), nil }
	suffix := func(revision string) string {
		return fileSizeBindingSuffixFor(listing, revision, dischargeBase, required)
	}
	const (
		oldChecker = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newChecker = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	if suffix(oldChecker) != suffix(oldChecker) || suffix(oldChecker) == "" {
		t.Fatal("an unchanged checker revision does not keep the binding stable")
	}
	if suffix(oldChecker) == suffix(newChecker) {
		t.Fatal("a changed checker revision does not change the binding, so the newer checker would never run")
	}
	if !strings.HasSuffix(suffix(newChecker), "@"+dischargeBase) {
		t.Fatalf("the governing base left the binding: %q", suffix(newChecker))
	}

	// Only the suffix differs: the candidate and the contract are unchanged.
	prefix := dischargeCommit + "|" + dischargeTree + "|" + dischargeContract().Revision
	before, after := prefix+suffix(oldChecker), prefix+suffix(newChecker)
	if !strings.HasPrefix(after, prefix) || before == after {
		t.Fatalf("the rebinding touched more than the file-size suffix: %q -> %q", before, after)
	}

	// Attempts are counted per operation KIND, so the new binding spends the
	// same assurance allowance and cannot renew it.
	var events []EngineeringEvent
	for index, key := range []string{before, after} {
		raw, err := json.Marshal(RunOperation{SchemaVersion: "0.1", ID: "op", RunID: "run-d", Kind: OpAssuranceGo, IdempotencyKey: key, Attempt: index + 1})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, EngineeringEvent{SchemaVersion: "0.1", ID: "e" + string(rune('a'+index)), RunID: "run-d", Sequence: int64(index + 1), Type: EventOperationBefore, Payload: raw})
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Attempts[OpAssuranceGo]; got != 2 {
		t.Fatalf("assurance attempts under two bindings = %d, want 2 (the allowance is per kind, not per binding)", got)
	}
}
