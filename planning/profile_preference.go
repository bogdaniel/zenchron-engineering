package planning

import "github.com/bogdaniel/zenchron-engineering/domain"

func preferProfile(stages []domain.PlanStage, profile string) []domain.PlanStage {
	for i := range stages {
		if stages[i].Kind == domain.StageAgent && stages[i].Profile == "" {
			stages[i].Profile = profile
		}
	}
	return stages
}
