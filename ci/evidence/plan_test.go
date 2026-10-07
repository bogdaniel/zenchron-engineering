package main

import (
	"slices"
	"strings"
	"testing"
)

func fixture() (Registry, Repo) {
	reg := Registry{
		Schema:         registrySchema,
		FocusedPackage: "runtime",
		Inert:          []string{"**/*.md", "docs/**"},
		Domains: []Domain{
			{Name: "guards", Why: "w", Always: true, Run: []string{"TestGuard"}},
			{Name: "handoff", Why: "w", Sources: []string{"runtime/handoff_*.go", "orchestration/handoff.go"}, Tests: []string{"runtime/*handoff*_test.go"}},
			{Name: "controller", Why: "w", Race: true, Sources: []string{"runtime/controller*.go"}, Tests: []string{"runtime/controller_*_test.go"}},
		},
	}
	repo := Repo{
		TestsInFile: map[string][]string{
			"runtime/guard_test.go":            {"TestGuard"},
			"runtime/handoff_repair_test.go":   {"TestHandoffRepair"},
			"runtime/controller_crash_test.go": {"TestControllerCrash"},
			"runtime/helpers_test.go":          {},
		},
		DepDirs:     []string{"orchestration", "domain"},
		PackageDirs: []string{"runtime", "orchestration", "domain", "cmd/zenchron-engineering"},
	}
	return reg, repo
}

func plan(changed ...string) Plan {
	reg, repo := fixture()
	return PlanEvidence(reg, repo, changed)
}

func TestAMappedSourceSelectsItsDomainAndExplainsWhy(t *testing.T) {
	got := plan("runtime/handoff_repair.go")
	if got.Whole || !slices.Equal(got.Run, []string{"TestGuard", "TestHandoffRepair"}) || len(got.Race) != 0 {
		t.Fatalf("plan = %+v", got)
	}
	if !slices.ContainsFunc(got.Reasons, func(r string) bool {
		return strings.HasPrefix(r, "handoff") && strings.Contains(r, "runtime/handoff_repair.go changed")
	}) {
		t.Fatalf("no reason names the change: %q", got.Reasons)
	}
}

func TestADependencySourceCanBeMappedToADomain(t *testing.T) {
	if got := plan("orchestration/handoff.go"); got.Whole || !slices.Contains(got.Run, "TestHandoffRepair") {
		t.Fatalf("plan = %+v", got)
	}
}

func TestUnclassifiedChangesFailSafeToTheWholePackage(t *testing.T) {
	for _, p := range []string{
		"runtime/brand_new.go",    // focused, unclassified
		"runtime/testdata/x.log",  // focused tree, not Go
		"domain/types.go",         // a package the focused one imports
		"go.mod",                  // outside every package, not inert
		"runtime/helpers_test.go", // shared helper with no tests of its own
	} {
		if got := plan(p); !got.Whole {
			t.Errorf("%s: plan = %+v, want whole", p, got)
		}
	}
}

func TestInertAndOtherPackageChangesSelectOnlyAlwaysEvidence(t *testing.T) {
	got := plan("docs/x.md", "runtime/README.md", "cmd/zenchron-engineering/main.go", "orchestration/handoff_test.go")
	if got.Whole || !slices.Equal(got.Run, []string{"TestGuard"}) {
		t.Fatalf("plan = %+v", got)
	}
}

func TestARaceDomainEscalatesOnlyItsOwnTests(t *testing.T) {
	got := plan("runtime/controller_succession.go", "runtime/handoff_slot.go")
	if !slices.Equal(got.Race, []string{"TestControllerCrash"}) {
		t.Fatalf("race = %v", got.Race)
	}
}

func TestAChangedTestFileRunsItsOwnTests(t *testing.T) {
	if got := plan("runtime/handoff_repair_test.go"); !slices.Contains(got.Run, "TestHandoffRepair") || got.Whole {
		t.Fatalf("plan = %+v", got)
	}
	if got := plan("runtime/deleted_test.go"); got.Whole || !slices.Equal(got.Run, []string{"TestGuard"}) {
		t.Fatalf("deleted test file: plan = %+v", got)
	}
}

func TestTheRunPatternIsAnchored(t *testing.T) {
	if got := RunPattern([]string{"TestA", "TestB"}); got != "^(TestA|TestB)$" {
		t.Fatalf("pattern = %q", got)
	}
}

func TestPatterns(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docsx/a", false},
		{"**/*.md", "runtime/README.md", true},
		{"runtime/*_test.go", "runtime/sub/a_test.go", false},
		{"runtime/handoff_*.go", "runtime/handoff_slot.go", true},
	} {
		if got := matches(c.pattern, c.path); got != c.want {
			t.Errorf("matches(%q, %q) = %v", c.pattern, c.path, got)
		}
	}
}
