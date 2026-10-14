package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Conformance of the real registry against the real repository. These run in
// T1 on every pull request, so a deleted rule, a renamed file or a new
// unclassified source is caught where it is introduced.

func loadReal(t *testing.T) (Registry, Repo, []string) {
	t.Helper()
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	root := "../.."
	repo, err := ScanRepo(root, reg.FocusedPackage)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	return reg, repo, strings.Fields(string(listed))
}

func TestEveryFocusedSourceIsClassified(t *testing.T) {
	reg, _, files := loadReal(t)
	for _, f := range files {
		if !strings.HasPrefix(f, reg.FocusedPackage+"/") || strings.Count(f, "/") != 1 ||
			!strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		if !matchesAny(reg.Whole, f) && !slices.ContainsFunc(reg.Domains, func(d Domain) bool { return matchesAny(d.Sources, f) }) {
			t.Errorf("%s is not classified by any domain's sources or as a hotspot", f)
		}
	}
}

func TestEveryFocusedTestIsReachableFromADomain(t *testing.T) {
	reg, repo, _ := loadReal(t)
	for file := range repo.TestsInFile {
		if !slices.ContainsFunc(reg.Domains, func(d Domain) bool { return matchesAny(d.Tests, file) }) {
			t.Errorf("%s is not in any domain's tests", file)
		}
	}
}

func TestNoRegistryRuleIsDead(t *testing.T) {
	reg, repo, files := loadReal(t)
	known := map[string]bool{}
	for _, tests := range repo.TestsInFile {
		for _, name := range tests {
			known[name] = true
		}
	}
	for _, p := range reg.Whole {
		if !anyFile(files, p) {
			t.Errorf("hotspot %q matches no file", p)
		}
	}
	for _, d := range reg.Domains {
		for _, p := range append(append([]string{}, d.Sources...), d.Tests...) {
			if !anyFile(files, p) {
				t.Errorf("domain %s: %q matches no file", d.Name, p)
			}
		}
		for _, name := range d.Run {
			if !known[name] {
				t.Errorf("domain %s: test %s does not exist", d.Name, name)
			}
		}
		if len(domainTests(d, repo)) == 0 {
			t.Errorf("domain %s selects no tests", d.Name)
		}
	}
}

func anyFile(files []string, pattern string) bool {
	for _, f := range files {
		if matches(pattern, f) {
			return true
		}
	}
	return false
}

func TestOperatorReceiptsSelectDurableStoreEvidenceAndRace(t *testing.T) {
	reg, repo, _ := loadReal(t)
	for _, file := range []string{"runtime/operator_action_receipt.go", "runtime/operator_action_receipt_test.go"} {
		got := PlanEvidence(reg, repo, []string{file})
		if got.Whole || !slices.Contains(got.Run, "TestOperatorActionReceiptIsObservationalDurableAndClosed") ||
			!slices.Contains(got.Race, "TestOperatorActionReceiptIsObservationalDurableAndClosed") ||
			!slices.Contains(got.Race, "TestJournalRefusesPayloadThatDoesNotMatchItsEventType") {
			t.Fatalf("%s lost durable-store/receipt race coverage: %+v", file, got)
		}
	}
}

func TestActiveOperatorSurfacesSelectBoundaryEvidenceNotInert(t *testing.T) {
	reg, repo, _ := loadReal(t)
	for _, file := range []string{
		"frontend/src/governed-action.tsx", "frontend/package-lock.json", "frontend/build.mjs",
		"controlplane/assets/design/react.js", "controlplane/assets/design/tokens.css",
		"controlplane/assets/design/index.html", "controlplane/assets/design/fixtures.js",
		"schemas/control-plane-operator-action-receipt.schema.json",
	} {
		got := PlanEvidence(reg, repo, []string{file})
		if matchesAny(reg.Inert, file) || got.Whole ||
			!slices.Contains(got.Race, "TestPrivilegedControlRequiresProofIdentityAndLease") ||
			!slices.Contains(got.Race, "TestSendControlSeparatesALostReplyFromNothingSent") ||
			!slices.Contains(got.Race, "TestOperatorActionReceiptIsObservationalDurableAndClosed") ||
			!slices.Contains(got.Race, "TestPlanSummariesPageBoundsReadsAndPreservesLifecycleOwner") {
			t.Fatalf("%s did not select actual observation/authority/receipt evidence: %+v", file, got)
		}
	}
	// The shared validator owns every domain's contract, not just browser DTOs.
	// Keep its broad race escalation despite this PR adding only API registrations.
	got := PlanEvidence(reg, repo, []string{"schemas/validate.go"})
	if !got.Whole || !slices.Contains(got.Race, "TestReviewTriggerSurvivesSupervisorRestart") {
		t.Fatalf("cross-domain schema registry lost whole-package race escalation: %+v", got)
	}
}

func TestRaceShardsCoverEverySelectedTestExactlyOnce(t *testing.T) {
	reg, repo, _ := loadReal(t)
	for _, changes := range [][]string{{"runtime/operator_action_receipt.go"}, {"schemas/validate.go"}} {
		all := PlanEvidence(reg, repo, changes).Race
		seen := map[string]int{}
		for _, shard := range []string{"1/4", "2/4", "3/4", "4/4"} {
			names, err := shardRaceTests(all, shard)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				seen[name]++
			}
		}
		if len(seen) != len(all) || seen["TestOperatorActionReceiptIsObservationalDurableAndClosed"] != 1 {
			t.Fatal("partition dropped selected race evidence")
		}
		for _, name := range all {
			if seen[name] != 1 {
				t.Fatalf("partition omitted or duplicated %s", name)
			}
		}
		unsharded, err := shardRaceTests(all, "")
		if err != nil || !slices.Equal(unsharded, all) {
			t.Fatal("unsharded command changed its evidence")
		}
	}
}

func TestRaceShardRefusesInvalidSelectorsAndAllowsEmptyEvidence(t *testing.T) {
	for _, invalid := range []string{"0/4", "5/4", "1/0", "1/-1", "1", "1/2/3", "bad", "9999999999999999999999/4"} {
		if _, err := shardRaceTests([]string{"TestOne"}, invalid); err == nil {
			t.Fatalf("accepted invalid shard %q", invalid)
		}
	}
	if names, err := shardRaceTests(nil, "1/4"); err != nil || len(names) != 0 {
		t.Fatalf("empty race selection invented evidence: %v %v", names, err)
	}
}
