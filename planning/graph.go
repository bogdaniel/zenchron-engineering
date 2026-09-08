package planning

// Graph validation, shared by templates and plans.
//
// A template and a plan are different artifacts with different authority - one
// is planning input, the other is approved work - but the graph laws are the
// same for both, and stating them twice would be two definitions of a valid
// decomposition. This file is the single definition; both callers adapt their
// stages into stageView and hand them over.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// stageView is the part of a stage the graph laws care about. It deliberately
// carries no budget, objective or rationale: those are not graph properties.
type stageView struct {
	ID                   string
	Kind                 domain.StageKind
	Role                 domain.EngineeringRole
	DependsOn            []string
	RequiredClaims       []string
	Action               *domain.Action
	Independence         *domain.IndependenceRequirement
	RequiresCapabilities []domain.EngineeringCapability
	Profile              string
	TrustRequirement     domain.TrustRequirement
}

// validateStageGraph refuses every graph a plan reconciler could not execute,
// and every graph that would quietly mean something other than what it says.
//
// The refusals are deterministic and complete before anything is approved:
// cycles, dangling dependencies, unknown vocabulary, gates that are really
// worker runs, and worker stages whose independence nothing could satisfy.
func validateStageGraph(stages []stageView) error {
	if len(stages) == 0 {
		return fmt.Errorf("a plan needs at least one stage")
	}
	byID := make(map[string]stageView, len(stages))
	for _, stage := range stages {
		if strings.TrimSpace(stage.ID) == "" {
			return fmt.Errorf("every stage needs an id")
		}
		if _, exists := byID[stage.ID]; exists {
			return fmt.Errorf("duplicate stage id %q", stage.ID)
		}
		byID[stage.ID] = stage
	}
	for _, stage := range stages {
		if err := validateStage(stage, byID); err != nil {
			return err
		}
	}
	return refuseCycles(stages, byID)
}

func validateStage(stage stageView, byID map[string]stageView) error {
	switch stage.Kind {
	case domain.StageAgent:
		if !domain.KnownRole(stage.Role) {
			return fmt.Errorf("stage %q is an agent stage and names role %q, which is not in the role catalogue", stage.ID, stage.Role)
		}
		for _, capability := range stage.RequiresCapabilities {
			if !domain.KnownCapability(capability) {
				return fmt.Errorf("stage %q requires capability %q, which is not in the v0 ontology", stage.ID, capability)
			}
		}
	case domain.StageAssuranceGate, domain.StageHumanDecisionGate:
		// A gate creates no EngineeringRun, so anything that would only make
		// sense for a worker is refused rather than ignored. Ignoring it is how
		// a gate quietly becomes a fake worker run.
		if stage.Role != "" {
			return fmt.Errorf("stage %q is a %s and names role %q: a gate is not performed by a worker", stage.ID, stage.Kind, stage.Role)
		}
		if stage.Profile != "" || len(stage.RequiresCapabilities) > 0 || stage.TrustRequirement != "" {
			return fmt.Errorf("stage %q is a %s and states worker requirements: a gate references existing evidence, authority or human decision state and executes nothing", stage.ID, stage.Kind)
		}
		if stage.Independence != nil {
			return fmt.Errorf("stage %q is a %s and states an independence requirement: independence constrains which worker produces a change, and a gate produces none", stage.ID, stage.Kind)
		}
		if stage.Kind == domain.StageAssuranceGate && len(stage.RequiredClaims) == 0 {
			return fmt.Errorf("stage %q is an assurance gate with no required claims: nothing would ever satisfy it", stage.ID)
		}
		if stage.Kind == domain.StageHumanDecisionGate && len(stage.RequiredClaims) == 0 && stage.Action == nil {
			return fmt.Errorf("stage %q is a human decision gate with neither a required claim nor a protected action: nothing states what a person is deciding", stage.ID)
		}
	default:
		return fmt.Errorf("stage %q has kind %q; the kinds are %s", stage.ID, stage.Kind, stageKindList())
	}

	seen := map[string]bool{}
	for _, dependency := range stage.DependsOn {
		if dependency == stage.ID {
			return fmt.Errorf("stage %q depends on itself", stage.ID)
		}
		if seen[dependency] {
			return fmt.Errorf("stage %q lists dependency %q twice", stage.ID, dependency)
		}
		seen[dependency] = true
		if _, exists := byID[dependency]; !exists {
			return fmt.Errorf("stage %q depends on %q, which is not a stage in this plan", stage.ID, dependency)
		}
	}
	if stage.Independence == nil {
		return nil
	}
	if !knownDimension(stage.Independence.Dimension) {
		return fmt.Errorf("stage %q requires independence dimension %q, which is not a dimension", stage.ID, stage.Independence.Dimension)
	}
	if stage.Independence.Dimension == domain.IndependenceHuman {
		// A worker is never a human leg. Requiring one of an agent stage would
		// be a requirement no eligible worker could ever satisfy, and silently
		// treating a worker as the human is exactly the substitution #64
		// refuses. The representation for "a person must decide" is a human
		// decision gate.
		return fmt.Errorf("stage %q is an agent stage requiring human independence: a worker is never an independent human leg, so express it as a %s", stage.ID, domain.StageHumanDecisionGate)
	}
	if len(stage.Independence.DifferentFrom) == 0 {
		return fmt.Errorf("stage %q requires independence from nothing: an independence obligation names the stages it separates", stage.ID)
	}
	for _, other := range stage.Independence.DifferentFrom {
		if other == stage.ID {
			return fmt.Errorf("stage %q requires independence from itself", stage.ID)
		}
		peer, exists := byID[other]
		if !exists {
			return fmt.Errorf("stage %q requires independence from %q, which is not a stage in this plan", stage.ID, other)
		}
		if peer.Kind != domain.StageAgent {
			return fmt.Errorf("stage %q requires independence from %q, which is a %s and produces nothing to be independent of", stage.ID, other, peer.Kind)
		}
	}
	return nil
}

// refuseCycles is a deterministic topological walk. A cycle is refused with the
// stages that remain unresolved, so an operator sees the loop rather than a
// bare "invalid graph".
func refuseCycles(stages []stageView, byID map[string]stageView) error {
	remaining := make(map[string]int, len(stages))
	for _, stage := range stages {
		remaining[stage.ID] = len(stage.DependsOn)
	}
	dependents := make(map[string][]string, len(stages))
	for _, stage := range stages {
		for _, dependency := range stage.DependsOn {
			dependents[dependency] = append(dependents[dependency], stage.ID)
		}
	}
	ready := make([]string, 0, len(stages))
	for id, count := range remaining {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	settled := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		settled++
		next := append([]string(nil), dependents[id]...)
		sort.Strings(next)
		for _, dependent := range next {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if settled == len(stages) {
		return nil
	}
	unresolved := make([]string, 0, len(stages)-settled)
	for id, count := range remaining {
		if count > 0 {
			unresolved = append(unresolved, id)
		}
	}
	sort.Strings(unresolved)
	return fmt.Errorf("stage dependencies form a cycle among %s", strings.Join(unresolved, ", "))
}

func stageKindList() string {
	kinds := make([]string, 0, len(domain.StageKinds()))
	for _, kind := range domain.StageKinds() {
		kinds = append(kinds, string(kind))
	}
	return strings.Join(kinds, ", ")
}

func knownDimension(dimension domain.IndependenceDimension) bool {
	for _, known := range domain.IndependenceDimensions() {
		if known == dimension {
			return true
		}
	}
	return false
}

// dimensionStrength orders the independence dimensions so a stronger
// requirement is never silently degraded into a weaker one.
func dimensionStrength(dimension domain.IndependenceDimension) int {
	for strength, known := range domain.IndependenceDimensions() {
		if known == dimension {
			return strength
		}
	}
	return -1
}
