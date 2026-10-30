package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

var realCostsJSON = costsJSON

func realCosts(t *testing.T) CostTable {
	t.Helper()
	costs, err := LoadCosts()
	if err != nil {
		t.Fatal(err)
	}
	return costs
}

// profileFor is the cost profile the evidence tool balances a selection by: the
// race selections by race measurements, the plain ones by plain measurements.
func profileFor(selection string, table CostTable) CostProfile {
	if strings.HasSuffix(selection, " race") {
		return table.Race
	}
	return table.Plain
}

// selections are the test sets CI shards: a narrow change, and the two kinds of
// whole-package fallback (a declared hotspot, an unclassified path).
func selections(t *testing.T) map[string][]string {
	t.Helper()
	reg, repo, _ := loadReal(t)
	out := map[string][]string{}
	for name, changes := range map[string][]string{
		"narrow":   {"runtime/operator_action_receipt.go"},
		"hotspot":  {"runtime/operations.go"},
		"fallback": {"go.mod"},
	} {
		plan := PlanEvidence(reg, repo, changes)
		out[name+" plain"] = plan.selected(repo)
		out[name+" race"] = plan.Race
	}
	return out
}

func TestShardsCoverEverySelectedTestExactlyOnce(t *testing.T) {
	costs := realCosts(t)
	for name, all := range selections(t) {
		if len(all) == 0 {
			t.Fatalf("%s: the selection is empty, so the partition is not being exercised", name)
		}
		for _, count := range []int{1, 4, 8} {
			seen := map[string]int{}
			for part := 1; part <= count; part++ {
				names, err := ShardOf(all, profileFor(name, costs), fmt.Sprintf("%d/%d", part, count))
				if err != nil {
					t.Fatal(err)
				}
				for _, test := range names {
					seen[test]++
				}
			}
			if len(seen) != len(all) {
				t.Fatalf("%s into %d: %d tests came out of %d selected", name, count, len(seen), len(all))
			}
			for _, test := range all {
				if seen[test] != 1 {
					t.Fatalf("%s into %d: %s ran %d times", name, count, test, seen[test])
				}
			}
		}
	}
}

func TestAWholePackageFallbackShardsTheUnshardedRunsTests(t *testing.T) {
	reg, repo, _ := loadReal(t)
	plan := PlanEvidence(reg, repo, []string{"runtime/operations.go"})
	if !plan.Whole {
		t.Fatal("the hotspot no longer falls back to the whole package")
	}
	all := map[string]bool{}
	for _, tests := range repo.TestsInFile {
		for _, test := range tests {
			all[test] = true
		}
	}
	if got := plan.selected(repo); len(got) != len(all) {
		t.Fatalf("a whole-package fallback shards %d tests; the unsharded run executes %d", len(got), len(all))
	}
}

func TestAPartitionDependsOnlyOnTheNamesAndTheCosts(t *testing.T) {
	costs := realCosts(t)
	for name, all := range selections(t) {
		profile := profileFor(name, costs)
		want := Partition(all, profile, 8)
		shuffled := slices.Clone(all)
		sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
		if got := Partition(append(shuffled, shuffled[0]), profile, 8); !equalShards(got, want) {
			t.Errorf("%s: input order or a duplicate changed the partition; independent runners would disagree", name)
		}
	}
}

// loads returns the summed cost of each shard.
func loads(shards [][]string, costs CostProfile) (loads []float64, largest float64) {
	for _, shard := range shards {
		load := 0.0
		for _, test := range shard {
			load += costs.of(test)
			largest = max(largest, costs.of(test))
		}
		loads = append(loads, load)
	}
	return loads, largest
}

// Balance is judged against what each profile replaced: the plain shards
// against dealing by name, the race shards against the same partition balanced
// by plain costs (what the race shards used before the race profile), both
// evaluated under the profile of the run that is being balanced.
func TestShardsAreBalancedByTheirOwnMeasuredCosts(t *testing.T) {
	costs := realCosts(t)
	for _, name := range []string{"hotspot plain", "hotspot race", "fallback race"} {
		all := selections(t)[name]
		profile := profileFor(name, costs)
		for _, count := range []int{4, 8} {
			previous := make([][]string, count)
			if strings.HasSuffix(name, " race") {
				previous = Partition(all, costs.Plain, count)
			} else {
				for i, test := range all {
					previous[i%count] = append(previous[i%count], test)
				}
			}
			before, _ := loads(previous, profile)
			after, largest := loads(Partition(all, profile, count), profile)
			total := 0.0
			for _, load := range after {
				total += load
			}
			// Longest-processing-time dealing leaves the heaviest shard within
			// one test of the mean, and no worse than the split it replaced.
			if mean := total / float64(count); slices.Max(after) > mean+largest {
				t.Errorf("%s into %d: heaviest %.0fs against a mean of %.0fs and a largest test of %.0fs", name, count, slices.Max(after), mean, largest)
			}
			if slices.Max(after) > slices.Max(before) {
				t.Errorf("%s into %d: the heaviest shard is %.0fs, worse than the %.0fs it replaced", name, count, slices.Max(after), slices.Max(before))
			}
			t.Logf("%s into %d: heaviest shard %.0fs (mean %.0fs); the previous split gave %.0fs", name, count, slices.Max(after), total/float64(count), slices.Max(before))
		}
	}
}

// Measurements decide balance and nothing else. Whatever the profile holds,
// every selected test is in exactly one shard.
func TestMissingAndStaleMeasurementsNeverChangeWhichTestsRun(t *testing.T) {
	all := selections(t)["hotspot race"]
	stale := map[string]float64{"TestThatWasDeleted": 900, "TestAlsoGone": 800}
	for _, test := range all[:50] {
		stale[test] = 0.001
	}
	for name, profile := range map[string]CostProfile{
		"no measurements":           {Default: 1},
		"stale and partial entries": {Default: 1, Tests: stale},
		"one absurd measurement":    {Default: 1, Tests: map[string]float64{all[0]: 1e9}},
	} {
		seen := map[string]int{}
		for _, shard := range Partition(all, profile, 8) {
			for _, test := range shard {
				seen[test]++
			}
		}
		if len(seen) != len(all) {
			t.Errorf("%s: %d tests ran of %d selected (stale names must not be added either)", name, len(seen), len(all))
		}
		for _, test := range all {
			if seen[test] != 1 {
				t.Errorf("%s: %s ran %d times", name, test, seen[test])
			}
		}
	}
}

func TestTheCostTableNamesRealTestsAndIsValid(t *testing.T) {
	table := realCosts(t)
	_, repo, _ := loadReal(t)
	known := map[string]bool{}
	for _, tests := range repo.TestsInFile {
		for _, test := range tests {
			known[test] = true
		}
	}
	for name, profile := range map[string]CostProfile{"plain": table.Plain, "race": table.Race} {
		if len(profile.Tests) == 0 {
			t.Errorf("the %s profile lists no measured test", name)
		}
		for test := range profile.Tests {
			if !known[test] {
				t.Errorf("the %s profile lists %q, which is not a runtime test; refresh the table", name, test)
			}
		}
	}
	const source = `"source":{"commit":"c","runs":["1"],"configuration":"x","observed":"2026-10-30","samples":1,"aggregation":"median"}`
	profile := `{` + source + `,"default":1,"tests":{}}`
	noAggregation := strings.Replace(source, `,"aggregation":"median"`, "", 1)
	if noAggregation == source {
		t.Fatal("the aggregation-less variant did not change the provenance")
	}
	for name, broken := range map[string]string{
		"not json":               `{`,
		"old schema":             `{"schema":"zenchron-evidence-costs/1","plain":` + profile + `,"race":` + profile + `}`,
		"no race profile":        `{"schema":"zenchron-evidence-costs/2","unit":"seconds","plain":` + profile + `}`,
		"no provenance":          `{"schema":"zenchron-evidence-costs/2","unit":"seconds","plain":` + profile + `,"race":{"default":1}}`,
		"zero default":           `{"schema":"zenchron-evidence-costs/2","unit":"seconds","plain":` + profile + `,"race":{` + source + `,"default":0}}`,
		"negative measured cost": `{"schema":"zenchron-evidence-costs/2","unit":"seconds","plain":` + profile + `,"race":{` + source + `,"default":1,"tests":{"TestA":-1}}}`,
	} {
		saved := costsJSON
		costsJSON = []byte(broken)
		_, err := LoadCosts()
		costsJSON = saved
		if err == nil {
			t.Errorf("accepted the cost table (%s): %s", name, broken)
		}
	}
	costsJSON = []byte(`{"schema":"zenchron-evidence-costs/2","unit":"seconds","plain":` + profile + `,"race":` + profile + `}`)
	_, err := LoadCosts()
	costsJSON = realCostsJSON
	if err != nil {
		t.Errorf("rejected a minimal valid table: %v", err)
	}
}

func TestShardSelectorsAreValidatedAndEmptyEvidenceStaysEmpty(t *testing.T) {
	costs := CostProfile{Default: 1}
	for _, invalid := range []string{"0/4", "5/4", "1/0", "1/-1", "1", "1/2/3", "bad", "9999999999999999999999/4"} {
		if _, err := ShardOf([]string{"TestOne"}, costs, invalid); err == nil {
			t.Fatalf("accepted invalid shard %q", invalid)
		}
	}
	if names, err := ShardOf(nil, costs, "1/4"); err != nil || len(names) != 0 {
		t.Fatalf("an empty selection invented evidence: %v %v", names, err)
	}
}

func TestOptionsRefuseSlicesThatWouldBeConfused(t *testing.T) {
	for name, opts := range map[string]options{
		"race shard without race": {raceShard: "1/4"},
		"race and shard":          {race: true, shard: "1/4"},
		"race and packages":       {race: true, packages: true},
		"shard and packages":      {shard: "1/4", packages: true},
		"plan and packages":       {planOnly: true, packages: true},
		"plan and shard":          {planOnly: true, shard: "1/4"},
		"plan and race":           {planOnly: true, race: true},
		"plan and race shard":     {planOnly: true, race: true, raceShard: "1/8"},
	} {
		if opts.validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, ok := range []options{{}, {planOnly: true}, {race: true, raceShard: "1/8"}, {shard: "2/4"}, {packages: true}} {
		if err := ok.validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

// The sharded run is safe only if it names every test the unsharded run
// executes. These are the ways a function escapes the by-name selection.
func TestTheShardedRunRefusesTestsItWouldNotName(t *testing.T) {
	scanned := []string{"TestA", "TestB"}
	listed := "TestA\nTestB\nok  \tgithub.com/example/runtime\t0.01s\n"
	if unseen := unseenByShards(listed, scanned); len(unseen) != 0 {
		t.Fatalf("a complete list was refused: %v", unseen)
	}
	for name, extra := range map[string]string{
		"a test the scan missed": "TestHidden\n",
		"an example":             "ExampleThing\n",
		"a fuzz target":          "FuzzParse\n",
	} {
		if unseen := unseenByShards(listed+extra, scanned); len(unseen) != 1 {
			t.Errorf("%s was accepted: %v", name, unseen)
		}
	}
}

func equalShards(a, b [][]string) bool {
	return slices.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}
