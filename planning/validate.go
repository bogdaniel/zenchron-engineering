package planning

// Deterministic plan validation.
//
// This is the boundary #64 calls "reasoning output is proposal, never
// authority". Everything that reaches it - a compiled plan, a reasoning agent's
// proposal, an operator's edit, a decomposition stage's revision - is checked by
// the SAME rules, so a plan cannot become executable by arriving through a
// different door.
//
// What it refuses is stated rather than emergent:
//
//	graph        cycles, dangling dependencies, gates that are worker runs
//	policy       any compiled obligation the plan would leave unmet
//	privilege    trust or permission the previous revision did not have
//	budget       an envelope wider than the operator authorized, or one that
//	             would reset consumption
//	identity     a revision that does not follow the one it replaces

import (
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ValidationError is the typed refusal. It carries every reason rather than the
// first, because an operator fixing a plan should see the whole answer.
type ValidationError struct {
	PlanID  string
	Reasons []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("plan %s is invalid: %s", e.PlanID, strings.Join(e.Reasons, "; "))
}

// ValidationInput is what a plan is judged against.
type ValidationInput struct {
	// Contract is the compiled work contract whose plan_requirements are the
	// obligations this plan must satisfy.
	Contract domain.EngineeringWorkContract
	// Envelope is the OPERATOR ceiling. A plan may be tighter and never wider.
	Envelope domain.PlanBudgetEnvelope
	// Previous is the revision being replaced, when there is one.
	Previous *domain.EngineeringPlan
	// Consumed is what the plan has already spent. A revision may tighten the
	// remaining envelope and may never claim back consumption.
	Consumed domain.PlanConsumption
}

// Validate refuses every plan that could not be executed as approved.
func Validate(plan domain.EngineeringPlan, input ValidationInput) error {
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }

	if plan.ID == "" || plan.Revision < 1 || plan.Objective == "" {
		add("a plan needs an id, a revision and an objective")
	}
	if plan.Subject.Repository == "" || plan.Subject.Revision == "" {
		add("a plan is bound to an exact repository revision")
	}
	if input.Contract.ID != "" {
		if plan.Provenance.Contract.ID != input.Contract.ID || plan.Provenance.Contract.Revision != input.Contract.Revision {
			add("plan provenance names contract %s/%s and it is compiled against %s/%s",
				plan.Provenance.Contract.ID, plan.Provenance.Contract.Revision, input.Contract.ID, input.Contract.Revision)
		}
	}
	if err := validateStageGraph(planStageViews(plan)); err != nil {
		add("%s", err.Error())
	}
	// Only agent stages become EngineeringRuns. Every other rule about gates is
	// in the graph laws; this is the one about what a gate REFERENCES.
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent {
			continue
		}
		if input.Contract.ID == "" {
			// No contract was supplied to validate against, so there is nothing
			// to check the claims AGAINST. That is a caller that asked for the
			// structural laws only.
			continue
		}
		for _, claim := range stage.RequiredClaims {
			if _, ok := input.Contract.RequiredClaims[claim]; !ok {
				// A CLAIMLESS contract used to skip this check entirely, so a
				// gate could reference claims nothing defines - valid at
				// approval, and unsatisfiable forever at runtime.
				add("stage %q is a %s referencing claim %q, which the work contract does not define", stage.ID, stage.Kind, claim)
			}
		}
	}
	if input.Contract.PlanRequirements != nil {
		reasons = append(reasons, unmetObligations(plan, *input.Contract.PlanRequirements)...)
	}
	reasons = append(reasons, envelopeViolations(plan, input)...)
	if input.Previous != nil {
		reasons = append(reasons, revisionViolations(plan, *input.Previous)...)
	}
	if len(reasons) > 0 {
		return &ValidationError{PlanID: plan.ID, Reasons: reasons}
	}
	return nil
}

func planStageViews(plan domain.EngineeringPlan) []stageView {
	views := make([]stageView, 0, len(plan.Stages))
	for _, stage := range plan.Stages {
		views = append(views, stageView{
			ID: stage.ID, Kind: stage.Kind, Role: stage.Role, DependsOn: stage.DependsOn,
			RequiredClaims: stage.RequiredClaims, Action: stage.Action, Independence: stage.Independence,
			SubstitutesRole:      stage.SubstitutesRole,
			RequiresCapabilities: stage.RequiresCapabilities, Profile: stage.Profile,
			TrustRequirement: stage.TrustRequirement,
		})
	}
	return views
}

// unmetObligations is the "policy wins" check. A plan that would leave an
// obligation unmet is refused even when a reasoning agent recommended it and
// even when an operator would have approved it: the obligation came from
// governance, and a plan is not the place it gets negotiated.
func unmetObligations(plan domain.EngineeringPlan, requirements domain.PlanRequirements) []string {
	var reasons []string
	producers := materialProducerStages(plan.Stages)
	for _, requirement := range requirements.Roles {
		stage, found := roleStage(plan, requirement.Role)
		if !found {
			// A human decision gate may stand in for the role where POLICY
			// permitted that substitution - and only there. It is the same
			// obligation answered by a person instead of a worker, which is
			// exactly what the permission says may happen.
			if substituted, ok := humanSubstitute(plan, requirement.Role); ok {
				if requirement.Independence == nil || !requirement.Independence.HumanSubstitutionPermitted {
					reasons = append(reasons, fmt.Sprintf(
						"stage %q substitutes a person for role %q and policy did not permit that substitution", substituted.ID, requirement.Role))
				} else if len(substituted.RequiredClaims) == 0 {
					reasons = append(reasons, fmt.Sprintf(
						"stage %q substitutes a person for role %q and states nothing for them to answer", substituted.ID, requirement.Role))
				}
				continue
			}
			reasons = append(reasons, fmt.Sprintf("policy requires role %q and no stage fulfils it (%s)", requirement.Role, requirement.Statement))
			continue
		}
		for _, capability := range requirement.Capabilities {
			if !stageRequires(stage, capability) {
				reasons = append(reasons, fmt.Sprintf("stage %q fulfils role %q without the required capability %q", stage.ID, requirement.Role, capability))
			}
		}
		if trustStrength(requirement.TrustRequirement) > trustStrength(stage.TrustRequirement) {
			reasons = append(reasons, fmt.Sprintf("stage %q requires %q execution trust and policy requires %q", stage.ID, stage.TrustRequirement, requirement.TrustRequirement))
		}
		if requirement.Independence == nil {
			continue
		}
		switch {
		case stage.Independence == nil:
			reasons = append(reasons, fmt.Sprintf("policy requires stage %q to be independent in dimension %q and the plan states no independence", stage.ID, requirement.Independence.Dimension))
		case dimensionStrength(stage.Independence.Dimension) < dimensionStrength(requirement.Independence.Dimension):
			// Degrading a required independence class is the failure #64 names
			// explicitly. It is refused rather than reported.
			reasons = append(reasons, fmt.Sprintf("stage %q states independence %q where policy requires %q", stage.ID, stage.Independence.Dimension, requirement.Independence.Dimension))
		default:
			for _, producer := range producers {
				if producer == stage.ID {
					continue
				}
				if !contains(stage.Independence.DifferentFrom, producer) {
					reasons = append(reasons, fmt.Sprintf("stage %q must be independent of the material producer %q and does not name it", stage.ID, producer))
				}
			}
			if stage.Independence.HumanSubstitutionPermitted && !requirement.Independence.HumanSubstitutionPermitted {
				// A plan cannot grant itself the substitution. Only the policy
				// that stated the obligation can permit it.
				reasons = append(reasons, fmt.Sprintf("stage %q permits an independent human substitution that policy does not", stage.ID))
			}
		}
	}
	for _, capability := range requirements.Capabilities {
		if !capabilityCovered(plan.Stages, capability) {
			reasons = append(reasons, fmt.Sprintf("policy requires capability %q and no stage requires it", capability))
		}
	}
	for _, gate := range requirements.Gates {
		if !gateCovered(plan, gate) {
			reasons = append(reasons, fmt.Sprintf("policy requires a %s gate and the plan has none matching it (%s)", gate.Kind, gate.Statement))
		}
	}
	return reasons
}

// humanSubstitute is the human decision gate standing in for one role, if the
// plan states one.
func humanSubstitute(plan domain.EngineeringPlan, role domain.EngineeringRole) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageHumanDecisionGate && stage.SubstitutesRole == role {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func roleStage(plan domain.EngineeringPlan, role domain.EngineeringRole) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent && stage.Role == role {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func stageRequires(stage domain.PlanStage, capability domain.EngineeringCapability) bool {
	for _, held := range stage.RequiresCapabilities {
		if held == capability {
			return true
		}
	}
	return false
}

func gateCovered(plan domain.EngineeringPlan, gate domain.GateRequirement) bool {
	for _, stage := range plan.Stages {
		if stage.Kind != gate.Kind || !sameAction(stage.Action, gate.Action) {
			continue
		}
		for _, claim := range gate.RequiredClaims {
			if !contains(stage.RequiredClaims, claim) {
				return false
			}
		}
		return true
	}
	return false
}

// envelopeViolations refuses a plan whose aggregate ceiling exceeds what the
// operator authorized, or whose stage budgets exceed the plan's own ceiling.
func envelopeViolations(plan domain.EngineeringPlan, input ValidationInput) []string {
	var reasons []string
	ceiling := input.Envelope
	envelope := plan.BudgetEnvelope
	if ceiling.MaxChildRuns > 0 && envelope.MaxChildRuns > ceiling.MaxChildRuns {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d child runs and the operator ceiling is %d", envelope.MaxChildRuns, ceiling.MaxChildRuns))
	}
	if ceiling.MaxConcurrency > 0 && envelope.MaxConcurrency > ceiling.MaxConcurrency {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d concurrent runs and the operator ceiling is %d", envelope.MaxConcurrency, ceiling.MaxConcurrency))
	}
	if ceiling.MaxProviderInvocations > 0 && envelope.MaxProviderInvocations > ceiling.MaxProviderInvocations {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d provider invocations and the operator ceiling is %d", envelope.MaxProviderInvocations, ceiling.MaxProviderInvocations))
	}
	if ceiling.MaxWallSeconds > 0 && envelope.MaxWallSeconds > ceiling.MaxWallSeconds {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d wall seconds and the operator ceiling is %d", envelope.MaxWallSeconds, ceiling.MaxWallSeconds))
	}
	if ceiling.MaxCostMicros != nil && envelope.MaxCostMicros != nil && *envelope.MaxCostMicros > *ceiling.MaxCostMicros {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d cost micros and the operator ceiling is %d", *envelope.MaxCostMicros, *ceiling.MaxCostMicros))
	}
	// A plan may never allow LESS than it has already spent: that would be a
	// ceiling the plan is already past, which reads as a plan that must stop
	// rather than as a reset - so it is refused at the boundary where an
	// operator can still fix it.
	if envelope.MaxChildRuns < input.Consumed.ChildRuns {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d child runs and %d have already been created", envelope.MaxChildRuns, input.Consumed.ChildRuns))
	}
	if envelope.MaxProviderInvocations < input.Consumed.ProviderInvocations {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d provider invocations and %d have already been spent", envelope.MaxProviderInvocations, input.Consumed.ProviderInvocations))
	}
	if envelope.MaxWallSeconds > 0 && envelope.MaxWallSeconds < input.Consumed.WallSeconds {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d wall seconds and %d have already been spent", envelope.MaxWallSeconds, input.Consumed.WallSeconds))
	}
	// Cost is only comparable where it is KNOWN. An unknown cost is not zero,
	// so a ceiling is never judged against one.
	if envelope.MaxCostMicros != nil && input.Consumed.CostKnown && input.Consumed.CostMicros != nil &&
		*envelope.MaxCostMicros < *input.Consumed.CostMicros {
		reasons = append(reasons, fmt.Sprintf("the plan allows %d cost micros and %d have already been reported", *envelope.MaxCostMicros, *input.Consumed.CostMicros))
	}
	agents := 0
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent {
			agents++
		}
	}
	if agents > envelope.MaxChildRuns {
		reasons = append(reasons, fmt.Sprintf("the plan has %d agent stages and allows %d child runs", agents, envelope.MaxChildRuns))
	}
	return reasons
}

// revisionViolations enforces the revision laws that are checkable from the two
// documents alone. Everything else about a revision - which stages are
// invalidated, what is already consumed - belongs to the reconciler, which can
// see durable state.
func revisionViolations(plan, previous domain.EngineeringPlan) []string {
	var reasons []string
	if plan.ID != previous.ID {
		reasons = append(reasons, fmt.Sprintf("revision replaces plan %q with plan %q", previous.ID, plan.ID))
	}
	if plan.Revision <= previous.Revision {
		reasons = append(reasons, fmt.Sprintf("revision %d does not follow revision %d", plan.Revision, previous.Revision))
	}
	if plan.Provenance.PreviousRevision == nil || *plan.Provenance.PreviousRevision != previous.Revision {
		reasons = append(reasons, "a revision records the exact revision it replaces")
	}
	if plan.Subject != previous.Subject {
		reasons = append(reasons, "a revision cannot move the plan onto a different subject")
	}
	// PRIVILEGE. A revision may tighten trust and independence; widening either
	// is new privilege and goes through policy, not through a plan edit.
	for _, stage := range plan.Stages {
		before, found := previousStage(previous, stage.ID)
		if !found {
			continue
		}
		// A stage that CHANGED KIND from a worker stage to a human decision
		// gate is the policy-permitted human substitution: no eligible
		// independent worker existed, policy said a person may answer instead,
		// and an operator decided to. It legitimately drops the worker
		// requirements, because a gate has no worker.
		//
		// It is permitted only where the previous stage carried that
		// permission. Without this the same shape would be the easiest way to
		// escape an independence obligation: turn the reviewer into a gate and
		// the requirement disappears with it.
		if before.Kind == domain.StageAgent && stage.Kind == domain.StageHumanDecisionGate {
			if before.Independence == nil || !before.Independence.HumanSubstitutionPermitted {
				reasons = append(reasons, fmt.Sprintf(
					"stage %q becomes a human decision gate and its previous form did not carry a policy-permitted human substitution: an independence obligation cannot be escaped by changing what the stage is", stage.ID))
			}
			if len(stage.RequiredClaims) == 0 {
				reasons = append(reasons, fmt.Sprintf("stage %q becomes a human decision gate stating nothing for a person to answer", stage.ID))
			}
			continue
		}
		// A stage that keeps its id may not change what it IS. Formally
		// carrying an obligation while becoming a different role is the same
		// escape as dropping it: the reviewer that must differ from the
		// producer cannot become an implementer and still be the reviewer.
		if before.Kind == domain.StageAgent && stage.Kind == domain.StageAgent && before.Role != stage.Role {
			reasons = append(reasons, fmt.Sprintf(
				"stage %q changes role from %q to %q across a revision: a stage that keeps its id keeps what it is, and a new responsibility is a new stage",
				stage.ID, before.Role, stage.Role))
		}
		if trustStrength(stage.TrustRequirement) < trustStrength(before.TrustRequirement) {
			reasons = append(reasons, fmt.Sprintf("stage %q lowers its execution trust from %q to %q across a revision", stage.ID, before.TrustRequirement, stage.TrustRequirement))
		}
		if before.Independence != nil {
			switch {
			case stage.Independence == nil:
				reasons = append(reasons, fmt.Sprintf("stage %q drops its independence requirement across a revision", stage.ID))
			case dimensionStrength(stage.Independence.Dimension) < dimensionStrength(before.Independence.Dimension):
				reasons = append(reasons, fmt.Sprintf("stage %q weakens independence from %q to %q across a revision", stage.ID, before.Independence.Dimension, stage.Independence.Dimension))
			case !before.Independence.HumanSubstitutionPermitted && stage.Independence.HumanSubstitutionPermitted:
				reasons = append(reasons, fmt.Sprintf("stage %q gains a human substitution permission across a revision", stage.ID))
			default:
				// The PEER SET is ratcheted too. Keeping the dimension and
				// re-pointing `different_from` at some other stage leaves the
				// obligation formally intact while the reviewer may share the
				// producer's vendor - the same escape one level down. A peer
				// may only leave the set with the stage it names.
				for _, peer := range before.Independence.DifferentFrom {
					if _, stillAStage := previousStage(plan, peer); !stillAStage {
						continue
					}
					if !namesPeer(stage.Independence.DifferentFrom, peer) {
						reasons = append(reasons, fmt.Sprintf(
							"stage %q stops requiring independence from %q across a revision while that stage remains: an obligation is not re-pointed, it is met",
							stage.ID, peer))
					}
				}
			}
		}
	}
	// A stage can also be REMOVED, and the ratchet above only compares ids that
	// exist in both revisions - so deleting `security-review` and adding
	// `security-review-2` without the obligation weakened the plan without
	// tripping a single rule. An obligation a revision drops has to still be
	// carried by some stage in the same role.
	for _, before := range previous.Stages {
		if _, still := previousStage(plan, before.ID); still {
			continue
		}
		if before.Independence != nil {
			if !roleCarriesIndependence(plan, before, before.Independence.HumanSubstitutionPermitted) {
				reasons = append(reasons, fmt.Sprintf(
					"stage %q carried %q independence for role %q and the revision removes it without any stage in that role carrying it: an obligation cannot be dropped by renaming the stage that held it",
					before.ID, before.Independence.Dimension, before.Role))
			}
		}
		if before.Kind == domain.StageAgent && trustStrength(before.TrustRequirement) > 0 {
			if !roleCarriesTrust(plan, before.Role, before.TrustRequirement, before.Independence != nil && before.Independence.HumanSubstitutionPermitted) {
				reasons = append(reasons, fmt.Sprintf(
					"stage %q required %q trust for role %q and the revision removes it without any stage in that role requiring it",
					before.ID, before.TrustRequirement, before.Role))
			}
		}
	}
	return reasons
}

// roleCarriesIndependence reports whether some stage in this role still carries
// an independence obligation at least as strong as the one named.
// substitutionPermitted says whether the REMOVED stage carried the policy
// permission for a person to answer in place of the worker. Without it, a
// human decision gate is not an answer to the obligation - accepting one
// anyway would reopen, through a rename, exactly the escape the same-id rule
// closes.
func roleCarriesIndependence(plan domain.EngineeringPlan, before domain.PlanStage, substitutionPermitted bool) bool {
	for _, stage := range plan.Stages {
		switch {
		case substitutionPermitted && stage.Kind == domain.StageHumanDecisionGate && stage.SubstitutesRole == before.Role:
			// A person standing in for the role, where policy permitted that
			// substitution, is the answer the permission describes.
			return true
		case stage.Role != before.Role || stage.Independence == nil:
			continue
		case dimensionStrength(stage.Independence.Dimension) < dimensionStrength(before.Independence.Dimension):
			continue
		case !carriesPeers(plan, stage.Independence.DifferentFrom, before.Independence.DifferentFrom):
			// The DIMENSION is not the obligation. A replacement in the same
			// role at the same strength that points somewhere else reviews
			// different work, and may share the vendor of the producer the
			// removed stage was about - the rename escape, one level down.
			continue
		default:
			return true
		}
	}
	return false
}

// carriesPeers reports whether a replacement still names every peer the removed
// stage named, for the peers that remain stages of this plan.
func carriesPeers(plan domain.EngineeringPlan, replacement, removed []string) bool {
	for _, peer := range removed {
		if _, remains := previousStage(plan, peer); !remains {
			continue
		}
		if !namesPeer(replacement, peer) {
			return false
		}
	}
	return true
}

func roleCarriesTrust(plan domain.EngineeringPlan, role domain.EngineeringRole, trust domain.TrustRequirement, substitutionPermitted bool) bool {
	for _, stage := range plan.Stages {
		if substitutionPermitted && stage.Kind == domain.StageHumanDecisionGate && stage.SubstitutesRole == role {
			return true
		}
		if stage.Role == role && trustStrength(stage.TrustRequirement) >= trustStrength(trust) {
			return true
		}
	}
	return false
}

func namesPeer(peers []string, peer string) bool {
	for _, named := range peers {
		if named == peer {
			return true
		}
	}
	return false
}

func previousStage(plan domain.EngineeringPlan, id string) (domain.PlanStage, bool) {
	for _, stage := range plan.Stages {
		if stage.ID == id {
			return stage, true
		}
	}
	return domain.PlanStage{}, false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
