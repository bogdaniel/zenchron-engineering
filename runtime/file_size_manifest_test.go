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

// The canonical checker refuses a manifest entry that is a symlink, in either
// manifest (read_manifest's Path(name).is_symlink()). The verifier decides it
// from the pinned tree's own listing (mode 120000), never from a filesystem path,
// and must refuse the same manifests. This is not the stale-entry maintenance
// check.
func TestSymlinkedManifestEntriesAreRefusedLikeTheCanonicalChecker(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	script, err := os.ReadFile(filepath.Join("..", "scripts", "check_file_sizes.py"))
	if err != nil {
		t.Fatal(err)
	}
	const goodBaseline, goodExceptions = "base.go\t1100\n", "exc.go\t1400\treason\n"
	canonicalRefuses := func(dir string) bool {
		command := exec.Command(python, "scripts/check_file_sizes.py")
		command.Dir = dir
		var stderr bytes.Buffer
		command.Stderr = &stderr
		return command.Run() != nil && strings.Contains(stderr.String(), "FAIL file-size check")
	}
	fixture := func(baseline, exceptions string, symlinks ...string) (dir, base, tree string) {
		dir = t.TempDir()
		for _, name := range symlinks {
			if err := os.Symlink("exc.go", filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		base, tree = sizeRepo(t, dir, map[string]string{
			"scripts/check_file_sizes.py": string(script),
			"file-size-baseline.tsv":      baseline,
			"file-size-exceptions.tsv":    exceptions,
			"exc.go":                      manyLines(1300),
			"base.go":                     manyLines(1100),
		}, "fixture")
		return dir, base, tree
	}
	for _, test := range []struct {
		name, baseline, exceptions string
		symlinks                   []string
		refused                    bool
	}{
		{"regular file entries", goodBaseline, goodExceptions, nil, false},
		{"regular entries beside an unrelated symlink", goodBaseline, goodExceptions, []string{"unrelated.go"}, false},
		{"symlinked exception entry", goodBaseline, "link.go\t1400\treason\n", []string{"link.go"}, true},
		{"symlinked baseline entry", "link.go\t1100\n", goodExceptions, []string{"link.go"}, true},
	} {
		dir, base, tree := fixture(test.baseline, test.exceptions, test.symlinks...)
		if got := canonicalRefuses(dir); got != test.refused {
			t.Errorf("%s: the fixture is wrong, the canonical checker refused=%v want %v", test.name, got, test.refused)
			continue
		}
		violations, goErr := CheckFileSizePolicy(dir, base, tree)
		if test.refused {
			if goErr == nil || !strings.Contains(goErr.Error(), "entry is a symlink") || !strings.Contains(goErr.Error(), "file-size-") {
				t.Errorf("%s: the canonical checker refused and the verifier did not, or did not say why: %v %+v", test.name, goErr, violations)
			}
			continue
		}
		if goErr != nil || len(violations) != 0 {
			t.Errorf("%s: the canonical checker accepted and the verifier refused or flagged: %v %+v", test.name, goErr, violations)
		}
	}

	// The base's manifest names a regular file; the CANDIDATE replaces that file
	// with a symlink. Both checkers refuse the candidate's tree.
	dir, base, _ := fixture(goodBaseline, goodExceptions)
	if err := os.Remove(filepath.Join(dir, "exc.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("base.go", filepath.Join(dir, "exc.go")); err != nil {
		t.Fatal(err)
	}
	_, candidateTree := sizeRepo(t, dir, map[string]string{}, "the candidate swaps a named file for a symlink")
	if !canonicalRefuses(dir) {
		t.Fatal("the fixture is wrong: the canonical checker accepted the swapped tree")
	}
	if _, err := CheckFileSizePolicy(dir, base, candidateTree); err == nil || !strings.Contains(err.Error(), "entry is a symlink") {
		t.Fatalf("a candidate that turns a manifest-named file into a symlink was accepted: %v", err)
	}
}

// Two DISTINCT checker revisions that share a long prefix must still give
// different bindings: the claim compares the complete revision, so a binding
// that did not change would leave earlier evidence rejected and the candidate
// reading as already assured.
func TestRevisionsSharingALongPrefixStillChangeTheBinding(t *testing.T) {
	listing := domain.EngineeringPolicy{Rules: map[string]domain.PolicyRule{"r": {Effect: domain.PolicyEffect{
		RequiredClaims: &map[string]domain.RequiredClaim{FileSizePolicyClaimID: {EvidenceClass: AssuranceEvidenceClass}},
	}}}}
	required := func() (domain.EngineeringWorkContract, error) { return dischargeContract(), nil }
	first := strings.Repeat("a", 64)
	for _, shared := range []int{16, 32, 63} {
		second := first[:shared] + strings.Repeat("b", 64-shared)
		if first == second || first[:shared] != second[:shared] {
			t.Fatalf("the fixture does not share exactly %d characters", shared)
		}
		a := fileSizeBindingSuffixFor(listing, first, dischargeBase, required)
		b := fileSizeBindingSuffixFor(listing, second, dischargeBase, required)
		if a == "" || a == b {
			t.Errorf("two revisions sharing their first %d characters give the same binding: %q", shared, a)
		}
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
	// The base is part of the binding too, and the suffix has a fixed length.
	otherBase := "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"
	if fileSizeBindingSuffixFor(listing, newChecker, otherBase, required) == suffix(newChecker) {
		t.Fatal("a changed governing base does not change the binding")
	}
	if got := len(suffix(newChecker)); got != len("|file-size-policy@")+64 {
		t.Fatalf("the binding suffix is not a fixed-length digest: %d", got)
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
