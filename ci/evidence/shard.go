package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Sharding splits a test selection across isolated runners without changing
// which tests run: a shard is a set of test names, the shards of one selection
// are a partition of it, and every runner computes the same partition from the
// same checkout. Balance is a speed concern only. A wrong cost can make a shard
// slow; it can never drop or duplicate a test.

//go:embed costs.json
var costsJSON []byte

const costsSchema = "zenchron-evidence-costs/2"

// CostTable holds one measured profile per kind of run. A -race test is not a
// constant multiple of its plain cost (measured ratios run from 1x to 19x), so
// the race shards are balanced by race measurements and the plain shards by
// plain ones.
// ponytail: a checked-in table refreshed by hand from hosted CI logs; a stale or
// missing entry costs balance, never coverage. Regenerate it when shard times
// drift apart (docs/ci-evidence.md, "Shard balance").
type CostTable struct {
	Schema string      `json:"schema"`
	Unit   string      `json:"unit"`
	Plain  CostProfile `json:"plain"`
	Race   CostProfile `json:"race"`
}

// CostProfile is where the numbers came from and the numbers. Tests lists the
// expensive tests only; Default stands for every other test.
type CostProfile struct {
	Source  CostSource         `json:"source"`
	Default float64            `json:"default"`
	Tests   map[string]float64 `json:"tests"`
}

// CostSource is the provenance of a profile: the exact revision measured, the
// hosted runs and configuration that produced the durations, and when.
type CostSource struct {
	Commit        string   `json:"commit"`
	Runs          []string `json:"runs"`
	Configuration string   `json:"configuration"`
	Observed      string   `json:"observed"`
	Samples       int      `json:"samples"`
	Aggregation   string   `json:"aggregation"`
}

func LoadCosts() (CostTable, error) {
	var table CostTable
	if err := json.Unmarshal(costsJSON, &table); err != nil {
		return table, fmt.Errorf("the shard cost table is not valid JSON: %w", err)
	}
	var errs []error
	if table.Schema != costsSchema {
		errs = append(errs, fmt.Errorf("shard cost table schema %q, want %q", table.Schema, costsSchema))
	}
	if table.Unit != "seconds" {
		errs = append(errs, fmt.Errorf("shard cost table unit %q, want \"seconds\"", table.Unit))
	}
	errs = append(errs, table.Plain.validate("plain"), table.Race.validate("race"))
	return table, errors.Join(errs...)
}

func (p CostProfile) validate(name string) error {
	var errs []error
	if src := p.Source; src.Commit == "" || len(src.Runs) == 0 || src.Configuration == "" || src.Observed == "" || src.Samples < 1 || src.Aggregation == "" {
		errs = append(errs, fmt.Errorf("the %s cost profile needs its provenance: commit, runs, configuration, observation date, sample count and aggregation", name))
	}
	if p.Default <= 0 {
		errs = append(errs, fmt.Errorf("the %s cost profile needs a positive default for unlisted tests", name))
	}
	for test, seconds := range p.Tests {
		if seconds <= 0 {
			errs = append(errs, fmt.Errorf("%s cost of %q must be positive, got %v", name, test, seconds))
		}
	}
	return errors.Join(errs...)
}

func (p CostProfile) of(test string) float64 {
	if seconds, listed := p.Tests[test]; listed {
		return seconds
	}
	return p.Default
}

// ParseShard reads "part/count" with 1 <= part <= count.
func ParseShard(text string) (part, count int, err error) {
	partText, countText, ok := strings.Cut(text, "/")
	part, partErr := strconv.Atoi(partText)
	count, countErr := strconv.Atoi(countText)
	if !ok || partErr != nil || countErr != nil || part < 1 || count < 1 || part > count {
		return 0, 0, fmt.Errorf("invalid shard %q: want part/count with 1 <= part <= count", text)
	}
	return part, count, nil
}

// Partition deals the tests into count shards, heaviest first onto the lightest
// shard (ties to the lowest index), so the result depends only on the names and
// the cost table. Duplicate names are one test.
func Partition(tests []string, costs CostProfile, count int) [][]string {
	ordered := slices.Clone(tests)
	sort.Strings(ordered)
	ordered = slices.Compact(ordered)
	sort.SliceStable(ordered, func(i, j int) bool { return costs.of(ordered[i]) > costs.of(ordered[j]) })
	shards, load := make([][]string, count), make([]float64, count)
	for _, test := range ordered {
		lightest := 0
		for i := range load {
			if load[i] < load[lightest] {
				lightest = i
			}
		}
		shards[lightest] = append(shards[lightest], test)
		load[lightest] += costs.of(test)
	}
	for _, shard := range shards {
		sort.Strings(shard)
	}
	return shards
}

// ShardOf is the tests of one shard of a selection.
func ShardOf(tests []string, costs CostProfile, shard string) ([]string, error) {
	part, count, err := ParseShard(shard)
	if err != nil {
		return nil, err
	}
	return Partition(tests, costs, count)[part-1], nil
}

// selected is the focused tests T1 runs plain: every one of them when the plan
// fell back to the whole package, otherwise the domains' tests.
func (p Plan) selected(repo Repo) []string {
	if !p.Whole {
		return p.Run
	}
	all := map[string]bool{}
	for _, tests := range repo.TestsInFile {
		for _, test := range tests {
			all[test] = true
		}
	}
	return sortedKeys(all)
}

// verifyEveryTestIsSeen is what makes a whole-package fallback safe to shard.
// Shards run tests by name, so a test the source scan cannot name would run in
// the unsharded `go test ./runtime` and in no shard. The compiler's own list is
// the authority.
func verifyEveryTestIsSeen(root, focused string, repo Repo) error {
	listed, err := output(root, "go", "test", "-list", ".*", "./"+focused)
	if err != nil {
		return err
	}
	unseen := unseenByShards(listed, (Plan{Whole: true}).selected(repo))
	if len(unseen) > 0 {
		return fmt.Errorf("sharded evidence would not run %d ./%s function(s) the unsharded run executes: %s",
			len(unseen), focused, strings.Join(unseen, ", "))
	}
	return nil
}

// unseenByShards names what `go test -list` reports that a by-name shard would
// miss: a Test the scan did not find, and any Fuzz or Example function, which
// -run would also have to name.
func unseenByShards(listed string, scanned []string) []string {
	known := map[string]bool{}
	for _, name := range scanned {
		known[name] = true
	}
	var unseen []string
	for _, name := range strings.Fields(listed) {
		switch {
		case strings.HasPrefix(name, "Test") && !known[name],
			strings.HasPrefix(name, "Fuzz"), strings.HasPrefix(name, "Example"):
			unseen = append(unseen, name)
		}
	}
	return unseen
}
