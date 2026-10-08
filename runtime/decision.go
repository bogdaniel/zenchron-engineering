package runtime

// Durable, authorized DecisionResolution (#508): the governed control-plane
// actions that resolve a live decision request, and that place a WorkGraph
// unit hold in the first place. Neither one starts a provider, and neither
// activates a WorkUnit directly - #472's existing frontier recomputation
// decides that on its own next read, exactly as it always has.
//
// Two different durable facts can need resolving, and findDecisionRequest is
// what lets ONE action resolve either without the caller needing to know
// which kind it named:
//
//   - a #473 decision_request MESSAGE, a worker asking a question from
//     inside its own run (found by id in the message store, scoped to its
//     batch);
//   - a #508 WorkUnitHold, an operator's gate on a WorkGraph unit before its
//     first activation (found by id in the hold store, never scoped to a
//     batch because the unit has no run yet).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// DecisionResolutionView is the governed answer to a decision-resolve action:
// durable state re-read after the attempt, never the request echoed back.
type DecisionResolutionView struct {
	Resolution orchestration.DecisionResolution `json:"resolution"`
}

// ResolveDecision is the governed external-authority action. It finds the
// live request, builds the actor's authority from the existing operator
// identity the control endpoint already established for this request, and
// persists the immutable resolution before anything downstream of it is
// told.
//
// There is no path here for a worker to supply its own authority: Authority
// is built from request.Operator and hardcoded to AuthorityKindOperator,
// never from anything a message report could carry, and a request naming no
// operator is refused outright.
func (s *Supervisor) ResolveDecision(request ControlRequest) (DecisionResolutionView, error) {
	operator := strings.TrimSpace(request.Operator)
	if operator == "" {
		return DecisionResolutionView{}, errors.New("a decision resolution needs its authorized operator identity")
	}
	decisionID := strings.TrimSpace(request.DecisionID)
	if decisionID == "" {
		return DecisionResolutionView{}, errors.New("a decision resolution names the request it resolves")
	}
	ref, current, err := s.findDecisionRequest(decisionID)
	if err != nil {
		return DecisionResolutionView{}, err
	}
	outcome := orchestration.DecisionOutcome{Kind: request.DecisionOutcomeKind, Value: request.DecisionOutcomeValue}
	authority := orchestration.DecisionResolutionAuthority{
		Actor: operator, AuthorityKind: orchestration.AuthorityKindOperator, Provenance: decisionProvenance(request),
	}
	reason := BoundedNote(request.Note)
	existing, found, err := s.deps.Store.DecisionResolutionByRequestID(ref.ID)
	if err != nil {
		return DecisionResolutionView{}, err
	}
	var existingPtr *orchestration.DecisionResolution
	if found {
		existingPtr = &existing
	}
	proposed, err := orchestration.ResolveDecision(ref, current, outcome, reason, authority, existingPtr, s.deps.Clock.Now())
	if err != nil {
		return DecisionResolutionView{}, err
	}
	stored, inserted, err := s.deps.Store.InsertDecisionResolution(proposed)
	if err != nil {
		return DecisionResolutionView{}, err
	}
	if !inserted {
		// A concurrent writer landed between the read above and this insert.
		// Decide against what is ACTUALLY durable now, not what was read a
		// moment ago: the stored row, not this request, is the one answer
		// that may stand.
		final, err := orchestration.ResolveDecision(ref, current, outcome, reason, authority, &stored, s.deps.Clock.Now())
		if err != nil {
			return DecisionResolutionView{}, err
		}
		stored = final
	}
	return DecisionResolutionView{Resolution: stored}, nil
}

// findDecisionRequest resolves one request id to its normalized facts,
// trying the message store first and the hold store second. Neither store
// naming it is the unknown-request refusal: fail closed, not "probably a
// hold".
func (s *Supervisor) findDecisionRequest(id string) (orchestration.DecisionRequestRef, *orchestration.HandoffSubject, error) {
	message, found, err := s.deps.Store.MessageByID(id)
	if err != nil {
		return orchestration.DecisionRequestRef{}, nil, err
	}
	if found {
		return s.decisionRequestFromMessage(message)
	}
	hold, found, err := s.deps.Store.WorkUnitHoldByID(id)
	if err != nil {
		return orchestration.DecisionRequestRef{}, nil, err
	}
	if !found {
		return orchestration.DecisionRequestRef{}, nil, fmt.Errorf("unknown decision request %s", id)
	}
	return hold.Ref(), nil, nil
}

func (s *Supervisor) decisionRequestFromMessage(message orchestration.EngineeringMessage) (orchestration.DecisionRequestRef, *orchestration.HandoffSubject, error) {
	if message.Kind != orchestration.KindDecisionRequest {
		return orchestration.DecisionRequestRef{}, nil, fmt.Errorf("%s is a %s, not a decision request", message.ID, message.Kind)
	}
	batch, found, err := s.deps.Store.OrchestrationBatch(message.Scope)
	if err != nil {
		return orchestration.DecisionRequestRef{}, nil, err
	}
	if !found {
		return orchestration.DecisionRequestRef{}, nil, fmt.Errorf("decision request %s names scope %s, which is unreadable", message.ID, message.Scope)
	}
	scope, current, err := batchMessageScope(s.deps.Store, batch)
	if err != nil {
		return orchestration.DecisionRequestRef{}, nil, err
	}
	live := false
	for _, admitted := range orchestration.Live(scope.Admitted) {
		if admitted.ID == message.ID {
			live = true
			break
		}
	}
	ref := orchestration.DecisionRequestRef{ID: message.ID, Scope: message.Scope, Subject: message.Subject, Live: live}
	if message.Subject == nil {
		return ref, nil, nil
	}
	return ref, current[message.Subject.Owner], nil
}

// PlaceWorkUnitHold is the governed action that creates #472's readiness-owner
// seam in the first place: an operator - never a worker, since a unit has no
// run before its first activation - gates one WorkGraph unit until it is
// explicitly resolved. It is placed at most once per unit; a unit already
// activated has nothing left to gate.
func (s *Supervisor) PlaceWorkUnitHold(request ControlRequest) (orchestration.WorkUnitHold, error) {
	operator := strings.TrimSpace(request.Operator)
	if operator == "" {
		return orchestration.WorkUnitHold{}, errors.New("a work unit hold needs its authorized operator identity")
	}
	graphID, unitID := strings.TrimSpace(request.GraphID), strings.TrimSpace(request.UnitID)
	if graphID == "" || unitID == "" {
		return orchestration.WorkUnitHold{}, errors.New("a work unit hold names its graph and its unit")
	}
	graph, found, err := s.deps.Store.WorkGraph(graphID)
	if err != nil {
		return orchestration.WorkUnitHold{}, err
	}
	if !found {
		return orchestration.WorkUnitHold{}, fmt.Errorf("unknown work graph %s", graphID)
	}
	named := false
	for _, unit := range graph.Units {
		if unit.ID == unitID {
			named = true
			break
		}
	}
	if !named {
		return orchestration.WorkUnitHold{}, fmt.Errorf("work graph %s names no unit %s", graphID, unitID)
	}
	id, err := orchestration.WorkUnitHoldID(graphID, unitID)
	if err != nil {
		return orchestration.WorkUnitHold{}, err
	}
	hold := orchestration.WorkUnitHold{
		SchemaVersion: orchestration.WorkUnitHoldSchemaVersion, ID: id, GraphID: graphID, UnitID: unitID,
		Purpose: BoundedNote(request.Note),
		RequestedBy: orchestration.DecisionResolutionAuthority{
			Actor: operator, AuthorityKind: orchestration.AuthorityKindOperator, Provenance: decisionProvenance(request),
		},
		RequestedAt: s.deps.Clock.Now(),
	}
	// THE SAME LOCK activateGraphFrontier holds across reading holds and
	// activating a unit (#508 review B3): checking "not yet activated" and
	// placing the hold must be one atomic step against that section, or a
	// Tick racing between the two could activate the unit and leave an
	// accepted "pre-activation" hold attached to a unit that is no longer
	// pre-activation.
	var stored orchestration.WorkUnitHold
	err = func() error {
		s.orchestrationMu.Lock()
		defer s.orchestrationMu.Unlock()
		activations, err := s.deps.Store.WorkUnitActivations(graphID)
		if err != nil {
			return err
		}
		if _, activated := activations[unitID]; activated {
			return fmt.Errorf(
				"work unit %s of graph %s is already activated; a hold only gates a unit before its first activation", unitID, graphID)
		}
		stored, _, err = s.deps.Store.PlaceWorkUnitHold(hold)
		return err
	}()
	return stored, err
}

// decisionProvenance distinguishes a governed request - one that proved the
// serving controller's identity on this connection (#398) - from the plain
// local-owner-only control endpoint every other request already relies on.
// Both are the same authority kind; this only records which proof produced
// the actor.
func decisionProvenance(request ControlRequest) string {
	if strings.TrimSpace(request.ExpectedController) != "" {
		return "governed_control_endpoint"
	}
	return "local_control_endpoint"
}
