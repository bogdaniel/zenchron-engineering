package runtime

// Reading one WorkGraph (#472): the durable graph revision, the activations it
// has written, and each activated unit's child run projected exactly as #470
// already projects an orchestrated item. Nothing is stored and nothing is
// decided here - this is the read side, and it reads the same whether or not a
// supervisor is running, and the same after a restart.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// WorkGraphView is one graph read through its units.
type WorkGraphView struct {
	GraphID    string `json:"graph_id"`
	Repository string `json:"repository"`
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
	Revision   int    `json:"revision"`
	// RevisionDigest is the content identity of the revision this view read.
	RevisionDigest string                        `json:"revision_digest"`
	RequestedBy    string                        `json:"requested_by,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
	Counts         orchestration.WorkGraphCounts `json:"counts"`
	Frontier       []string                      `json:"frontier,omitempty"`
	Units          []WorkGraphUnitView           `json:"units"`
}

// WorkGraphUnitView is one unit: what it is, what it depends on, the state the
// graph projected for it, and the child run behind it when there is one.
type WorkGraphUnitView struct {
	UnitID    string                  `json:"unit_id"`
	Purpose   string                  `json:"purpose"`
	Role      domain.EngineeringRole  `json:"role"`
	Issue     int                     `json:"issue"`
	DependsOn []string                `json:"depends_on,omitempty"`
	State     orchestration.UnitState `json:"state"`
	Reason    string                  `json:"reason,omitempty"`
	// InputsDigest is the digest of the upstream outputs this unit would be,
	// or was correctly, activated against, and Inputs is that same set,
	// readable - what an activation records so the child run's execution can be
	// given the exact outputs it consumes.
	InputsDigest string                       `json:"inputs_digest,omitempty"`
	Inputs       orchestration.WorkUnitInputs `json:"inputs,omitempty"`
	// AwaitingDecision is a readiness hold from an owner outside the graph.
	AwaitingDecision *orchestration.DecisionWait `json:"awaiting_decision,omitempty"`
	RunID            string                      `json:"run_id,omitempty"`
	// Child is the child run's own #470 projection, when this unit is
	// activated. The graph does not restate what that view already says.
	Child *OrchestrationItemView `json:"child,omitempty"`
	// Output is the exact subject of the admitted handoff this unit produced.
	Output *orchestration.UnitOutput `json:"output,omitempty"`
}

// WorkGraphStatus projects one graph's current revision. A unit whose child
// cannot be read is reported as unknown on its own line and blocks only what
// depends on it.
// holds are readiness holds keyed by unit id, reported by an owner OUTSIDE this
// graph: the supervisor asks SupervisorDependencies.WorkUnitHolds and passes
// what it answered, and a read with no source - the CLI's, today - passes none.
// #472 owns no decision record, no authority and no persistence for one, so
// nothing supplies holds yet; #508 does.
func WorkGraphStatus(store *SQLiteOperationStore, stateDir, graphID string, now time.Time,
	holds map[string]orchestration.DecisionWait) (WorkGraphView, error) {
	graph, found, err := store.WorkGraph(graphID)
	if err != nil {
		return WorkGraphView{}, err
	}
	if !found {
		return WorkGraphView{}, fmt.Errorf("unknown work graph %q", graphID)
	}
	digest, err := graph.RevisionDigest()
	if err != nil {
		return WorkGraphView{}, err
	}
	tx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkGraphView{}, err
	}
	defer tx.Rollback()
	activations, err := queryWorkUnitActivations(tx, graph.ID)
	if err != nil {
		return WorkGraphView{}, err
	}
	facts := make(map[string]orchestration.UnitFacts, len(graph.Units))
	children := make(map[string]OrchestrationItemView, len(graph.Units))
	for _, unit := range graph.Units {
		activation, activated := activations[unit.ID]
		if !activated {
			if hold, held := holds[unit.ID]; held {
				facts[unit.ID] = orchestration.UnitFacts{AwaitingDecision: &hold}
			}
			continue
		}
		child := projectOrchestrationItem(tx, stateDir, orchestration.BatchItem{Issue: unit.Issue, RunID: activation.RunID}, now)
		children[unit.ID] = child
		fact := orchestration.UnitFacts{
			Activated: true, ActivationInputsDigest: activation.InputsDigest, Item: child.State,
			Terminal: terminalDisposition(child.Disposition),
		}
		if child.State == "" {
			fact.Unreadable, fact.UnreadableReason = true, child.Reason
		}
		if child.State == orchestration.ItemCompleted {
			// A completed child names an admitted handoff by definition. If
			// this read cannot find it, THIS unit is unreadable - its siblings
			// and their branches are not.
			output, err := admittedOutput(tx, activation.RunID, child.HandoffID)
			if err != nil {
				fact.Item, fact.Unreadable, fact.UnreadableReason = "", true, boundedDetail(err.Error())
			}
			fact.Output = output
		}
		facts[unit.ID] = fact
	}
	if err := tx.Commit(); err != nil {
		return WorkGraphView{}, err
	}
	projection, err := orchestration.ProjectWorkGraph(graph, facts)
	if err != nil {
		return WorkGraphView{}, err
	}
	view := WorkGraphView{
		GraphID: graph.ID, Repository: graph.Repository, AgentID: graph.AgentID, Name: graph.Name,
		Revision: graph.Revision, RevisionDigest: digest, RequestedBy: graph.RequestedBy,
		CreatedAt: graph.CreatedAt, Frontier: projection.Frontier,
	}
	for _, unit := range graph.Units {
		decided := projection.Units[unit.ID]
		out := WorkGraphUnitView{
			UnitID: unit.ID, Purpose: unit.Purpose, Role: unit.Role, Issue: unit.Issue,
			DependsOn: unit.DependsOn, State: decided.State, Reason: decided.Reason,
			InputsDigest: decided.InputsDigest, Inputs: decided.Inputs,
			AwaitingDecision: decided.AwaitingDecision,
		}
		if activation, activated := activations[unit.ID]; activated {
			child := children[unit.ID]
			out.RunID, out.Child, out.Output = activation.RunID, &child, facts[unit.ID].Output
		}
		view.Counts.Add(out.State)
		view.Units = append(view.Units, out)
	}
	return view, nil
}

// admittedOutput is the exact subject of the admitted handoff a completed child
// transferred. A completed item names one by definition; its absence is an
// incoherent read, not an output of none.
func admittedOutput(tx *sql.Tx, runID, handoffID string) (*orchestration.UnitOutput, error) {
	handoffs, err := queryRunHandoffs(tx, runID)
	if err != nil {
		return nil, err
	}
	for _, handoff := range handoffs {
		if handoff.ID != handoffID {
			continue
		}
		// The producer's own report travels with the subject. It is the
		// handoff: a downstream unit given only a commit has the change and
		// not what the producer said about it. It stays worker-authored text.
		return &orchestration.UnitOutput{
			HandoffID: handoff.ID, RunID: handoff.RunID,
			CandidateRevision: handoff.Subject.CandidateRevision,
			CandidateTree:     handoff.Subject.CandidateTree,
			Outcome:           handoff.ProducerReport.Outcome,
			Summary:           handoff.ProducerReport.Summary,
			Unresolved:        handoff.ProducerReport.Unresolved,
			RecommendedNext:   handoff.ProducerReport.RecommendedNext,
		}, nil
	}
	return nil, fmt.Errorf("run %s projects as completed on handoff %s, which is not admitted", runID, handoffID)
}
