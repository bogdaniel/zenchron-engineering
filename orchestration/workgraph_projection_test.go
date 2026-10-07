package orchestration

import (
	"strings"
	"testing"
)

func output(revision string) *UnitOutput {
	return &UnitOutput{HandoffID: "handoff-" + revision, CandidateRevision: revision, CandidateTree: "tree-" + revision}
}

func project(t *testing.T, facts map[string]UnitFacts) WorkGraphProjection {
	t.Helper()
	projection, err := ProjectWorkGraph(composed(t, 1, diamond()), facts)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return projection
}

func assertStates(t *testing.T, projection WorkGraphProjection, want map[string]UnitState) {
	t.Helper()
	for id, state := range want {
		if got := projection.Units[id].State; got != state {
			t.Fatalf("unit %s is %s, want %s (reason: %s)", id, got, state, projection.Units[id].Reason)
		}
	}
}

func assertFrontier(t *testing.T, projection WorkGraphProjection, want ...string) {
	t.Helper()
	if strings.Join(projection.Frontier, ",") != strings.Join(want, ",") {
		t.Fatalf("frontier = %v, want %v", projection.Frontier, want)
	}
}

// readyDigest is the digest a unit's dependencies currently present, taken from
// the projection itself so the test and the production path cannot disagree
// about what an activation records.
func readyDigest(t *testing.T, projection WorkGraphProjection, unitID string) string {
	t.Helper()
	digest := projection.Units[unitID].InputsDigest
	if digest == "" {
		t.Fatalf("unit %s presents no inputs digest; it is %s", unitID, projection.Units[unitID].State)
	}
	return digest
}

// TestOnlyTheRootIsInitiallyRunnable is #472 acceptance 1, first half.
func TestOnlyTheRootIsInitiallyRunnable(t *testing.T) {
	projection := project(t, nil)
	assertFrontier(t, projection, "a")
	assertStates(t, projection, map[string]UnitState{
		"a": UnitReady, "b": UnitBlocked, "c": UnitBlocked, "d": UnitBlocked,
	})
	if !strings.Contains(projection.Units["b"].Reason, `dependency "a" is ready and has transferred no admitted handoff yet`) {
		t.Fatalf("b's reason does not name its unsatisfied dependency: %q", projection.Units["b"].Reason)
	}
	// A root unit still records a digest, so a later activation has something
	// to be compared against.
	readyDigest(t, projection, "a")
}

// TestProviderExitAloneUnlocksNothing is #472's core law: a producer unit is not
// satisfied because its worker finished, only because its handoff was admitted.
func TestProviderExitAloneUnlocksNothing(t *testing.T) {
	root := project(t, nil)
	digest := readyDigest(t, root, "a")
	for _, item := range []ItemState{ItemRunning, ItemWaiting, ItemHandoffPending, ItemPartial, ItemNotCreated, ItemQueued} {
		projection := project(t, map[string]UnitFacts{
			"a": {Activated: true, ActivationInputsDigest: digest, Item: item},
		})
		assertFrontier(t, projection)
		assertStates(t, projection, map[string]UnitState{
			"a": UnitState(item), "b": UnitBlocked, "c": UnitBlocked, "d": UnitBlocked,
		})
	}
}

// TestAnAdmittedHandoffUnlocksBothDependents is #472 acceptance 1 and 2: B and C
// become runnable concurrently, and only once A's handoff is admitted.
func TestAnAdmittedHandoffUnlocksBothDependents(t *testing.T) {
	root := project(t, nil)
	facts := map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: readyDigest(t, root, "a"),
		Item: ItemCompleted, Output: output("c1"),
	}}
	projection := project(t, facts)
	assertFrontier(t, projection, "b", "c")
	assertStates(t, projection, map[string]UnitState{
		"a": UnitState(ItemCompleted), "b": UnitReady, "c": UnitReady, "d": UnitBlocked,
	})
	// B and C consume the same single upstream output, so they record the same
	// digest; D consumes both, so it records neither until both exist.
	if readyDigest(t, projection, "b") != readyDigest(t, projection, "c") {
		t.Fatal("two units consuming one output recorded different inputs")
	}
	if projection.Units["d"].InputsDigest != "" {
		t.Fatal("d recorded an inputs digest while a dependency has no output")
	}
}

// TestDWaitsForBothUpstreamHandoffs is #472 acceptance 3.
func TestDWaitsForBothUpstreamHandoffs(t *testing.T) {
	root := project(t, nil)
	rootDigest := readyDigest(t, root, "a")
	unlocked := project(t, map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1"),
	}})
	branch := readyDigest(t, unlocked, "b")
	facts := map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
		"b": {Activated: true, ActivationInputsDigest: branch, Item: ItemCompleted, Output: output("c2")},
		"c": {Activated: true, ActivationInputsDigest: branch, Item: ItemRunning},
	}
	half := project(t, facts)
	assertFrontier(t, half)
	assertStates(t, half, map[string]UnitState{"d": UnitBlocked})
	if !strings.Contains(half.Units["d"].Reason, `dependency "c" is running`) {
		t.Fatalf("d's reason does not name the dependency it waits for: %q", half.Units["d"].Reason)
	}
	facts["c"] = UnitFacts{Activated: true, ActivationInputsDigest: branch, Item: ItemCompleted, Output: output("c3")}
	whole := project(t, facts)
	assertFrontier(t, whole, "d")
	assertStates(t, whole, map[string]UnitState{"d": UnitReady})
}

// TestAFailedUnitBlocksOnlyWhatDependsOnIt is #472 acceptance 4.
func TestAFailedUnitBlocksOnlyWhatDependsOnIt(t *testing.T) {
	root := project(t, nil)
	rootDigest := readyDigest(t, root, "a")
	unlocked := project(t, map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1"),
	}})
	branch := readyDigest(t, unlocked, "b")
	for _, terminal := range []ItemState{ItemFailed, ItemStopped} {
		projection := project(t, map[string]UnitFacts{
			"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
			"b": {Activated: true, ActivationInputsDigest: branch, Item: ItemRunning},
			"c": {Activated: true, ActivationInputsDigest: branch, Item: terminal},
		})
		assertStates(t, projection, map[string]UnitState{
			"b": UnitState(ItemRunning), "c": UnitState(terminal), "d": UnitBlocked,
		})
		if !strings.Contains(projection.Units["d"].Reason, "will never transfer an admitted handoff") {
			t.Fatalf("a %s dependency reads as merely waiting: %q", terminal, projection.Units["d"].Reason)
		}
	}
	// The UNRELATED branch keeps going: with B completed and C failed, nothing
	// about B is held back, and B's own output is still its own.
	projection := project(t, map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
		"b": {Activated: true, ActivationInputsDigest: branch, Item: ItemCompleted, Output: output("c2")},
		"c": {Activated: true, ActivationInputsDigest: branch, Item: ItemFailed},
	})
	assertStates(t, projection, map[string]UnitState{"b": UnitState(ItemCompleted), "d": UnitBlocked})
	assertFrontier(t, projection)
}

// TestATerminalUnsatisfiedUnitReadsAsDeadNotWaiting: handoff_pending and partial
// both occur on a LIVE child that may still transfer an admitted handoff, and on
// an ENDED one that never will. The two must not read the same to an operator.
func TestATerminalUnsatisfiedUnitReadsAsDeadNotWaiting(t *testing.T) {
	root := project(t, nil)
	digest := readyDigest(t, root, "a")
	for _, item := range []ItemState{ItemHandoffPending, ItemPartial} {
		live := project(t, map[string]UnitFacts{
			"a": {Activated: true, ActivationInputsDigest: digest, Item: item},
		})
		if strings.Contains(live.Units["b"].Reason, "will never") {
			t.Fatalf("a LIVE %s dependency reads as dead: %q", item, live.Units["b"].Reason)
		}
		ended := project(t, map[string]UnitFacts{
			"a": {Activated: true, ActivationInputsDigest: digest, Item: item, Terminal: true},
		})
		assertStates(t, ended, map[string]UnitState{"a": UnitState(item), "b": UnitBlocked, "d": UnitBlocked})
		for _, dependent := range []string{"b", "c", "d"} {
			if !strings.Contains(ended.Units[dependent].Reason, "will never transfer an admitted handoff") {
				t.Fatalf("%s behind an ENDED %s dependency reads as merely waiting: %q",
					dependent, item, ended.Units[dependent].Reason)
			}
		}
		assertFrontier(t, ended)
	}
	// A terminal COMPLETED unit is satisfied, not dead.
	settled := project(t, map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: digest, Item: ItemCompleted, Terminal: true, Output: output("c1")},
	})
	assertFrontier(t, settled, "b", "c")
}

// TestReplacedUpstreamSubjectInvalidatesDownstream is #472 acceptance 7. Once
// completed is NOT always completed.
func TestReplacedUpstreamSubjectInvalidatesDownstream(t *testing.T) {
	root := project(t, nil)
	rootDigest := readyDigest(t, root, "a")
	unlocked := project(t, map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1"),
	}})
	branch := readyDigest(t, unlocked, "b")
	settled := map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
		"b": {Activated: true, ActivationInputsDigest: branch, Item: ItemCompleted, Output: output("c2")},
		"c": {Activated: true, ActivationInputsDigest: branch, Item: ItemCompleted, Output: output("c3")},
	}
	before := project(t, settled)
	assertFrontier(t, before, "d")

	// A's output is REPLACED: the run transferred a second admitted handoff
	// bound to a different candidate.
	replaced := map[string]UnitFacts{}
	for id, fact := range settled {
		replaced[id] = fact
	}
	replaced["a"] = UnitFacts{Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c9")}
	after := project(t, replaced)
	assertStates(t, after, map[string]UnitState{
		"a": UnitState(ItemCompleted), "b": UnitInvalidated, "c": UnitInvalidated, "d": UnitBlocked,
	})
	assertFrontier(t, after)
	if !strings.Contains(after.Units["b"].Reason, "replaced after this unit was activated") {
		t.Fatalf("b's invalidation does not say why: %q", after.Units["b"].Reason)
	}
	if !strings.Contains(after.Units["d"].Reason, "will never transfer an admitted handoff") {
		t.Fatalf("d is not blocked behind an invalidated dependency: %q", after.Units["d"].Reason)
	}

	// An upstream unit that stops being satisfied at all invalidates the same
	// way, rather than leaving its dependents complete.
	reopened := map[string]UnitFacts{}
	for id, fact := range settled {
		reopened[id] = fact
	}
	reopened["a"] = UnitFacts{Activated: true, ActivationInputsDigest: rootDigest, Item: ItemRunning}
	projection := project(t, reopened)
	assertStates(t, projection, map[string]UnitState{"b": UnitInvalidated, "c": UnitInvalidated, "d": UnitBlocked})
	if !strings.Contains(projection.Units["b"].Reason, "no longer holds") {
		t.Fatalf("b's invalidation does not say why: %q", projection.Units["b"].Reason)
	}
}

// TestAnUnreadableChildBlocksOnlyItsDependents keeps one bad read from deciding
// anything about the rest of the graph.
func TestAnUnreadableChildBlocksOnlyItsDependents(t *testing.T) {
	root := project(t, nil)
	rootDigest := readyDigest(t, root, "a")
	unlocked := project(t, map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1"),
	}})
	branch := readyDigest(t, unlocked, "b")
	projection := project(t, map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
		"b": {Activated: true, ActivationInputsDigest: branch, Unreadable: true, UnreadableReason: "the run row is unreadable"},
	})
	assertStates(t, projection, map[string]UnitState{
		"a": UnitState(ItemCompleted), "b": UnitUnknown, "c": UnitReady, "d": UnitBlocked,
	})
	assertFrontier(t, projection, "c")
	if projection.Units["b"].Reason != "the run row is unreadable" {
		t.Fatalf("the unknown unit does not carry the read failure: %q", projection.Units["b"].Reason)
	}
}

// TestProjectionFailsClosedOnStatesItDoesNotKnow refuses to compute a frontier
// past a child state this build cannot interpret.
func TestProjectionFailsClosedOnStatesItDoesNotKnow(t *testing.T) {
	root := project(t, nil)
	// A state this build does not know blocks what depends on it and NOTHING
	// else. It is never read as satisfied, and never guessed into a frontier.
	projection := project(t, map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: readyDigest(t, root, "a"), Item: "transcending"},
	})
	assertStates(t, projection, map[string]UnitState{
		"a": UnitUnknown, "b": UnitBlocked, "c": UnitBlocked, "d": UnitBlocked,
	})
	assertFrontier(t, projection)
	if !strings.Contains(projection.Units["a"].Reason, `unrecognized child item state "transcending"`) {
		t.Fatalf("the unknown unit does not say what it could not read: %q", projection.Units["a"].Reason)
	}
	// A completed child with no admitted output is incoherent, not satisfied.
	_, err := ProjectWorkGraph(composed(t, 1, diamond()), map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: readyDigest(t, root, "a"), Item: ItemCompleted},
	})
	if err == nil || !strings.Contains(err.Error(), "completed but names no admitted output") {
		t.Fatalf("err = %v, want a refusal of a completed unit with no output", err)
	}
}

// TestCountsAddUpToEveryUnit keeps the aggregate honest: every unit lands in
// exactly one bucket, and the activated buckets reuse #470's vocabulary.
func TestCountsAddUpToEveryUnit(t *testing.T) {
	root := project(t, nil)
	rootDigest := readyDigest(t, root, "a")
	unlocked := project(t, map[string]UnitFacts{"a": {
		Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1"),
	}})
	branch := readyDigest(t, unlocked, "b")
	projection := project(t, map[string]UnitFacts{
		"a": {Activated: true, ActivationInputsDigest: rootDigest, Item: ItemCompleted, Output: output("c1")},
		"b": {Activated: true, ActivationInputsDigest: branch, Item: ItemFailed},
		"c": {Activated: true, ActivationInputsDigest: "stale", Item: ItemRunning},
	})
	var counts WorkGraphCounts
	for _, unit := range composed(t, 1, diamond()).Units {
		counts.Add(projection.Units[unit.ID].State)
	}
	if counts.Total != 4 || counts.Blocked != 1 || counts.Invalidated != 1 ||
		counts.Activated.Total != 2 || counts.Activated.Completed != 1 || counts.Activated.Failed != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}
