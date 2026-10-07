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
		if !slices.ContainsFunc(reg.Domains, func(d Domain) bool { return matchesAny(d.Sources, f) }) {
			t.Errorf("%s is not classified by any domain's sources", f)
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
