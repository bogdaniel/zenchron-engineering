package runtime

import "fmt"

// A remediation epoch of a plan-bound objective unit is a CHILD RUN of that
// plan (#480), so it is admitted against what the plan approved.
//
// The epoch machinery (#581) knows graphs and units, not plans. Left alone it
// would create a run the plan never counted, under whichever worker the graph
// names rather than the one the stage was approved against. This is the one
// place that closes both gaps, before the epoch's batch is written.

// admitEpochToPlan refuses an epoch the governing plan did not grant, and
// charges an admitted one to the plan's child-run budget exactly once.
//
// The charge is keyed by the epoch's run, the key the reconciler uses for a
// stage's own run, so a replay - or a crash between the charge and the batch
// write, which deterministically reuses the same run id - counts it once. A
// graph with no objective plan has nothing to admit against.
func (s *Supervisor) admitEpochToPlan(graph WorkGraphView, unitID, epochRunID string) error {
	if graph.ObjectivePlan == nil {
		return nil
	}
	ref := *graph.ObjectivePlan
	plan, found, err := s.deps.Store.PlanRevision(ref.ID, ref.Revision)
	if err != nil {
		return err
	}
	if !found || plan.Digest != ref.Digest {
		return fmt.Errorf("plan %s revision %d no longer matches the graph's approved binding", ref.ID, ref.Revision)
	}
	snapshot, err := s.deps.Store.ReplayPlan(ref.ID)
	if err != nil {
		return err
	}
	stage, ok := snapshot.Stages[unitID]
	if !ok || stage.AgentID == "" {
		return fmt.Errorf("plan %s froze no assignment for stage %s, so no epoch can succeed it", ref.ID, unitID)
	}
	agent, err := s.deps.Agents.Agent(graph.AgentID)
	if err != nil {
		return err
	}
	for _, drift := range []struct{ what, approved, epoch string }{
		{"agent", stage.AgentID, agent.ID},
		{"provider kind", stage.ProviderKind, agent.Kind},
		{"trust mode", stage.TrustMode, string(agent.TrustMode)},
	} {
		if drift.approved != drift.epoch {
			return fmt.Errorf("stage %s was approved against %s %q and the graph's remediation worker has %q: an epoch cannot change the worker a plan approved",
				unitID, drift.what, drift.approved, drift.epoch)
		}
	}
	if snapshot.RunConsumed[epochRunID].ChildRuns > 0 {
		return nil
	}
	if snapshot.Consumed.ChildRuns >= plan.BudgetEnvelope.MaxChildRuns {
		return fmt.Errorf("plan %s allows %d child runs and %d have been created, so stage %s cannot start a remediation epoch",
			ref.ID, plan.BudgetEnvelope.MaxChildRuns, snapshot.Consumed.ChildRuns, unitID)
	}
	if block := invocationCeilingReached(plan, snapshot, unitID); block != nil {
		return fmt.Errorf("stage %s cannot start a remediation epoch: %s", unitID, block.Reason)
	}
	return appendPlanEvent(s.deps.Store, s.deps.Clock.Now(), ref.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		Key: "child_run:" + epochRunID, StageID: unitID, RunID: epochRunID, ChildRuns: 1,
	})
}
