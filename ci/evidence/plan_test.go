package main

import (
	"os"
	"path/filepath"
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
			{Name: "controller", Why: "w", Race: true, Sources: []string{"runtime/controller*.go"}, Tests: []string{"runtime/controller_*_test.go", "runtime/shutdown_*_test.go"}},
		},
	}
	repo := Repo{
		TestsInFile: map[string][]string{
			"runtime/guard_test.go":              {"TestGuard"},
			"runtime/handoff_repair_test.go":     {"TestHandoffRepair"},
			"runtime/controller_crash_test.go":   {"TestControllerCrash"},
			"runtime/helpers_test.go":            {},
			"runtime/shutdown_semantics_test.go": {"TestShutdown"},
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
	if !slices.Equal(got.Race, []string{"TestControllerCrash", "TestShutdown"}) {
		t.Fatalf("race = %v", got.Race)
	}
}

func TestAChangedTestFileSelectsItsWholeDomainAndRacePolicy(t *testing.T) {
	// The file matches the domain's tests, not its sources.
	got := plan("runtime/shutdown_semantics_test.go")
	if got.Whole || !slices.Equal(got.Run, []string{"TestControllerCrash", "TestGuard", "TestShutdown"}) ||
		!slices.Equal(got.Race, []string{"TestControllerCrash", "TestShutdown"}) {
		t.Fatalf("plan = %+v", got)
	}
	// Deleted: the file no longer exists, but its domain still owns the path.
	if got := plan("runtime/shutdown_gone_test.go"); got.Whole || !slices.Contains(got.Race, "TestControllerCrash") {
		t.Fatalf("deleted test file: plan = %+v", got)
	}
	if got := plan("runtime/unowned_test.go"); !got.Whole {
		t.Fatalf("test file no domain owns: plan = %+v, want whole", got)
	}
}

func TestAWholeFallbackEscalatesEveryHighRiskDomain(t *testing.T) {
	reg, repo := fixture()
	reg.Whole = []string{"runtime/operations.go"}
	reg.Domains[1].Race = true // handoff: a second high-risk domain
	every := []string{"TestControllerCrash", "TestHandoffRepair", "TestShutdown"}
	for _, p := range []string{"runtime/operations.go", "runtime/brand_new.go", "domain/types.go"} {
		if got := PlanEvidence(reg, repo, []string{p}); !got.Whole || !slices.Equal(got.Race, every) {
			t.Errorf("%s: plan = %+v, want whole with every race domain", p, got)
		}
	}
	// A narrow mapped source keeps only its own race domain.
	got := PlanEvidence(reg, repo, []string{"runtime/controller_succession.go"})
	if got.Whole || !slices.Equal(got.Race, []string{"TestControllerCrash", "TestShutdown"}) {
		t.Fatalf("narrow controller change: race = %v", got.Race)
	}
}

func TestAHotspotRunsTheWholePackage(t *testing.T) {
	reg, repo := fixture()
	// The hotspot is also a controller source: being classified must not
	// narrow it back to one domain.
	reg.Whole = []string{"runtime/controller_operations.go"}
	if got := PlanEvidence(reg, repo, []string{"runtime/controller_operations.go"}); !got.Whole {
		t.Fatalf("plan = %+v, want whole", got)
	}
}

func TestTestFunctionsReadsDeclarationsNotText(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x_test.go")
	source := "package x\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {}\n\nfunc TestMultiline(\n\tt *testing.T,\n) {}\n\nfunc TestMain(m *testing.M) {}\n\nfunc helper(t *testing.T) {}\n\n// func TestInComment(t *testing.T) {}\n"
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := testFunctions(file)
	if err != nil || !slices.Equal(got, []string{"TestOne", "TestMultiline"}) {
		t.Fatalf("tests = %v, %v", got, err)
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
