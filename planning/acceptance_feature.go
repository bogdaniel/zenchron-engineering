package planning

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// bindAcceptanceFeature stamps the operator's declared feature on the one
// terminal deterministic integration stage. A plan with none, or with several
// terminal integrations, is refused before approval: the declaration is a
// requirement, and a requirement with no stage to carry it is not silently
// dropped.
func bindAcceptanceFeature(stages []domain.PlanStage, feature string) ([]domain.PlanStage, error) {
	if feature == "" {
		return stages, nil
	}
	if !domain.ValidAcceptanceFeature(feature) {
		return nil, fmt.Errorf("acceptance feature %q is not a valid feature name", feature)
	}
	views := make([]stageView, 0, len(stages))
	byID := make(map[string]stageView, len(stages))
	for _, stage := range stages {
		view := stageView{ID: stage.ID, Kind: stage.Kind, ExecutionKind: stage.ExecutionKind, DependsOn: stage.DependsOn}
		views, byID[stage.ID] = append(views, view), view
	}
	final := -1
	for i, stage := range stages {
		if stage.Kind != domain.StageAgent || orchestration.WorkUnitExecutionKind(stage.ExecutionKind) != orchestration.ExecutionKindIntegrationCompose {
			continue
		}
		if downstreamIntegration(stage.ID, views, byID) {
			continue
		}
		if final >= 0 {
			return nil, fmt.Errorf("acceptance feature %q is ambiguous: stages %q and %q are both final deterministic integrations", feature, stages[final].ID, stage.ID)
		}
		final = i
	}
	if final < 0 {
		return nil, fmt.Errorf("acceptance feature %q needs a final deterministic integration stage, and the plan has none", feature)
	}
	stages = append([]domain.PlanStage{}, stages...)
	stages[final].AcceptanceFeature = feature
	return stages, nil
}

// withoutEchoedAcceptanceFeature copies a proposal, dropping a stage's
// acceptance_feature when it merely echoes the operator's own declaration (a
// revision such as a human substitution carries untouched stages back verbatim)
// and refusing any other value: a proposal cannot invent or replace one. The
// compiler then stamps the operator's declaration on the stage it belongs to.
func withoutEchoedAcceptanceFeature(proposed []domain.PlanStage, declared string) ([]domain.PlanStage, error) {
	out := append([]domain.PlanStage{}, proposed...)
	for i, stage := range out {
		switch {
		case stage.AcceptanceFeature == "":
		case stage.AcceptanceFeature == declared:
			out[i].AcceptanceFeature = ""
		default:
			return nil, fmt.Errorf("stage %q declares acceptance feature %q: only the operator's planning request may, and a proposal cannot invent, remove or replace it", stage.ID, stage.AcceptanceFeature)
		}
	}
	return out, nil
}
