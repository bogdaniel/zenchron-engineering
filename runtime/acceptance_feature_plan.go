package runtime

// Operator-declared final-integration feature acceptance: the plan-time half.
//
// The operator's planning request names the feature, the compiler binds it to
// the one terminal deterministic integration stage (planning.Compile), and this
// file refuses, before approval, a plan whose EFFECTIVE input set that stage
// would consume cannot be governed - the same set compileObjectiveGraph hands
// the WorkGraph unit, not the stage's literal depends_on.

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

const (
	// attributionNone is the reserved option an operator selects to attribute
	// the failure to no upstream input. It can never be a unit id.
	attributionNone = "none"
	// maxAttributedInputs is how many upstream inputs the attribution decision
	// can name: the decision menu is bounded, and one slot is the reserved
	// "none".
	maxAttributedInputs = orchestration.MaxDecisionRequestOptions - 1
)

// validateAcceptanceFeaturePlan refuses a declared feature whose final
// integration consumes more inputs than an attribution decision can name, or
// one that names an input "none".
func validateAcceptanceFeaturePlan(plan domain.EngineeringPlan) error {
	for _, stage := range plan.Stages {
		if stage.AcceptanceFeature == "" {
			continue
		}
		if !objectiveCandidateStage(stage) {
			return fmt.Errorf("stage %q declares acceptance feature %q and produces no material change", stage.ID, stage.AcceptanceFeature)
		}
		inputs := objectiveDependencies(plan, stage)
		if len(inputs) > maxAttributedInputs {
			return fmt.Errorf("stage %q declares acceptance feature %q and consumes %d inputs; the attribution decision can name at most %d", stage.ID, stage.AcceptanceFeature, len(inputs), maxAttributedInputs)
		}
		for _, input := range inputs {
			if input == attributionNone {
				return fmt.Errorf("stage %q declares acceptance feature %q and consumes a unit named %q, which is reserved for the attribution decision", stage.ID, stage.AcceptanceFeature, attributionNone)
			}
		}
		if err := requireFinalMaterialIntegration(plan, stage); err != nil {
			return err
		}
	}
	return nil
}

// requireFinalMaterialIntegration refuses a declaration whose integration is not
// where all the objective's material ends up. Over the EFFECTIVE objective graph
// (the units compileObjectiveGraph builds, not the literal depends_on), every
// other material-producing stage must be in the integration's consumed ancestry:
// a producer downstream of it, or one it never consumes, would ship material the
// feature tests never saw. Reviewers, typed gates and approvals produce no
// material, are not units, and remain allowed after it.
func requireFinalMaterialIntegration(plan domain.EngineeringPlan, final domain.PlanStage) error {
	byID := make(map[string]domain.PlanStage, len(plan.Stages))
	for _, stage := range plan.Stages {
		byID[stage.ID] = stage
	}
	ancestry := func(id string) map[string]bool {
		seen := map[string]bool{}
		var walk func(string)
		walk = func(current string) {
			for _, dependency := range objectiveDependencies(plan, byID[current]) {
				if !seen[dependency] {
					seen[dependency] = true
					walk(dependency)
				}
			}
		}
		walk(id)
		return seen
	}
	consumed := ancestry(final.ID)
	for _, stage := range plan.Stages {
		if stage.ID == final.ID || !objectiveCandidateStage(stage) || consumed[stage.ID] {
			continue
		}
		if ancestry(stage.ID)[final.ID] {
			return fmt.Errorf("stage %q produces material downstream of %q, which declares acceptance feature %q: the feature tests would not see it", stage.ID, final.ID, final.AcceptanceFeature)
		}
		return fmt.Errorf("stage %q produces material outside the ancestry %q consumes, and %q declares acceptance feature %q: the feature tests would not see it", stage.ID, final.ID, final.ID, final.AcceptanceFeature)
	}
	return nil
}

// declaredAcceptanceFeature reads the operator-declared feature of one
// WorkGraph unit from the digest-pinned plan the graph was compiled from. A
// graph that is not an objective graph, or a unit that is not a plan stage,
// declares none. The plan revision must be exactly the one the graph pins: a
// graph never reads a feature from a revision it was not compiled from.
func declaredAcceptanceFeature(store *SQLiteOperationStore, graphID, unitID string) (string, error) {
	graph, found, err := store.WorkGraph(graphID)
	if err != nil {
		return "", err
	}
	if !found || graph.ObjectivePlan == nil {
		return "", nil
	}
	plan, found, err := store.PlanRevision(graph.ObjectivePlan.ID, graph.ObjectivePlan.Revision)
	if err != nil {
		return "", err
	}
	if !found || plan.Digest != graph.ObjectivePlan.Digest {
		return "", fmt.Errorf("work graph %s pins plan %s revision %d, which is not stored at that digest", graphID, graph.ObjectivePlan.ID, graph.ObjectivePlan.Revision)
	}
	stage, found := plan.Stage(unitID)
	if !found {
		return "", nil
	}
	return stage.AcceptanceFeature, nil
}

// verifyAcceptanceFeatureOrigin refuses an origin whose recorded feature is not
// the one its pinned plan stage declares. The origin is a copy for identity and
// replay; the plan is the source, so a disagreement is corrupt state that stops
// the run rather than a value to prefer.
// ponytail: two store reads per run load for an orchestrated run; memoize by plan
// digest if reconcile passes ever show it in a profile.
func verifyAcceptanceFeatureOrigin(store *SQLiteOperationStore, origin *orchestration.BatchOrigin) error {
	if origin == nil {
		return nil
	}
	declared, err := declaredAcceptanceFeature(store, origin.GraphID, origin.UnitID)
	if err != nil {
		return err
	}
	if declared != origin.AcceptanceFeature {
		return fmt.Errorf("work unit %s of graph %s records acceptance feature %q and its pinned plan declares %q",
			origin.UnitID, origin.GraphID, origin.AcceptanceFeature, declared)
	}
	return nil
}
