package runtime

import (
	"context"
	"fmt"
)

// PlanningBudget bounds every planning invocation, including initial intake.
func (r *EngineeringRuntime) PlanningBudget(stageSeconds int) ProviderBudget {
	return ProviderBudget{WallLimit: r.planningWallLimit(stageSeconds), InactivityLimit: r.deps.Budgets.defaults().ProviderInactivityLimit}
}

// StartPlanStageRun creates the ordinary EngineeringRun for one dependency-ready
// agent stage.
//
// It is deliberately the same run every other path creates: same identity
// derivation, same claim, same genesis and agent-binding events, same
// scheduler, same leases, same candidate lifecycle. What a plan adds is the
// BINDING - which plan revision, which stage, which assignment - and nothing
// else. A second kind of run would be a second runtime.
//
// The identity includes the plan revision and the stage, so two stages of one
// plan are two runs and a new revision's stage is a new run rather than a
// silent continuation of work approved under different terms.
func (r *EngineeringRuntime) StartPlanStageRun(ctx context.Context, issue int, binding RunPlanBinding) (StartOutcome, error) {
	return r.startPlanStageRun(ctx, issue, binding, nil)
}

func (r *EngineeringRuntime) startPlanStageRun(ctx context.Context, issue int, binding RunPlanBinding, batch *RunOrchestrationBinding) (StartOutcome, error) {
	if issue <= 0 {
		return StartOutcome{}, fmt.Errorf("a plan stage run answers a source issue, and none was given")
	}
	if binding.PlanID == "" || binding.StageID == "" || binding.AssignmentID == "" || binding.Revision < 1 {
		return StartOutcome{}, fmt.Errorf("a plan stage run needs the plan, revision, stage and assignment it was created for")
	}
	goal := issueGoal(r.deps.Repository.Identity, issue)
	runID, err := derivedRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, 0, &planStageIdentity{
		Plan: binding.PlanID, Revision: binding.Revision, Stage: binding.StageID,
		Generation: binding.Generation,
	})
	if err != nil {
		return StartOutcome{}, err
	}
	existing, found, err := r.deps.Store.Run(runID)
	if err != nil {
		return StartOutcome{}, err
	}
	if found {
		if (existing.Orchestration == nil) != (batch == nil) || (batch != nil && existing.Orchestration.BatchID != batch.BatchID) {
			return StartOutcome{}, &RunConflictError{RunID: runID, Detail: "durable run belongs to a different work graph batch"}
		}
		// The reconciler is idempotent: it re-derives the same identity every
		// tick, so finding the run it created before is the ordinary case, not
		// a conflict. A row describing different work IS a conflict.
		if existing.Repository != r.deps.Repository.Identity || existing.Goal != goal {
			return StartOutcome{}, &RunConflictError{RunID: runID, Detail: "durable run describes different work"}
		}
		if existing.Plan == nil || existing.Plan.PlanID != binding.PlanID || existing.Plan.StageID != binding.StageID {
			return StartOutcome{}, &RunConflictError{RunID: runID, Detail: "durable run belongs to a different plan stage"}
		}
		// The ASSIGNMENT is part of what the run is. Adopting a run created
		// under a different assignment would silently hand this stage's work to
		// a run bound to another worker, another profile and another frozen
		// instruction set.
		if existing.Plan.AssignmentID != binding.AssignmentID {
			return StartOutcome{}, &RunConflictError{
				RunID: runID,
				Detail: fmt.Sprintf("durable run was created under assignment %s and this start names %s",
					existing.Plan.AssignmentID, binding.AssignmentID),
			}
		}
		// The run identity carries the repository, the issue, the configuration
		// and the plan stage - but NOT the controller. Two controllers that
		// share a configuration digest and differ in build or identity derive
		// the same id, so without this check the second one would adopt the
		// first one's live work. StartIssueRun refuses exactly this, and a plan
		// stage run is an ordinary run: it is refused here on the same terms.
		if err := r.refuseUnlessSucceeded(runID, existing); err != nil {
			return StartOutcome{}, err
		}
		if err := r.repairAgentBinding(runID, existing); err != nil {
			return StartOutcome{}, err
		}
		return StartOutcome{RunID: runID, Adopted: true, AdoptedFrom: existing.ControllerSHA256}, nil
	}
	created, err := r.createRun(ctx, runID, goal, &binding, batch, binding.StageBudget, r.deps.Store.ClaimRun)
	return StartOutcome{RunID: created}, err
}
