package orchestration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// The #472 diamond: A, then B and C, then D.
//
//	A
//	├── B
//	└── C
//	     \ /
//	      D
func diamond() []WorkUnit {
	return []WorkUnit{
		{ID: "a", Purpose: "land the schema", Role: domain.RoleImplementer, Issue: 101},
		{ID: "b", Purpose: "land the reader", Role: domain.RoleImplementer, Issue: 102, DependsOn: []string{"a"}},
		{ID: "c", Purpose: "land the writer", Role: domain.RoleImplementer, Issue: 103, DependsOn: []string{"a"}},
		{ID: "d", Purpose: "land the surface", Role: domain.RoleImplementer, Issue: 104, DependsOn: []string{"b", "c"}},
	}
}

func composed(t *testing.T, revision int, units []WorkUnit) WorkGraph {
	t.Helper()
	graph, err := WorkGraphProposal{Name: "m2-o1", Revision: revision, Units: units}.
		Compose("acme/repo", "claude", "operator@example", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return graph
}

func TestWorkGraphIdentityAndRevisionAreDeterministic(t *testing.T) {
	first := composed(t, 1, diamond())
	// A SECOND composition a moment later, with the units in another order
	// within the document, is the same graph at the same revision.
	later, err := WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: diamond()}.
		Compose("acme/repo", "claude", "someone-else@example", time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if later.ID != first.ID {
		t.Fatalf("graph identity moved: %s then %s", first.ID, later.ID)
	}
	firstDigest, err := first.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	laterDigest, err := later.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != laterDigest {
		t.Fatalf("the same revision composed twice digests differently: %s then %s", firstDigest, laterDigest)
	}
	// A different repository, agent or name is a different graph.
	for _, other := range []WorkGraph{
		composedUnder(t, "acme/other", "claude", "m2-o1"),
		composedUnder(t, "acme/repo", "codex", "m2-o1"),
		composedUnder(t, "acme/repo", "claude", "m2-o2"),
	} {
		if other.ID == first.ID {
			t.Fatalf("a different graph shares identity %s", first.ID)
		}
	}
	// And a changed unit is a different revision digest.
	changed := diamond()
	changed[2].DependsOn = nil
	altered := composed(t, 1, changed)
	alteredDigest, err := altered.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if alteredDigest == firstDigest {
		t.Fatal("removing a dependency did not change the revision digest")
	}
}

func composedUnder(t *testing.T, repository, agent, name string) WorkGraph {
	t.Helper()
	graph, err := WorkGraphProposal{Name: name, Revision: 1, Units: diamond()}.
		Compose(repository, agent, "operator@example", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestWorkGraphRefusesGraphsNothingCouldAdvance(t *testing.T) {
	cases := map[string]struct {
		units []WorkUnit
		want  string
	}{
		"cycle": {
			units: []WorkUnit{
				{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 1, DependsOn: []string{"c"}},
				{ID: "b", Purpose: "p", Role: domain.RoleImplementer, Issue: 2, DependsOn: []string{"a"}},
				{ID: "c", Purpose: "p", Role: domain.RoleImplementer, Issue: 3, DependsOn: []string{"b"}},
			},
			want: "form a cycle among a, b, c",
		},
		"self dependency": {
			units: []WorkUnit{{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 1, DependsOn: []string{"a"}}},
			want:  "depends on itself",
		},
		"unknown dependency": {
			units: []WorkUnit{{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 1, DependsOn: []string{"ghost"}}},
			want:  `depends on "ghost", which is not a unit of this graph`,
		},
		"repeated dependency": {
			units: []WorkUnit{
				{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 1},
				{ID: "b", Purpose: "p", Role: domain.RoleImplementer, Issue: 2, DependsOn: []string{"a", "a"}},
			},
			want: `lists dependency "a" twice`,
		},
		"duplicate unit": {
			units: []WorkUnit{
				{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 1},
				{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 2},
			},
			want: `work unit "a" is named more than once`,
		},
		"two units on one issue": {
			units: []WorkUnit{
				{ID: "a", Purpose: "p", Role: domain.RoleImplementer, Issue: 7},
				{ID: "b", Purpose: "p", Role: domain.RoleImplementer, Issue: 7},
			},
			want: "both perform issue 7",
		},
		"unknown role": {
			units: []WorkUnit{{ID: "a", Purpose: "p", Role: "wizard", Issue: 1}},
			want:  "not in the role catalogue",
		},
		"no issue": {
			units: []WorkUnit{{ID: "a", Purpose: "p", Role: domain.RoleImplementer}},
			want:  "a unit performs one existing issue",
		},
		"no purpose": {
			units: []WorkUnit{{ID: "a", Role: domain.RoleImplementer, Issue: 1}},
			want:  "purpose is required",
		},
		"no unit id": {
			units: []WorkUnit{{Purpose: "p", Role: domain.RoleImplementer, Issue: 1}},
			want:  "work unit id is required",
		},
		"no units": {units: nil, want: "at least one work unit"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: test.units}.
				Compose("acme/repo", "claude", "operator@example", time.Unix(1700000000, 0).UTC())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestWorkGraphRefusesTamperedIdentityAndSchema(t *testing.T) {
	graph := composed(t, 1, diamond())
	graph.Name = "renamed"
	if err := graph.Validate(); err == nil || !strings.Contains(err.Error(), "does not match the identity") {
		t.Fatalf("a renamed graph keeping its id validated: %v", err)
	}
	graph = composed(t, 1, diamond())
	graph.SchemaVersion = "9.9"
	if err := graph.Validate(); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("an unknown schema version validated: %v", err)
	}
	graph = composed(t, 1, diamond())
	graph.Revision = 0
	if err := graph.Validate(); err == nil || !strings.Contains(err.Error(), "is not a revision") {
		t.Fatalf("revision zero validated: %v", err)
	}
	graph = composed(t, 1, diamond())
	graph.CreatedAt = time.Time{}
	if err := graph.Validate(); err == nil || !strings.Contains(err.Error(), "creation time") {
		t.Fatalf("a graph with no creation time validated: %v", err)
	}
}

// TestMutationValidationRefusesEveryEscalation is #472 acceptance 6 and the
// proposal seam: a mutation may be proposed by anything, and only deterministic
// code admits it.
func TestMutationValidationRefusesEveryEscalation(t *testing.T) {
	current := composed(t, 1, diamond())
	activated := map[string]bool{"a": true, "b": true}

	withCycle := diamond()
	withCycle[0].DependsOn = []string{"d"}
	cases := map[string]struct {
		next WorkGraph
		want string
	}{
		"a cycle is refused before execution": {next: raw(t, 2, withCycle), want: "form a cycle"},
		"an activated unit may not be removed": {
			next: composed(t, 2, diamond()[:1]), want: `"b" has already been activated, so revision 2 may not remove it`,
		},
		"an activated unit may not change its issue": {
			next: composed(t, 2, mutate(diamond(), func(units []WorkUnit) { units[1].Issue = 999 })),
			want: `"b" has already been activated, so revision 2 may not change`,
		},
		"an activated unit may not gain a dependency": {
			next: composed(t, 2, mutate(diamond(), func(units []WorkUnit) { units[1].DependsOn = []string{"a", "c"} })),
			want: `"b" has already been activated, so revision 2 may not change`,
		},
		"an activated unit may not change its role": {
			next: composed(t, 2, mutate(diamond(), func(units []WorkUnit) { units[1].Role = domain.RoleReviewer })),
			want: `"b" has already been activated, so revision 2 may not change`,
		},
		"a revision may not skip":   {next: composed(t, 3, diamond()), want: "the next revision is 2, not 3"},
		"a revision may not repeat": {next: composed(t, 1, diamond()), want: "the next revision is 2, not 1"},
		"a revision of another graph is not this graph's next": {
			next: func() WorkGraph {
				other := composedUnder(t, "acme/repo", "claude", "m2-o2")
				other.Revision = 2
				return other
			}(),
			want: "describes a different graph",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMutation(current, test.next, activated); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one containing %q", err, test.want)
			}
		})
	}

	// What a mutation MAY do: add units, and edit a unit nothing has activated.
	grown := append(diamond(), WorkUnit{ID: "e", Purpose: "land the docs",
		Role: domain.RoleImplementer, Issue: 105, DependsOn: []string{"d"}})
	if err := ValidateMutation(current, composed(t, 2, grown), activated); err != nil {
		t.Fatalf("adding a unit downstream of everything was refused: %v", err)
	}
	edited := mutate(diamond(), func(units []WorkUnit) { units[3].Issue = 444 })
	if err := ValidateMutation(current, composed(t, 2, edited), activated); err != nil {
		t.Fatalf("editing an unactivated unit was refused: %v", err)
	}
}

// raw builds a revision document WITHOUT validating it, which is the only way to
// hand ValidateMutation the kind of proposal a model could produce.
func raw(t *testing.T, revision int, units []WorkUnit) WorkGraph {
	t.Helper()
	id, err := WorkGraphID("acme/repo", "claude", "m2-o1")
	if err != nil {
		t.Fatal(err)
	}
	return WorkGraph{
		SchemaVersion: WorkGraphSchemaVersion, ID: id, Repository: "acme/repo", AgentID: "claude",
		Name: "m2-o1", Revision: revision, RequestedBy: "operator@example",
		CreatedAt: time.Unix(1700000000, 0).UTC(), Units: units,
	}
}

func mutate(units []WorkUnit, edit func([]WorkUnit)) []WorkUnit {
	edit(units)
	return units
}

// TestRevisionDigestIsOrderInsensitive: two documents that describe the SAME
// graph have the same content identity, however the proposer ordered the units
// or their dependency lists. Without it, re-emitting an unchanged graph in
// another order would read as a changed revision and be refused as a conflict.
func TestRevisionDigestIsOrderInsensitive(t *testing.T) {
	forward := composed(t, 1, diamond())
	baseline, err := forward.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}

	// REORDERED UNITS: d, c, b, a.
	reversed := diamond()
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	shuffled := composed(t, 1, reversed)
	digest, err := shuffled.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != baseline {
		t.Fatalf("reordering the units changed the revision identity: %s then %s", baseline, digest)
	}

	// REORDERED DEPENDENCIES: d depends on c, b rather than b, c.
	swapped := diamond()
	swapped[3].DependsOn = []string{"c", "b"}
	rewritten := composed(t, 1, swapped)
	digest, err = rewritten.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != baseline {
		t.Fatalf("reordering a dependency list changed the revision identity: %s then %s", baseline, digest)
	}

	// Canonicalizing is a question, not a mutation: the caller's own document
	// is left exactly as it was submitted, and that is what gets stored.
	if fmt.Sprint(rewritten.Units[3].DependsOn) != "[c b]" {
		t.Fatalf("asking for the digest reordered the caller's document: %v", rewritten.Units[3].DependsOn)
	}

	// And a REAL change still changes it.
	changed := diamond()
	changed[3].DependsOn = []string{"b"}
	altered := composed(t, 1, changed)
	digest, err = altered.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest == baseline {
		t.Fatal("dropping a dependency did not change the revision identity")
	}
}

// TestWorkUnitBatchIdentityIsPerUnitExecution is the #472 binding law: a graph
// unit's child batch is THIS unit's, against THESE inputs. Issue number alone
// never names it, so no unit can be satisfied by work performed for something
// else.
func TestWorkUnitBatchIdentityIsPerUnitExecution(t *testing.T) {
	inputs := WorkUnitInputs{{UnitID: "a", UnitOutput: UnitOutput{HandoffID: "handoff-a",
		RunID: "run-a", CandidateRevision: "c1", CandidateTree: "tree-c1"}}}
	origin := BatchOrigin{GraphID: "graph-1", UnitID: "b", Inputs: inputs}
	unit, err := WorkUnitBatchID("acme/repo", "claude", 102, origin)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := BatchID("acme/repo", "claude", []int{102})
	if err != nil {
		t.Fatal(err)
	}
	if unit == direct {
		t.Fatal("a work unit execution shares its batch identity with a direct orchestration of the same issue")
	}
	same, err := WorkUnitBatchID("acme/repo", "claude", 102, origin)
	if err != nil || same != unit {
		t.Fatalf("the same unit execution named two batches: %s then %s (%v)", unit, same, err)
	}
	replaced := WorkUnitInputs{{UnitID: "a", UnitOutput: UnitOutput{HandoffID: "handoff-a2",
		RunID: "run-a", CandidateRevision: "c9", CandidateTree: "tree-c9"}}}
	for name, other := range map[string]BatchOrigin{
		"another graph": {GraphID: "graph-2", UnitID: "b", Inputs: inputs},
		"another unit":  {GraphID: "graph-1", UnitID: "c", Inputs: inputs},
		"other inputs":  {GraphID: "graph-1", UnitID: "b", Inputs: replaced},
		"no inputs":     {GraphID: "graph-1", UnitID: "b"},
	} {
		id, err := WorkUnitBatchID("acme/repo", "claude", 102, other)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id == unit {
			t.Fatalf("%s names the same batch execution", name)
		}
	}
	// The input ORDER is not part of the identity; the set is.
	pair := WorkUnitInputs{inputs[0], replaced[0]}
	pair[1].UnitID = "z"
	flipped := WorkUnitInputs{pair[1], pair[0]}
	left, err := WorkUnitBatchID("acme/repo", "claude", 102, BatchOrigin{GraphID: "graph-1", UnitID: "b", Inputs: pair})
	if err != nil {
		t.Fatal(err)
	}
	right, err := WorkUnitBatchID("acme/repo", "claude", 102, BatchOrigin{GraphID: "graph-1", UnitID: "b", Inputs: flipped})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("one input set in two orders named two batches: %s then %s", left, right)
	}
	for name, broken := range map[string]BatchOrigin{
		"no graph": {UnitID: "b", Inputs: inputs},
		"no unit":  {GraphID: "graph-1", Inputs: inputs},
		"bad input": {GraphID: "graph-1", UnitID: "b",
			Inputs: WorkUnitInputs{{UnitID: "a", UnitOutput: UnitOutput{RunID: "run-a"}}}},
	} {
		if _, err := WorkUnitBatchID("acme/repo", "claude", 102, broken); err == nil {
			t.Errorf("an origin with %s named a batch", name)
		}
	}
}

// TestAnOriginBearingBatchDescribesOneUnitExecution keeps the stored document
// honest: a graph-owned batch performs one unit over one issue.
func TestAnOriginBearingBatchDescribesOneUnitExecution(t *testing.T) {
	origin := &BatchOrigin{GraphID: "graph-1", UnitID: "b", Inputs: WorkUnitInputs{{UnitID: "a",
		UnitOutput: UnitOutput{HandoffID: "h", RunID: "run-a", CandidateRevision: "c", CandidateTree: "t"}}}}
	id, err := WorkUnitBatchID("acme/repo", "claude", 102, *origin)
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{SchemaVersion: BatchSchemaVersion, ID: id, Repository: "acme/repo", AgentID: "claude",
		CreatedAt: time.Unix(1700000000, 0).UTC(), Origin: origin,
		Items: []BatchItem{{Issue: 102, RunID: "run-102"}}}
	if err := batch.Validate(); err != nil {
		t.Fatalf("a one-unit batch was refused: %v", err)
	}
	// Two issues under one unit origin is not one unit execution.
	twoIssues := batch
	twoIssues.Items = []BatchItem{{Issue: 102, RunID: "run-102"}, {Issue: 103, RunID: "run-103"}}
	if err := twoIssues.Validate(); err == nil || !strings.Contains(err.Error(), "a unit performs one") {
		t.Fatalf("err = %v, want a refusal of a multi-issue unit batch", err)
	}
	// The origin is part of the identity: stripping it leaves a document that
	// does not match its own id.
	stripped := batch
	stripped.Origin = nil
	if err := stripped.Validate(); err == nil || !strings.Contains(err.Error(), "does not match the identity") {
		t.Fatalf("err = %v, want a refusal of a batch whose origin was removed", err)
	}
	// A DIRECT batch still validates exactly as it did before origins existed.
	directID, err := BatchID("acme/repo", "claude", []int{102})
	if err != nil {
		t.Fatal(err)
	}
	direct := batch
	direct.ID, direct.Origin = directID, nil
	if err := direct.Validate(); err != nil {
		t.Fatalf("a direct batch was refused: %v", err)
	}
}

// TestTheDocumentBoundIsWhatTheTransportCarries: a graph this package accepts is
// a graph an operator can actually submit. The per-field bounds alone cannot say
// that, so the document itself is bounded.
func TestTheDocumentBoundIsWhatTheTransportCarries(t *testing.T) {
	wide := make([]WorkUnit, MaxWorkGraphUnits)
	for i := range wide {
		wide[i] = WorkUnit{ID: fmt.Sprintf("unit-%02d", i), Role: domain.RoleImplementer, Issue: 500 + i,
			Purpose: strings.Repeat("x", maxUnitPurposeBytes)}
	}
	_, err := WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: wide}.
		Compose("acme/repo", "claude", "operator@example", time.Unix(1700000000, 0).UTC())
	if err == nil || !strings.Contains(err.Error(), "one control request carries") {
		t.Fatalf("err = %v, want a refusal of an untransportable graph", err)
	}
	// A graph within the bound is accepted, and its document really is within
	// it - measured, not assumed.
	narrow := make([]WorkUnit, MaxWorkGraphUnits)
	for i := range narrow {
		narrow[i] = WorkUnit{ID: fmt.Sprintf("unit-%02d", i), Role: domain.RoleImplementer, Issue: 500 + i,
			Purpose: "land the thing"}
	}
	graph, err := WorkGraphProposal{Name: "m2-o1", Revision: 1, Units: narrow}.
		Compose("acme/repo", "claude", "operator@example", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("a graph of %d small units was refused: %v", MaxWorkGraphUnits, err)
	}
	size, err := graph.documentBytes()
	if err != nil {
		t.Fatal(err)
	}
	if size > MaxWorkGraphDocumentBytes {
		t.Fatalf("an accepted graph's document is %d bytes, above the %d byte bound", size, MaxWorkGraphDocumentBytes)
	}
}
