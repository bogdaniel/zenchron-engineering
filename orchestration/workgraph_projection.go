package orchestration

// Graph progression, projected. Nothing in this file is stored: a unit's state
// is recomputed from the durable graph document, the durable activation records
// and the child runs on every read, so it cannot drift from the runtime that is
// the authority for all three, and a restart reproduces the identical frontier.
//
// Provider kind is absent from every decision here, and so is provider exit. A
// unit is satisfied by an ADMITTED handoff bound to an exact candidate, and by
// nothing else.

import (
	"fmt"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// UnitState is one work unit's state. The four graph-owned states below are
// joined by the whole #470 ItemState vocabulary for an activated unit: the
// graph does not rename what the child run already says it is.
type UnitState string

const (
	// UnitBlocked is a unit at least one of whose dependencies is not
	// satisfied. Its reason names the dependency and says whether anything
	// could still satisfy it.
	UnitBlocked UnitState = "blocked"
	// UnitReady is a runnable unit: every dependency is satisfied by an
	// admitted output, and no child run has been claimed for it yet. The
	// existing scheduler, not this state, decides when that child executes.
	UnitReady UnitState = "ready"
	// UnitUnknown is an activated unit whose child run could not be read. It
	// is never satisfied, and it never hides its siblings: one unreadable
	// child blocks what depends on it and nothing else.
	UnitUnknown UnitState = "unknown"
	// UnitInvalidated is an activated unit whose dependencies no longer
	// present the exact outputs it was activated against - because an upstream
	// output was replaced, or because an upstream unit stopped being
	// satisfied. Satisfaction is NOT "once completed, always completed", and
	// an invalidated unit is reported as invalid rather than carried forward.
	//
	// It is terminal for this graph: it is not in the frontier, and nothing
	// downstream of it is. Re-performing invalidated work is remediation, and
	// remediation is deliberately not in #472.
	UnitInvalidated UnitState = "invalidated"
)

// UnitOutput is the exact subject a unit's admitted handoff bound. It is what
// makes a downstream unit's satisfaction checkable against a replacement.
type UnitOutput struct {
	HandoffID         string `json:"handoff_id"`
	CandidateRevision string `json:"candidate_revision"`
	CandidateTree     string `json:"candidate_tree"`
}

// UnitFacts are the durable facts one unit is projected from. The runtime
// supplies them: the activation record it wrote, the child item state #470
// already projects, and the admitted handoff that child transferred.
type UnitFacts struct {
	// Activated is whether this graph has durably claimed a child run for the
	// unit. A unit is activated exactly once.
	Activated bool
	// ActivationInputsDigest is the digest of the upstream outputs recorded
	// when the unit was activated.
	ActivationInputsDigest string
	// Item is the child's projected state. Required when Activated unless
	// Unreadable.
	Item ItemState
	// Terminal is whether the child RUN has ended. A terminal child produces
	// nothing further, so one that is not satisfied never will be - which is
	// not visible from the item state alone: handoff_pending and partial both
	// occur on a live child that may still transfer an admitted handoff.
	Terminal bool
	// Unreadable is set when the child's facts could not be read at all.
	Unreadable bool
	// UnreadableReason says why.
	UnreadableReason string
	// Output is the subject of the admitted handoff behind a completed child.
	Output *UnitOutput
}

// UnitProjection is one unit's decided state.
type UnitProjection struct {
	State  UnitState `json:"state"`
	Reason string    `json:"reason,omitempty"`
	// InputsDigest is the digest of the upstream outputs this unit would be,
	// or was correctly, activated against. It is empty while a dependency has
	// no admitted output.
	InputsDigest string `json:"inputs_digest,omitempty"`
}

// WorkGraphProjection is the whole graph decided in one pass.
type WorkGraphProjection struct {
	Units map[string]UnitProjection `json:"units"`
	// Frontier is every runnable unit, ascending. It is the answer to "which
	// units are runnable", not to "which of them executes now".
	Frontier []string `json:"frontier,omitempty"`
}

// unitInput is one consumed upstream output, in the digest's canonical form.
type unitInput struct {
	Unit              string `json:"unit"`
	CandidateRevision string `json:"candidate_revision"`
	CandidateTree     string `json:"candidate_tree"`
}

// ProjectWorkGraph decides every unit's state and the runnable frontier.
//
// It walks the units dependency-first, so each unit is decided after everything
// it consumes. A child state this build does not understand is refused onto
// `unknown` rather than guessed: a frontier computed past it could start work
// nothing durable says is ready, and nothing downstream of it proceeds - but
// every other branch does.
func ProjectWorkGraph(graph WorkGraph, facts map[string]UnitFacts) (WorkGraphProjection, error) {
	order, err := topologicalOrder(graph.Units)
	if err != nil {
		return WorkGraphProjection{}, err
	}
	byID := make(map[string]WorkUnit, len(graph.Units))
	for _, unit := range graph.Units {
		byID[unit.ID] = unit
	}
	projection := WorkGraphProjection{Units: make(map[string]UnitProjection, len(graph.Units))}
	satisfied, dead := map[string]bool{}, map[string]bool{}
	for _, id := range order {
		unit, fact := byID[id], facts[id]
		digest, reason, settled, err := currentInputs(unit, facts, satisfied, dead, projection.Units)
		if err != nil {
			return WorkGraphProjection{}, err
		}
		// Read only where the switch below selects it: an unactivated unit has
		// no child state, and an unreadable one has none to interpret.
		state, stateErr := unitStateOf(fact.Item)
		switch {
		case !fact.Activated && reason != "":
			projection.Units[id] = UnitProjection{State: UnitBlocked, Reason: reason}
		case !fact.Activated:
			projection.Units[id] = UnitProjection{State: UnitReady, InputsDigest: digest}
			projection.Frontier = append(projection.Frontier, id)
		case reason != "":
			projection.Units[id] = UnitProjection{State: UnitInvalidated,
				Reason: "activated against upstream output that no longer holds: " + reason}
		case fact.Unreadable:
			projection.Units[id] = UnitProjection{State: UnitUnknown, Reason: fact.UnreadableReason}
		case stateErr != nil:
			// REFUSED, not guessed - and refused on this unit alone, so a
			// state a newer binary wrote blocks what depends on it and leaves
			// every other branch to proceed.
			projection.Units[id] = UnitProjection{State: UnitUnknown, Reason: stateErr.Error()}
		case fact.ActivationInputsDigest != digest:
			projection.Units[id] = UnitProjection{State: UnitInvalidated,
				Reason: "an upstream output was replaced after this unit was activated against it"}
		default:
			if fact.Item == ItemCompleted && fact.Output == nil {
				// A completed child transferred an ADMITTED handoff by
				// definition, so facts without its subject are incoherent,
				// not a unit to carry forward as satisfied.
				return WorkGraphProjection{}, fmt.Errorf("work unit %q is completed but names no admitted output", id)
			}
			projection.Units[id] = UnitProjection{State: state, InputsDigest: digest}
			satisfied[id] = fact.Item == ItemCompleted
		}
		// A unit nothing could still satisfy, decided once, and PROPAGATED: its
		// own state says so, its child run ENDED without satisfying it, or it
		// waits on a dependency that is itself dead. Without the last two a
		// dead branch would read to an operator as one still worth waiting
		// for - the terminal case is invisible in the state alone, and the
		// transitive case is what makes a whole branch below a failure honest.
		dead[id] = settled || neverSatisfiable(projection.Units[id].State) ||
			(fact.Activated && fact.Terminal && !satisfied[id])
	}
	sort.Strings(projection.Frontier)
	return projection, nil
}

// currentInputs is the digest of the outputs this unit's dependencies present
// RIGHT NOW, or the reason one of them presents none and whether that reason is
// permanent.
func currentInputs(unit WorkUnit, facts map[string]UnitFacts, satisfied, dead map[string]bool, decided map[string]UnitProjection) (string, string, bool, error) {
	// A unit with no dependencies still records a digest - the digest of an
	// empty input list - so that every activation has one to be compared
	// against, and a unit whose dependencies were somehow replaced by none
	// would not silently match.
	inputs := make([]unitInput, 0, len(unit.DependsOn))
	var waiting, settled string
	for _, dependency := range sortedCopy(unit.DependsOn) {
		if !satisfied[dependency] {
			// EVERY unsatisfied dependency is examined, not just the first.
			// A dependency that can never be satisfied is the fact that
			// matters, and reporting an alphabetically earlier sibling that is
			// merely running would describe a dead branch as a live one.
			state := decided[dependency].State
			if waiting == "" {
				waiting = unsatisfiedReason(dependency, state, dead[dependency])
			}
			if settled == "" && dead[dependency] {
				settled = unsatisfiedReason(dependency, state, true)
			}
			continue
		}
		output := facts[dependency].Output
		inputs = append(inputs, unitInput{Unit: dependency,
			CandidateRevision: output.CandidateRevision, CandidateTree: output.CandidateTree})
	}
	if settled != "" {
		return "", settled, true, nil
	}
	if waiting != "" {
		return "", waiting, false, nil
	}
	digest, err := domain.Digest(inputs)
	return digest, "", false, err
}

// neverSatisfiable is whether a unit in this state could still transfer an
// admitted handoff, from the state ALONE. A terminal child that is not
// satisfied is also dead, which the state does not say; the projection records
// both together.
func neverSatisfiable(state UnitState) bool {
	switch state {
	case UnitState(ItemFailed), UnitState(ItemStopped), UnitInvalidated:
		return true
	}
	return false
}

// unsatisfiedReason says what a dependency is, and whether anything could still
// change it. A terminal child will never produce an admitted handoff, and
// telling an operator it is merely "waiting" would be a lie about a dead branch.
func unsatisfiedReason(dependency string, state UnitState, dead bool) string {
	if dead {
		return fmt.Sprintf("dependency %q is %s and will never transfer an admitted handoff", dependency, state)
	}
	return fmt.Sprintf("dependency %q is %s and has transferred no admitted handoff yet", dependency, state)
}

// WorkGraphCounts is the aggregate a whole graph is read through.
type WorkGraphCounts struct {
	Total       int `json:"total"`
	Blocked     int `json:"blocked"`
	Ready       int `json:"ready"`
	Invalidated int `json:"invalidated"`
	Unknown     int `json:"unknown"`
	// Activated counts every unit whose child run this graph has claimed,
	// through the same #470 item vocabulary the child is projected in. Its own
	// Total is therefore the number of activated units, not of all units.
	Activated Counts `json:"activated"`
}

// Add counts one unit.
func (c *WorkGraphCounts) Add(state UnitState) {
	c.Total++
	switch state {
	case UnitBlocked:
		c.Blocked++
	case UnitReady:
		c.Ready++
	case UnitInvalidated:
		c.Invalidated++
	case UnitUnknown:
		c.Unknown++
	default:
		c.Activated.Add(ItemState(state))
	}
}

// unitStateOf carries an activated unit's child state into the unit vocabulary
// unchanged, and refuses one this build does not know.
func unitStateOf(item ItemState) (UnitState, error) {
	switch item {
	case ItemNotCreated, ItemQueued, ItemRunning, ItemWaiting, ItemHandoffPending,
		ItemCompleted, ItemPartial, ItemFailed, ItemStopped:
		return UnitState(item), nil
	}
	return "", fmt.Errorf("unrecognized child item state %q", item)
}
