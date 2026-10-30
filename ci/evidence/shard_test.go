package main

import (
	"fmt"
	"slices"
	"sort"
	"testing"
)

func realCosts(t *testing.T) Costs {
	t.Helper()
	costs, err := LoadCosts()
	if err != nil {
		t.Fatal(err)
	}
	return costs
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
				names, err := ShardOf(all, costs, fmt.Sprintf("%d/%d", part, count))
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
	all := selections(t)["hotspot plain"]
	want := Partition(all, costs, 8)
	shuffled := slices.Clone(all)
	sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
	if got := Partition(append(shuffled, shuffled[0]), costs, 8); !equalShards(got, want) {
		t.Fatal("input order or a duplicate changed the partition; independent runners would disagree")
	}
}

// loads returns the summed cost of each shard.
func loads(shards [][]string, costs Costs) (loads []float64, largest float64) {
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

// The race shards used to be dealt round robin over the name-sorted selection.
func TestShardsAreBalancedByCostNotByCount(t *testing.T) {
	costs := realCosts(t)
	for name, all := range map[string][]string{"plain": selections(t)["hotspot plain"], "race": selections(t)["hotspot race"]} {
		for _, count := range []int{4, 8} {
			byName := make([][]string, count)
			for i, test := range all {
				byName[i%count] = append(byName[i%count], test)
			}
			before, _ := loads(byName, costs)
			after, largest := loads(Partition(all, costs, count), costs)
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
				t.Errorf("%s into %d: the heaviest shard is %.0fs, worse than the %.0fs of dealing by name", name, count, slices.Max(after), slices.Max(before))
			}
			t.Logf("%s into %d: heaviest shard %.0fs (mean %.0fs); dealing by name gave %.0fs", name, count, slices.Max(after), total/float64(count), slices.Max(before))
		}
	}
}

func TestTheCostTableNamesRealTestsAndIsValid(t *testing.T) {
	costs := realCosts(t)
	_, repo, _ := loadReal(t)
	known := map[string]bool{}
	for _, tests := range repo.TestsInFile {
		for _, test := range tests {
			known[test] = true
		}
	}
	for test := range costs.Tests {
		if !known[test] {
			t.Errorf("the cost table lists %q, which is not a runtime test; refresh the table", test)
		}
	}
	for _, broken := range []string{`{`, `{"schema":"x","default":1}`, `{"schema":"zenchron-evidence-costs/1","default":0}`, `{"schema":"zenchron-evidence-costs/1","default":1,"tests":{"TestA":-1}}`} {
		saved := costsJSON
		costsJSON = []byte(broken)
		_, err := LoadCosts()
		costsJSON = saved
		if err == nil {
			t.Errorf("accepted the cost table %s", broken)
		}
	}
}

func TestShardSelectorsAreValidatedAndEmptyEvidenceStaysEmpty(t *testing.T) {
	costs := realCosts(t)
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
