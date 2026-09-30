package runtime

import "github.com/bogdaniel/zenchron-engineering/domain"

// PlanDependencyBlocker identifies the failed ancestor preventing execution.
type PlanDependencyBlocker struct {
	StageID string `json:"stage_id"`
	Reason  string `json:"reason"`
}

// withDependencyBlockers annotates a copy for operator views only. Scheduling
// and replay continue to use the original lifecycle state and reason.
func (s PlanSnapshot) withDependencyBlockers(plan domain.EngineeringPlan) PlanSnapshot {
	stages := make(map[string]PlanStageProjection, len(s.Stages))
	for id, stage := range s.Stages {
		stage.BlockedBy = nil
		stages[id] = stage
	}
	graph := make(map[string]domain.PlanStage, len(plan.Stages))
	for _, stage := range plan.Stages {
		graph[stage.ID] = stage
	}
	for _, stage := range plan.Stages {
		projection := s.stage(stage.ID)
		if projection.State != PlanStagePending && projection.State != "" {
			continue
		}
		projection.BlockedBy = nil
		seen := map[string]bool{stage.ID: true}
		var visit func(string)
		visit = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			dependency := s.stage(id)
			switch dependency.State {
			case PlanStageFailed:
				reason := dependency.Reason
				if reason == "" {
					reason = "failed"
				}
				projection.BlockedBy = append(projection.BlockedBy, PlanDependencyBlocker{StageID: id, Reason: reason})
			case PlanStagePending, "", PlanStageInvalidated:
				for _, upstream := range graph[id].DependsOn {
					visit(upstream)
				}
			}
		}
		for _, dependency := range stage.DependsOn {
			visit(dependency)
		}
		stages[stage.ID] = projection
	}
	s.Stages = stages
	return s
}
