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
	"strings"

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
	// UnitAwaitingDecision is a unit whose dependencies are all satisfied but
	// which a readiness owner outside the graph is holding on an unresolved
	// decision (#508). It is not runnable, and the graph neither makes nor
	// resolves the decision.
	UnitAwaitingDecision UnitState = "awaiting_decision"
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
// makes a downstream unit's satisfaction checkable against a replacement, and
// what a downstream unit's execution is given.
type UnitOutput struct {
	HandoffID         string `json:"handoff_id"`
	RunID             string `json:"run_id"`
	CandidateRevision string `json:"candidate_revision"`
	CandidateTree     string `json:"candidate_tree"`
	// Outcome, Summary, Unresolved and RecommendedNext are the producer's own
	// report, carried because that report IS the handoff: a downstream unit
	// given only a commit has the change and not what the producer said about
	// it. Every one of them is worker-authored, and whatever renders them
	// treats them as untrusted data.
	Outcome         string   `json:"outcome"`
	Summary         string   `json:"summary"`
	Unresolved      []string `json:"unresolved,omitempty"`
	RecommendedNext []string `json:"recommended_next,omitempty"`
}

// WorkUnitInput is one consumed upstream output, as the unit that consumes it
// was activated against: WHICH producer, and exactly what it transferred. It is
// the durable, readable input set a child run's execution is reconstructed from,
// not a digest of it.
//
// The output is EMBEDDED rather than restated. "What an admitted handoff
// transferred" is one shape, and two copies of it would be two places for a
// field to be added to and one place for it to be forgotten.
type WorkUnitInput struct {
	UnitID string `json:"unit_id"`
	UnitOutput
}

// WorkUnitInputs is one unit's whole consumed input set, in canonical order.
type WorkUnitInputs []WorkUnitInput

// Validate refuses an input that cannot name the exact output it consumed.
func (inputs WorkUnitInputs) Validate() error {
	if len(inputs) > MaxWorkGraphUnits {
		return fmt.Errorf("a work unit consumes %d inputs, above the %d input bound", len(inputs), MaxWorkGraphUnits)
	}
	seen := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		for name, value := range map[string]string{
			"unit_id": input.UnitID, "run_id": input.RunID, "handoff_id": input.HandoffID,
			"candidate_revision": input.CandidateRevision, "candidate_tree": input.CandidateTree,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("work unit input from %q is missing its %s", input.UnitID, name)
			}
		}
		if seen[input.UnitID] {
			return fmt.Errorf("work unit input from %q is named more than once", input.UnitID)
		}
		seen[input.UnitID] = true
	}
	return nil
}

// Digest is the input set's content identity: what an activation records, and
// what a later pass compares the dependencies' CURRENT outputs against.
func (inputs WorkUnitInputs) Digest() (string, error) {
	return domain.Digest(inputs.canonical())
}

// canonical sorts by consumed unit id, so the digest describes the SET rather
// than the order a projection happened to walk it in.
func (inputs WorkUnitInputs) canonical() WorkUnitInputs {
	out := append(WorkUnitInputs(nil), inputs...)
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	for i := range out {
		out[i].Unresolved = append([]string(nil), out[i].Unresolved...)
		out[i].RecommendedNext = append([]string(nil), out[i].RecommendedNext...)
	}
	return out
}

// DecisionWait is a readiness hold a unit is under from an owner OUTSIDE this
// graph: an unresolved decision the work needs before it may run.
//
// It is a seam, deliberately. #472 owns no decision record, no authority and no
// persistence for one - #508 does - and nothing here can resolve a hold. The
// graph's only obligation is to REPRESENT it: a held unit is not runnable and
// never enters the frontier, and the moment its owner stops reporting the hold
// the ordinary frontier computation includes it again. There is no path by
// which a worker answers its own hold.
type DecisionWait struct {
	// Reference is the holding owner's opaque identity for the decision. The
	// graph stores none of it and interprets none of it.
	Reference string `json:"reference"`
	Detail    string `json:"detail,omitempty"`
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
	// AwaitingDecision is a readiness hold reported by an owner outside this
	// graph. A held unit is never runnable; see DecisionWait.
	AwaitingDecision *DecisionWait
}

// UnitProjection is one unit's decided state.
type UnitProjection struct {
	State  UnitState `json:"state"`
	Reason string    `json:"reason,omitempty"`
	// InputsDigest is the digest of the upstream outputs this unit would be,
	// or was correctly, activated against. It is empty while a dependency has
	// no admitted output.
	InputsDigest string `json:"inputs_digest,omitempty"`
	// Inputs is that same input set, readable. An activation records it so the
	// child run's execution can be given the exact outputs it consumes rather
	// than only a digest proving which ones they were.
	Inputs WorkUnitInputs `json:"inputs,omitempty"`
	// AwaitingDecision is the readiness hold behind UnitAwaitingDecision.
	AwaitingDecision *DecisionWait `json:"awaiting_decision,omitempty"`
}

// WorkGraphProjection is the whole graph decided in one pass.
type WorkGraphProjection struct {
	Units map[string]UnitProjection `json:"units"`
	// Frontier is every runnable unit, ascending. It is the answer to "which
	// units are runnable", not to "which of them executes now".
	Frontier []string `json:"frontier,omitempty"`
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
		inputs, reason, settled, err := currentInputs(unit, facts, satisfied, dead, projection.Units)
		if err != nil {
			return WorkGraphProjection{}, err
		}
		digest := ""
		if reason == "" {
			if digest, err = inputs.Digest(); err != nil {
				return WorkGraphProjection{}, err
			}
		}
		// Read only where the switch below selects it: an unactivated unit has
		// no child state, and an unreadable one has none to interpret.
		state, stateErr := unitStateOf(fact.Item)
		switch {
		case !fact.Activated && reason != "":
			projection.Units[id] = UnitProjection{State: UnitBlocked, Reason: reason}
		case !fact.Activated && fact.AwaitingDecision != nil:
			// EVERY dependency is satisfied and the work is still held. It is
			// NOT in the frontier, so nothing activates it, and the hold's
			// owner is the only thing that can lift it.
			projection.Units[id] = UnitProjection{State: UnitAwaitingDecision,
				Reason: boundedHoldReason(*fact.AwaitingDecision), InputsDigest: digest, Inputs: inputs,
				AwaitingDecision: fact.AwaitingDecision}
		case !fact.Activated:
			projection.Units[id] = UnitProjection{State: UnitReady, InputsDigest: digest, Inputs: inputs}
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
			projection.Units[id] = UnitProjection{State: state, InputsDigest: digest, Inputs: inputs}
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

// currentInputs is the exact set of outputs this unit's dependencies present
// RIGHT NOW, or the reason one of them presents none and whether that reason is
// permanent.
func currentInputs(unit WorkUnit, facts map[string]UnitFacts, satisfied, dead map[string]bool, decided map[string]UnitProjection) (WorkUnitInputs, string, bool, error) {
	// A unit with no dependencies still has an input set - the empty one, whose
	// digest is a real digest - so that every activation records one to be
	// compared against, and a unit whose dependencies were somehow replaced by
	// none would not silently match.
	inputs := make(WorkUnitInputs, 0, len(unit.DependsOn))
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
		inputs = append(inputs, WorkUnitInput{UnitID: dependency, UnitOutput: *facts[dependency].Output})
	}
	if settled != "" {
		return nil, settled, true, nil
	}
	if waiting != "" {
		return nil, waiting, false, nil
	}
	return inputs.canonical(), "", false, nil
}

// boundedHoldReason states a hold in the graph's own words, with the owner's
// opaque reference and detail quoted as the untrusted text they are.
func boundedHoldReason(hold DecisionWait) string {
	detail := strings.TrimSpace(hold.Detail)
	if len(detail) > maxHoldDetailBytes {
		detail = detail[:maxHoldDetailBytes]
	}
	if detail == "" {
		return fmt.Sprintf("held on unresolved decision %q", hold.Reference)
	}
	return fmt.Sprintf("held on unresolved decision %q: %s", hold.Reference, detail)
}

// maxHoldDetailBytes bounds how much of a hold owner's explanation is repeated.
const maxHoldDetailBytes = 200

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
	// AwaitingDecision counts units a readiness owner outside the graph is
	// holding on an unresolved decision (#508).
	AwaitingDecision int `json:"awaiting_decision"`
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
	case UnitAwaitingDecision:
		c.AwaitingDecision++
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
