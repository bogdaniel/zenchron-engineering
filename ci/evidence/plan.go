package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// The impact registry is the ONE authoritative mapping from changed
// engineering surfaces to the T1 evidence a pull request must produce. The
// workflow runs this program and holds no path list of its own, so there is
// nowhere else a mapping could drift to.

//go:embed registry.json
var registryJSON []byte

const registrySchema = "zenchron-evidence-registry/1"

type Registry struct {
	Schema string `json:"schema"`
	// FocusedPackage is the one package too expensive to run whole on every
	// pull request. Every other package always runs whole in T1.
	FocusedPackage string   `json:"focused_package"`
	Inert          []string `json:"inert"`
	Domains        []Domain `json:"domains"`
}

// Domain is an invariant/evidence domain: the sources whose change requires
// its evidence, and the tests that ARE that evidence.
type Domain struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	// Always selects the domain on every pull request (source-scan guards).
	Always bool `json:"always"`
	// Race escalates the domain: its tests also run under -race.
	Race    bool     `json:"race"`
	Sources []string `json:"sources"`
	// Tests are test-file globs; every Test function in a matching file runs.
	Tests []string `json:"tests"`
	// Run names individual tests, for domains that are a few tests out of
	// files that belong to other domains.
	Run []string `json:"run"`
}

func LoadRegistry() (Registry, error) {
	var reg Registry
	if err := json.Unmarshal(registryJSON, &reg); err != nil {
		return reg, fmt.Errorf("the evidence registry is not valid JSON: %w", err)
	}
	return reg, reg.validate()
}

func (r Registry) validate() error {
	if r.Schema != registrySchema {
		return fmt.Errorf("registry schema is %q, want %q", r.Schema, registrySchema)
	}
	if r.FocusedPackage == "" {
		return fmt.Errorf("registry names no focused package")
	}
	patterns := append([]string{}, r.Inert...)
	seen := map[string]bool{}
	for _, d := range r.Domains {
		if d.Name == "" || seen[d.Name] {
			return fmt.Errorf("domain name %q is empty or duplicated", d.Name)
		}
		seen[d.Name] = true
		if strings.TrimSpace(d.Why) == "" {
			return fmt.Errorf("domain %s does not say why it exists", d.Name)
		}
		if len(d.Tests) == 0 && len(d.Run) == 0 {
			return fmt.Errorf("domain %s names no evidence", d.Name)
		}
		if !d.Always && len(d.Sources) == 0 {
			return fmt.Errorf("domain %s is never selected: no sources and not always", d.Name)
		}
		patterns = append(append(patterns, d.Sources...), d.Tests...)
	}
	for _, p := range patterns {
		if _, err := path.Match(strings.TrimPrefix(p, "**/"), ""); err != nil {
			return fmt.Errorf("pattern %q: %w", p, err)
		}
	}
	return nil
}

// Repo is the repository facts the plan is computed from. Collecting them is
// I/O; planning from them is not, so the plan is deterministic and testable.
type Repo struct {
	// TestsInFile maps every focused-package test file to its Test functions.
	TestsInFile map[string][]string
	// DepDirs are the in-module package directories the focused package
	// imports. An unclassified change there can break it in unknown ways.
	DepDirs []string
	// PackageDirs are all module package directories.
	PackageDirs []string
}

type Plan struct {
	// Reasons explains every selection, one line per piece of evidence.
	Reasons []string
	// Whole is set when some change cannot be classified: fail safe to the
	// whole focused package rather than to zero behavioral evidence.
	Whole bool
	Run   []string
	Race  []string
}

func matches(pattern, p string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "**"); ok && strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(p, prefix)
	}
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		matched, _ := path.Match(rest, path.Base(p))
		return matched
	}
	matched, _ := path.Match(pattern, p)
	return matched
}

func matchesAny(patterns []string, p string) bool {
	for _, pattern := range patterns {
		if matches(pattern, p) {
			return true
		}
	}
	return false
}

func under(dirs []string, p string) bool {
	for _, dir := range dirs {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// PlanEvidence computes the T1 evidence for a set of changed paths.
func PlanEvidence(reg Registry, repo Repo, changed []string) Plan {
	var plan Plan
	because := map[string][]string{}
	run, race := map[string]bool{}, map[string]bool{}
	inert := 0
	for _, d := range reg.Domains {
		if d.Always {
			because[d.Name] = append(because[d.Name], "always runs")
		}
	}
	for _, p := range changed {
		if matchesAny(reg.Inert, p) {
			inert++
			continue
		}
		if strings.HasSuffix(p, "_test.go") && !strings.HasPrefix(p, reg.FocusedPackage+"/") {
			// Test files are never imported, so outside the focused package
			// they affect only their own package, which runs whole.
			continue
		}
		if strings.HasSuffix(p, "_test.go") {
			tests, exists := repo.TestsInFile[p]
			switch {
			case !exists:
				plan.Reasons = append(plan.Reasons, "nothing to run: "+p+" was deleted")
			case len(tests) == 0:
				plan.Whole = true
				plan.Reasons = append(plan.Reasons, "whole ./"+reg.FocusedPackage+": "+p+" is a shared test helper with no tests of its own")
			default:
				for _, t := range tests {
					run[t] = true
				}
				plan.Reasons = append(plan.Reasons, fmt.Sprintf("own tests (%d): %s changed", len(tests), p))
			}
			continue
		}
		classified := false
		for _, d := range reg.Domains {
			if matchesAny(d.Sources, p) {
				because[d.Name] = append(because[d.Name], p+" changed")
				classified = true
			}
		}
		switch {
		case classified:
		case under(append([]string{reg.FocusedPackage}, repo.DepDirs...), p):
			plan.Whole = true
			plan.Reasons = append(plan.Reasons, "whole ./"+reg.FocusedPackage+": "+p+" is not classified by any domain in ci/evidence/registry.json")
		case under(repo.PackageDirs, p):
			// Covered: every package but the focused one runs whole in T1.
		default:
			plan.Whole = true
			plan.Reasons = append(plan.Reasons, "whole ./"+reg.FocusedPackage+": "+p+" is outside every package and not declared inert")
		}
	}
	for _, d := range reg.Domains {
		reasons, selected := because[d.Name]
		if !selected {
			continue
		}
		tests := domainTests(d, repo)
		for _, t := range tests {
			run[t] = true
			if d.Race {
				race[t] = true
			}
		}
		label := d.Name
		if d.Race {
			label += " (race)"
		}
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("%s, %d tests: %s", label, len(tests), strings.Join(reasons, ", ")))
	}
	if inert > 0 {
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("no behavioral evidence: %d inert path(s)", inert))
	}
	plan.Run, plan.Race = sortedKeys(run), sortedKeys(race)
	return plan
}

func domainTests(d Domain, repo Repo) []string {
	set := map[string]bool{}
	for file, tests := range repo.TestsInFile {
		if !matchesAny(d.Tests, file) {
			continue
		}
		for _, t := range tests {
			set[t] = true
		}
	}
	for _, t := range d.Run {
		set[t] = true
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RunPattern is the anchored -run expression for exactly these tests.
func RunPattern(tests []string) string {
	return "^(" + strings.Join(tests, "|") + ")$"
}
