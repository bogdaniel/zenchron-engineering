package runtime

// Durable, authorized DecisionResolution (#508): the governed control-plane
// actions that resolve a live decision request, and that place a WorkGraph
// unit hold in the first place. Neither one starts a provider, and neither
// activates a WorkUnit directly - #472's existing frontier recomputation
// decides that on its own next read, exactly as it always has.
//
// Two different durable facts can need resolving, and
// SQLiteOperationStore.ResolveDecisionRequest (decision_store.go) is what
// lets ONE action resolve either without the caller needing to know which
// kind it named:
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

// ResolveDecision is the governed external-authority action. It builds the
// actor's authority from the existing operator identity the control endpoint
// already established for this request, then delegates finding the live
// request, validating it, and persisting the immutable resolution to
// SQLiteOperationStore.ResolveDecisionRequest as ONE linearized database
// operation (#508 review P2): nothing can supersede the request, move its
// subject, or write a competing resolution between the check this action
// relies on and the commit that makes it durable.
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
	outcome := orchestration.DecisionOutcome{Kind: request.DecisionOutcomeKind, Value: request.DecisionOutcomeValue}
	authority := orchestration.DecisionResolutionAuthority{
		Actor: operator, AuthorityKind: orchestration.AuthorityKindOperator, Provenance: decisionProvenance(request),
	}
	stored, err := s.deps.Store.ResolveDecisionRequest(decisionID, outcome, BoundedNote(request.Note), authority, s.deps.Clock.Now())
	if err != nil {
		return DecisionResolutionView{}, err
	}
	return DecisionResolutionView{Resolution: stored}, nil
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
	// THE SAME LOCK activateGraphFrontier holds across reading holds,
	// activating a unit, AND adopting a graph revision (AdoptWorkGraph,
	// workgraph.go). #508 review P3: a revision adopted between this
	// method's OWN membership check and its hold insertion could remove or
	// repoint the unit out from under a check already passed, exactly as an
	// activation could - so BOTH the graph/unit membership check and the
	// not-yet-activated check are taken fresh, inside this one critical
	// section, against the CURRENT revision and the CURRENT activations,
	// never a snapshot read before the lock was held.
	var stored orchestration.WorkUnitHold
	err = func() error {
		s.orchestrationMu.Lock()
		defer s.orchestrationMu.Unlock()
		graph, found, err := s.deps.Store.WorkGraph(graphID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("unknown work graph %s", graphID)
		}
		named := false
		for _, unit := range graph.Units {
			if unit.ID == unitID {
				named = true
				break
			}
		}
		if !named {
			return fmt.Errorf("work graph %s names no unit %s", graphID, unitID)
		}
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
