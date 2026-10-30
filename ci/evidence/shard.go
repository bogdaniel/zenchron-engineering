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

const costsSchema = "zenchron-evidence-costs/1"

// Costs is the measured plain-run duration of the expensive focused tests.
// ponytail: a checked-in table refreshed by hand from a push-event T2 log; a
// stale or missing entry costs balance, never coverage. Regenerate it when shard
// times drift apart (docs/ci-evidence.md, "Shard balance").
type Costs struct {
	Schema  string             `json:"schema"`
	Unit    string             `json:"unit"`
	Source  string             `json:"source"`
	Default float64            `json:"default"`
	Tests   map[string]float64 `json:"tests"`
}

func LoadCosts() (Costs, error) {
	var costs Costs
	if err := json.Unmarshal(costsJSON, &costs); err != nil {
		return costs, fmt.Errorf("the shard cost table is not valid JSON: %w", err)
	}
	var errs []error
	if costs.Schema != costsSchema {
		errs = append(errs, fmt.Errorf("shard cost table schema %q, want %q", costs.Schema, costsSchema))
	}
	if costs.Default <= 0 {
		errs = append(errs, errors.New("the shard cost table needs a positive default for unlisted tests"))
	}
	for name, seconds := range costs.Tests {
		if seconds <= 0 {
			errs = append(errs, fmt.Errorf("shard cost of %q must be positive, got %v", name, seconds))
		}
	}
	return costs, errors.Join(errs...)
}

func (c Costs) of(test string) float64 {
	if seconds, listed := c.Tests[test]; listed {
		return seconds
	}
	return c.Default
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
func Partition(tests []string, costs Costs, count int) [][]string {
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
func ShardOf(tests []string, costs Costs, shard string) ([]string, error) {
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
