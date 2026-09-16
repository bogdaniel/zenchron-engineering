package runtime

import (
	"context"
	"fmt"
	"time"
)

const ReasonReviewBudgetExhausted = "review_wall_budget_exhausted"
const EventRunWallBudgetExtended = "run.wall_budget_extended"

// WallBudgetExtension is explicit operator authority for a new TOTAL active
// allowance. It never resets consumption or grants execution/publication rights.
type WallBudgetExtension struct {
	Total    time.Duration    `json:"total"`
	Operator RecordedOperator `json:"operator"`
}

func (s *runState) authorizedWallLimit(limit time.Duration) time.Duration {
	for _, event := range s.events {
		if event.Type != EventRunWallBudgetExtended {
			continue
		}
		p, err := decodePayload[WallBudgetExtension](event.Payload)
		if err == nil && p.Total > limit {
			limit = p.Total
		}
	}
	return limit
}

// ExtendWallBudget is a governed recovery operation on a published, waiting
// run. Terminal generations cannot be resurrected. Plan stages require their
// own budget governance and are deliberately excluded from this operation.
// Callers must hold the same exclusive state ownership used to drive runs.
func (r *EngineeringRuntime) ExtendWallBudget(ctx context.Context, runID string, total time.Duration, operator RecordedOperator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if operator.ID == "" || operator.Provenance != ProvenanceLocalUnverified {
		return &OperatorIdentityError{Detail: "budget extension requires a resolved local operator"}
	}
	state, err := r.load(runID)
	if err != nil {
		return err
	}
	if terminalDisposition(state.snapshot.Disposition) || state.run.Plan != nil || state.controllerChanged || state.projection.ObservedExternalHead != "" || state.projection.SourceIntentChanged {
		return fmt.Errorf("budget extension requires a nonterminal standalone run with unchanged controller and source")
	}
	if state.snapshot.Disposition != Waiting || state.snapshot.Reason != ReasonReviewBudgetExhausted {
		return fmt.Errorf("budget extension requires a durable %s wait", ReasonReviewBudgetExhausted)
	}
	// Retrying the same total is idempotent, including after a lost CLI response.
	for _, event := range state.events {
		if event.Type == EventRunWallBudgetExtended {
			p, err := decodePayload[WallBudgetExtension](event.Payload)
			if err != nil {
				return err
			}
			if p.Total == total && p.Operator == operator {
				return nil
			}
		}
	}
	if total <= state.budgets().WallLimit || total <= state.activeElapsed(r.deps.Clock.Now()) {
		return fmt.Errorf("new total must exceed both the current allowance and consumed active time")
	}
	return r.append(state, EventRunWallBudgetExtended, "", WallBudgetExtension{Total: total, Operator: operator}, nil)
}
