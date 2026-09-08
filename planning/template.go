package planning

// EngineeringPlanTemplate is PLANNING INPUT.
//
// It states a reusable process an operator wants: these stages, in this order,
// with these roles and these independence expectations. It is not governance
// and it is not execution authority. Policy may add obligations a template
// omitted; a template may not remove one, widen trust or permission, declare
// evidence sufficient, reset a budget, or authorize adoption.
//
// It is also deliberately not an execution DSL. There are no loops, no
// expressions, no scripts and no nested templates. The single conditionality a
// template has is `when`, which reuses the SAME fact predicate the policy
// compiler already evaluates - so a conditional stage is deterministic and is
// explained by the same facts everything else in this product is explained by.

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// validateTemplateGraph applies the shared graph laws plus the two rules that
// are specific to a template: an unknown budget envelope may only tighten (that
// is checked when it is applied, where the operator ceiling is known), and a
// conditional stage's predicate has to be a predicate the compiler can evaluate.
func validateTemplateGraph(template domain.EngineeringPlanTemplate) error {
	stages := make([]stageView, 0, len(template.Stages))
	for _, stage := range template.Stages {
		stages = append(stages, stageView{
			ID: stage.ID, Kind: stage.Kind, Role: stage.Role, DependsOn: stage.DependsOn,
			RequiredClaims: stage.RequiredClaims, Action: stage.Action,
			Independence: stage.Independence, RequiresCapabilities: stage.RequiresCapabilities,
			Profile: stage.Profile,
		})
	}
	if err := validateStageGraph(stages); err != nil {
		return err
	}
	for _, stage := range template.Stages {
		if stage.When == nil {
			continue
		}
		if stage.When.Fact == "" {
			return fmt.Errorf("stage %q has a condition naming no fact", stage.ID)
		}
		if stage.When.Equals != domain.FactTrue && stage.When.Equals != domain.FactFalse && stage.When.Equals != domain.FactUnknown {
			return fmt.Errorf("stage %q has a condition comparing fact %q to %q, which is not a fact value", stage.ID, stage.When.Fact, stage.When.Equals)
		}
	}
	return nil
}

// TemplateStagesFor selects the stages a template contributes for one set of
// engineering facts.
//
// A conditional stage is included when its predicate matches, by exactly the
// rule the policy compiler uses. An unconditional stage is always included.
// Nothing else about a template is conditional, so this is the whole of its
// "logic".
func TemplateStagesFor(template domain.EngineeringPlanTemplate, facts []domain.EngineeringFact) []domain.TemplateStage {
	selected := make([]domain.TemplateStage, 0, len(template.Stages))
	included := make(map[string]bool, len(template.Stages))
	for _, stage := range template.Stages {
		if stage.When == nil || matchesFact(*stage.When, facts) {
			selected = append(selected, stage)
			included[stage.ID] = true
		}
	}
	// A dependency on a stage the condition excluded is DROPPED rather than
	// left dangling: the excluded stage is not going to happen, so waiting for
	// it would deadlock the plan. The remaining dependencies still order what
	// is left, which is what an operator writing a conditional stage means.
	result := make([]domain.TemplateStage, 0, len(selected))
	for _, stage := range selected {
		kept := make([]string, 0, len(stage.DependsOn))
		for _, dependency := range stage.DependsOn {
			if included[dependency] {
				kept = append(kept, dependency)
			}
		}
		if len(kept) == 0 {
			stage.DependsOn = nil
		} else {
			stage.DependsOn = kept
		}
		result = append(result, stage)
	}
	return result
}

// matchesFact is the template's predicate evaluation. It is deliberately the
// same shape as the policy compiler's: same fact key, same value, same optional
// stage/confidence/provenance qualifiers.
func matchesFact(condition domain.PolicyCondition, facts []domain.EngineeringFact) bool {
	for _, fact := range facts {
		if fact.Key != condition.Fact || fact.Value != condition.Equals {
			continue
		}
		if condition.Stage != nil && fact.Stage != *condition.Stage {
			continue
		}
		if condition.Confidence != nil && fact.Confidence != *condition.Confidence {
			continue
		}
		if condition.Provenance != nil {
			if condition.Provenance.Type != nil && fact.Provenance.Type != *condition.Provenance.Type {
				continue
			}
			if condition.Provenance.Producer != nil && fact.Provenance.Producer != *condition.Provenance.Producer {
				continue
			}
		}
		return true
	}
	return false
}
