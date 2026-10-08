// Package integration owns #475: the deterministic responsibility for
// composing parallel WorkGraph (#472) outputs into one exact integration
// candidate.
//
// It states which exact upstream EngineeringHandoff outputs one integration
// attempt consumes, the base revision it starts from, and the order
// composition must follow - never which agent performs it, never a second
// scheduler, run database, policy engine or authority, and never a second
// shape for a fact #472 already owns. Contract.Inputs IS
// orchestration.WorkUnitInputs: an integration WorkUnit's consumed inputs are
// a WorkGraph unit's inputs, reused unchanged.
//
// This package is the functional core: it imports no runtime, store or Git
// package, so nothing here can run a command, open a file or hold a lock.
// The runtime package's IntegrateInputs is the imperative shell that actually
// performs the Git composition this package only states and classifies.
package integration

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// SchemaVersion versions the durable contract document.
const SchemaVersion = "0.1"

// MaxInputs bounds one integration attempt's consumed input set. It reuses
// the WorkGraph's own unit bound: an integration attempt's inputs are a
// subset of one graph's units, never a larger fan-in the rest of the system
// would itself refuse to construct.
const MaxInputs = orchestration.MaxWorkGraphUnits

// Contract names exactly what one integration attempt composes: which
// WorkGraph unit it is, the base revision it starts from, and the admitted
// upstream outputs it consumes. It carries no credential and no remote:
// composing it is bounded Git plumbing inside a runtime-owned workspace, not
// an execution or authority decision.
type Contract struct {
	SchemaVersion string `json:"schema_version"`
	GraphID       string `json:"graph_id"`
	UnitID        string `json:"unit_id"`
	// BaseRevision is the base this attempt starts from. The runtime verifies
	// it, at composition time, as an ancestor of every input it merges - see
	// runtime.IntegrateInputs. This package only requires that one be named.
	BaseRevision string                       `json:"base_revision"`
	Inputs       orchestration.WorkUnitInputs `json:"inputs"`
}

// NewContract composes and validates a contract in one step.
func NewContract(graphID, unitID, baseRevision string, inputs orchestration.WorkUnitInputs) (Contract, error) {
	c := Contract{
		SchemaVersion: SchemaVersion, GraphID: graphID, UnitID: unitID,
		BaseRevision: baseRevision, Inputs: inputs,
	}
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}
	return c, nil
}

// Validate refuses a contract that does not name one bounded, composable
// integration attempt.
func (c Contract) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("integration contract schema version %q is not %q", c.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(c.GraphID) == "" || strings.TrimSpace(c.UnitID) == "" {
		return errors.New("an integration contract names its graph and its unit")
	}
	if strings.TrimSpace(c.BaseRevision) == "" {
		return errors.New("an integration contract names the base revision it starts from")
	}
	if err := c.Inputs.Validate(); err != nil {
		return err
	}
	// FEWER THAN TWO INPUTS is not a composition - it is one unit's output
	// passed through, which is already #472's ordinary dependency delivery
	// and needs no integration attempt of its own.
	if len(c.Inputs) < 2 {
		return fmt.Errorf("an integration contract composes at least two upstream outputs, got %d", len(c.Inputs))
	}
	if len(c.Inputs) > MaxInputs {
		return fmt.Errorf("integration contract names %d inputs, above the %d input bound", len(c.Inputs), MaxInputs)
	}
	return nil
}

// Digest is this contract's content identity: the graph, the unit, the base
// revision and the exact input SET. It is independent of arrival order -
// WorkUnitInputs.Digest already canonicalizes by consumed unit id - so the
// same two upstream outputs always identify the same integration attempt
// however a caller happened to assemble or receive them.
func (c Contract) Digest() (string, error) {
	inputsDigest, err := c.Inputs.Digest()
	if err != nil {
		return "", err
	}
	return domain.Digest(struct {
		Graph  string `json:"graph"`
		Unit   string `json:"unit"`
		Base   string `json:"base"`
		Inputs string `json:"inputs"`
	}{c.GraphID, c.UnitID, c.BaseRevision, inputsDigest})
}

// MergeStep is one ordered merge this contract requires: one upstream unit's
// exact admitted commit and tree.
type MergeStep struct {
	UnitID string `json:"unit_id"`
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	// HandoffID is the admitted handoff this step's commit and tree were
	// recorded against. A caller re-verifies it against the unit's LIVE
	// current admitted handoff before fetching the commit at all: a commit
	// that remains fetchable is not evidence that it is still current, and
	// a mismatch here is what proves it is not.
	HandoffID string `json:"handoff_id"`
}

// Plan is the deterministic composition order: by consumed unit id, which is
// WorkUnitInputs' own canonical key.
//
// Two contracts naming the same input set therefore always plan the same
// sequence of merges, whatever order the two producers finished in, and a
// restarted attempt replans identically rather than depending on the order
// this process happened to observe them in this time.
func (c Contract) Plan() []MergeStep {
	ordered := append(orchestration.WorkUnitInputs(nil), c.Inputs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].UnitID < ordered[j].UnitID })
	steps := make([]MergeStep, len(ordered))
	for i, in := range ordered {
		steps[i] = MergeStep{UnitID: in.UnitID, Commit: in.CandidateRevision, Tree: in.CandidateTree, HandoffID: in.HandoffID}
	}
	return steps
}
