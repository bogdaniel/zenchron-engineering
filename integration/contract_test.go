package integration

import (
	"fmt"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func input(unitID, revision string) orchestration.WorkUnitInput {
	return orchestration.WorkUnitInput{
		UnitID: unitID,
		UnitOutput: orchestration.UnitOutput{
			HandoffID: "handoff-" + unitID, RunID: "run-" + unitID,
			CandidateRevision: revision, CandidateTree: "tree-" + unitID,
			Outcome: "completed", Summary: "did the thing",
		},
	}
}

func TestContractValidateRefusesIncomplete(t *testing.T) {
	ok := orchestration.WorkUnitInputs{input("a", "rev-a"), input("b", "rev-b")}
	cases := map[string]Contract{
		"bad schema":    {SchemaVersion: "9.9", GraphID: "g", UnitID: "u", BaseRevision: "base", Inputs: ok},
		"missing graph": {SchemaVersion: SchemaVersion, UnitID: "u", BaseRevision: "base", Inputs: ok},
		"missing unit":  {SchemaVersion: SchemaVersion, GraphID: "g", BaseRevision: "base", Inputs: ok},
		"missing base":  {SchemaVersion: SchemaVersion, GraphID: "g", UnitID: "u", Inputs: ok},
		"one input": {SchemaVersion: SchemaVersion, GraphID: "g", UnitID: "u", BaseRevision: "base",
			Inputs: orchestration.WorkUnitInputs{input("a", "rev-a")}},
		"no inputs": {SchemaVersion: SchemaVersion, GraphID: "g", UnitID: "u", BaseRevision: "base"},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: accepted an invalid contract", name)
		}
	}
}

func TestContractValidateRefusesTooManyInputs(t *testing.T) {
	inputs := make(orchestration.WorkUnitInputs, MaxInputs+1)
	for i := range inputs {
		inputs[i] = input(fmt.Sprintf("unit-%d", i), "rev")
	}
	c := Contract{SchemaVersion: SchemaVersion, GraphID: "g", UnitID: "u", BaseRevision: "base", Inputs: inputs}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a contract above the input bound")
	}
}

func TestNewContractAcceptsTwoInputs(t *testing.T) {
	inputs := orchestration.WorkUnitInputs{input("a", "rev-a"), input("b", "rev-b")}
	c, err := NewContract("graph-1", "integrate", "base-rev", inputs)
	if err != nil {
		t.Fatal(err)
	}
	if c.GraphID != "graph-1" || c.UnitID != "integrate" || c.BaseRevision != "base-rev" {
		t.Fatalf("contract does not carry what it was given: %+v", c)
	}
}

// TestDigestIsOrderIndependent proves requirement D: the same two upstream
// outputs name the same integration attempt however a caller assembled or
// received them.
func TestDigestIsOrderIndependent(t *testing.T) {
	a, b := input("alpha", "rev-alpha"), input("beta", "rev-beta")
	forward, err := NewContract("graph-1", "integrate", "base-rev", orchestration.WorkUnitInputs{a, b})
	if err != nil {
		t.Fatal(err)
	}
	backward, err := NewContract("graph-1", "integrate", "base-rev", orchestration.WorkUnitInputs{b, a})
	if err != nil {
		t.Fatal(err)
	}
	forwardDigest, err := forward.Digest()
	if err != nil {
		t.Fatal(err)
	}
	backwardDigest, err := backward.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if forwardDigest != backwardDigest {
		t.Fatalf("digest depends on arrival order: %s vs %s", forwardDigest, backwardDigest)
	}
}

// TestPlanIsOrderIndependent proves requirement 3: composition order is
// deterministic, independent of arrival order.
func TestPlanIsOrderIndependent(t *testing.T) {
	a, b := input("alpha", "rev-alpha"), input("beta", "rev-beta")
	forward, err := NewContract("graph-1", "integrate", "base-rev", orchestration.WorkUnitInputs{a, b})
	if err != nil {
		t.Fatal(err)
	}
	backward, err := NewContract("graph-1", "integrate", "base-rev", orchestration.WorkUnitInputs{b, a})
	if err != nil {
		t.Fatal(err)
	}
	fp, bp := forward.Plan(), backward.Plan()
	if len(fp) != 2 || len(bp) != 2 {
		t.Fatalf("plan did not carry both inputs: %+v / %+v", fp, bp)
	}
	if fp[0].UnitID != "alpha" || fp[1].UnitID != "beta" {
		t.Fatalf("plan is not canonically ordered: %+v", fp)
	}
	if fp[0] != bp[0] || fp[1] != bp[1] {
		t.Fatalf("plan depends on arrival order: %+v vs %+v", fp, bp)
	}
}

func TestContractDigestChangesWithBase(t *testing.T) {
	inputs := orchestration.WorkUnitInputs{input("a", "rev-a"), input("b", "rev-b")}
	c1, err := NewContract("graph-1", "integrate", "base-one", inputs)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := NewContract("graph-1", "integrate", "base-two", inputs)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := c1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := c2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("two different base revisions produced the same digest")
	}
}
