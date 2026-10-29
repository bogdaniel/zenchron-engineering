package planning

import (
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// SubstituteHumanReview replaces one agent stage with a human decision gate.
//
// It is the operator's answer to an independence shortage that POLICY permits a
// human to fill: no eligible independent worker exists, policy said a person may
// review instead, and the operator decided to do that. The conversion is
// deterministic and it is not applied here - it produces the stages for a new
// revision, which goes through the ordinary validation and approval boundary
// like any other.
//
// It refuses where policy did not permit the substitution. A plan cannot grant
// itself a substitution, and neither can an operator: the permission comes from
// the obligation that required the independence in the first place.
func SubstituteHumanReview(plan domain.EngineeringPlan, stageID string, claims []string) ([]domain.PlanStage, error) {
	stage, found := plan.Stage(stageID)
	if !found {
		return nil, fmt.Errorf("plan %s has no stage %q", plan.ID, stageID)
	}
	if stage.Kind != domain.StageAgent {
		return nil, fmt.Errorf("stage %q is a %s: only an agent stage can be replaced by a human decision", stageID, stage.Kind)
	}
	if stage.Independence == nil || !stage.Independence.HumanSubstitutionPermitted {
		return nil, fmt.Errorf("stage %q does not carry a policy-permitted human substitution: the permission comes from the obligation that required the independence, and nothing else may grant it", stageID)
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("a human decision gate states what the person is deciding, and no required claim was given")
	}
	stages := make([]domain.PlanStage, 0, len(plan.Stages))
	for _, existing := range plan.Stages {
		if existing.ID != stageID {
			stages = append(stages, existing)
			continue
		}
		stages = append(stages, domain.PlanStage{
			ID:              existing.ID,
			Kind:            domain.StageHumanDecisionGate,
			DependsOn:       existing.DependsOn,
			RequiredClaims:  mergeStrings(existing.RequiredClaims, claims),
			SubstitutesRole: existing.Role,
			Rationale: fmt.Sprintf("operator decision: no eligible worker is independent of %s in dimension %q, and policy permits an independent human review in its place",
				strings.Join(existing.Independence.DifferentFrom, ", "), existing.Independence.Dimension),
		})
	}
	return stages, nil
}
