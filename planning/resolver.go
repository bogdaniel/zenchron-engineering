package planning

// Role -> profile -> execution agent resolution.
//
// It is deterministic and explainable, and it is deliberately NOT a quality
// router. #64 is explicit that at M2 the useful discrimination comes from trust
// eligibility, invocation-mode support, availability, independence and operator
// preference - not from an AI opinion about which model is better, which is #70
// and needs measured outcomes that do not exist yet.
//
// Every rejection is recorded. "Why did my security reviewer not get this
// stage" is an operator question with an exact answer, and an answer that only
// exists inside a resolver's control flow is not an answer.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// BlockKind classifies why a stage could not be assigned. It is typed because
// the operator's next action differs per kind: install a profile, authenticate
// an agent, add a second vendor, or make an explicit decision policy permits.
type BlockKind string

const (
	// BlockNoEligibleProfile means nothing installed can perform the role.
	BlockNoEligibleProfile BlockKind = "no_eligible_profile"
	// BlockInvocationMode means every otherwise-eligible worker lacks the
	// invocation mode the stage requires - a planner-role stage with no
	// provider that can prove a non-mutating boundary, for instance.
	BlockInvocationMode BlockKind = "invocation_mode_unavailable"
	// BlockIndependence means the only eligible workers would violate an
	// independence obligation. This is the single-vendor case #64 names: never
	// assign the same vendor silently, never stall without explanation.
	BlockIndependence BlockKind = "independence_shortage"
	// BlockUnavailable means the eligible workers are not currently usable -
	// unauthenticated, not installed, out of quota.
	BlockUnavailable BlockKind = "agent_unavailable"
	// BlockMissingArtifact means the plan names an operator artifact that is
	// not installed: a pinned profile, or a context policy. It is its own kind
	// because the answer is different - install it, or edit the plan - and
	// reporting it as "nothing can perform this role" sent an operator looking
	// for a worker that was never the problem.
	BlockMissingArtifact BlockKind = "artifact_not_installed"
)

// Blocked is a typed, explainable refusal to assign one stage.
type Blocked struct {
	StageID string                       `json:"stage_id"`
	Role    domain.EngineeringRole       `json:"role"`
	Kind    BlockKind                    `json:"kind"`
	Reason  string                       `json:"reason"`
	Explain domain.ResolutionExplanation `json:"explanation"`
	// HumanSubstitutionPermitted reports that POLICY permits an independent
	// human review in place of the worker that could not be found. It is
	// surfaced as an operator DECISION, not taken automatically: substituting a
	// person for a worker changes what the plan is, so it goes through the
	// ordinary revision and approval boundary.
	HumanSubstitutionPermitted bool `json:"human_substitution_permitted,omitempty"`
}

// Resolution is the outcome for a whole plan revision.
type Resolution struct {
	Assignments []domain.AgentAssignment `json:"assignments"`
	Blocked     []Blocked                `json:"blocked,omitempty"`
}

// Assignment returns the assignment for one stage.
func (r Resolution) Assignment(stageID string) (domain.AgentAssignment, bool) {
	for _, assignment := range r.Assignments {
		if assignment.StageID == stageID {
			return assignment, true
		}
	}
	return domain.AgentAssignment{}, false
}

// ResolveInput is everything resolution reads.
type ResolveInput struct {
	Plan     domain.EngineeringPlan
	Registry Registry
	// Agents is the registered workforce, as the runtime describes it.
	Agents   []domain.ExecutionAgentDescriptor
	Contract domain.EngineeringWorkContract
	Model    domain.ProjectModel
	Facts    []domain.EngineeringFact
	// Upstream is the accepted output of completed stages, keyed by stage id.
	// It is empty at planning time and populated by the reconciler as stages
	// complete, which is what lets an integrator consume EXPLICIT upstream
	// outputs rather than inheriting a conversation.
	Upstream map[string][]domain.UpstreamOutput
	// DefaultAgent is the operator's configured default. It is a PREFERENCE
	// used to break ties deterministically, never an eligibility rule.
	DefaultAgent string
	// Frozen is the assignment a stage is ALREADY executing under, keyed by
	// stage id.
	//
	// A stage that has run is not re-resolved. Two things depend on that: the
	// work is executing under the configuration an operator approved, and a
	// downstream independence obligation is evaluated against the worker that
	// actually produced the change rather than against whichever worker would
	// be chosen for it today.
	Frozen map[string]domain.AgentAssignment
}

// Resolve assigns every agent stage it can and explains every stage it cannot.
//
// Stages resolve in the plan's canonical order, so an independence obligation
// is evaluated against assignments that already exist. That ordering is why the
// plan is topologically sorted: a reviewer's independence is a fact about the
// producer, and the producer has to have been assigned first.
func Resolve(input ResolveInput) (Resolution, error) {
	resolution := Resolution{}
	assigned := map[string]domain.AgentAssignment{}
	profiles := input.candidateProfiles()
	// EVERY frozen assignment is known before the first stage resolves, not as
	// its own stage comes round. A plan compiled before the compiler added the
	// dependency edge can still list a reviewer ahead of its producer, and
	// evaluating that reviewer against an empty map would block it forever on
	// "no resolved worker" while the producer's assignment sat in this very
	// input.
	for id, frozen := range input.Frozen {
		assigned[id] = frozen
	}

	for _, stage := range input.Plan.Stages {
		if stage.Kind != domain.StageAgent {
			// A gate is satisfied from existing evidence, authority or human
			// decision state. Resolving one would be inventing a worker for it.
			continue
		}
		if frozen, ok := input.Frozen[stage.ID]; ok {
			assigned[stage.ID] = frozen
			resolution.Assignments = append(resolution.Assignments, frozen)
			continue
		}
		// An artifact the plan NAMES and the operator has not installed is a
		// deterministic refusal with the artifact's own name in it. Without
		// this, a pinned profile that was never installed was reported as "no
		// eligible profile" - true, and useless - and a missing context policy
		// surfaced much later as a hard error from the middle of resolution.
		if missing := input.missingArtifact(stage); missing != "" {
			resolution.Blocked = append(resolution.Blocked, Blocked{
				StageID: stage.ID, Role: stage.Role, Kind: BlockMissingArtifact, Reason: missing,
			})
			continue
		}
		assignment, blocked, err := input.resolveStage(stage, profiles, assigned)
		if err != nil {
			return Resolution{}, err
		}
		if blocked != nil {
			resolution.Blocked = append(resolution.Blocked, *blocked)
			continue
		}
		assigned[stage.ID] = assignment
		resolution.Assignments = append(resolution.Assignments, assignment)
	}
	return resolution, nil
}

// candidateProfiles is the installed profiles plus one DIRECT profile per
// agent the operator has NOT specialized.
//
// The direct profile exists so the product works before an operator has defined
// any customization: a plan can be executed by the workers they already
// configured. It advertises exactly what the agent advertises and adds no
// instruction, so it specializes nothing and escalates nothing.
//
// An agent the operator HAS specialized is reached only through that
// specialization. Offering the raw worker beside it would let the resolver
// quietly bypass the instructions and context policy the operator wrote - which
// is the whole reason they wrote a profile - and would do it invisibly, because
// both candidates name the same agent.
func (input ResolveInput) candidateProfiles() []domain.AgentProfile {
	profiles := append([]domain.AgentProfile{}, input.Registry.Profiles()...)
	specialized := map[string]bool{}
	for _, profile := range profiles {
		specialized[profile.ExecutionAgent] = true
	}
	for _, agent := range input.Agents {
		if specialized[agent.ID] {
			continue
		}
		profiles = append(profiles, DirectProfile(agent))
	}
	sort.SliceStable(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles
}

// DirectProfile is the implicit profile for one registered agent: the worker
// used as itself.
func DirectProfile(agent domain.ExecutionAgentDescriptor) domain.AgentProfile {
	profile := domain.AgentProfile{
		SchemaVersion:    domain.SchemaVersion,
		ID:               agent.ID,
		Version:          1,
		ExecutionAgent:   agent.ID,
		Model:            agent.Model,
		Capabilities:     agent.Capabilities,
		TrustRequirement: agent.TrustMode,
		Source:           domain.ArtifactSource{Type: domain.SourceOperatorConfig, Location: "agents." + agent.ID},
	}
	// The digest is computed from the same content rule every other artifact
	// uses, so an assignment against a direct profile freezes an identity that
	// changes when the agent's configuration changes.
	if digest, err := profile.ContentDigest(); err == nil {
		profile.Digest = digest
	}
	return profile
}

func (input ResolveInput) resolveStage(stage domain.PlanStage, profiles []domain.AgentProfile, assigned map[string]domain.AgentAssignment) (domain.AgentAssignment, *Blocked, error) {
	agents := map[string]domain.ExecutionAgentDescriptor{}
	for _, agent := range input.Agents {
		agents[agent.ID] = agent
	}
	explanation := domain.ResolutionExplanation{}
	var selected *domain.AgentProfile
	var selectedAgent domain.ExecutionAgentDescriptor
	blockedBy := map[BlockKind]bool{}

	for _, profile := range profiles {
		if stage.Profile != "" && profile.ID != stage.Profile {
			continue
		}
		agent, known := agents[profile.ExecutionAgent]
		evaluation := domain.CandidateEvaluation{Profile: profile.ID, Agent: profile.ExecutionAgent}
		if !known {
			evaluation.Reasons = []string{"agent " + profile.ExecutionAgent + " is not configured"}
			explanation.Considered = append(explanation.Considered, evaluation)
			blockedBy[BlockNoEligibleProfile] = true
			continue
		}
		reasons, kinds := eligibility(stage, profile, agent, assigned)
		if len(reasons) > 0 {
			evaluation.Reasons = reasons
			explanation.Considered = append(explanation.Considered, evaluation)
			for _, kind := range kinds {
				blockedBy[kind] = true
			}
			continue
		}
		evaluation.Eligible = true
		explanation.Considered = append(explanation.Considered, evaluation)
		if selected == nil || preferred(profile, agent, *selected, selectedAgent, stage, input.DefaultAgent) {
			candidate := profile
			selected, selectedAgent = &candidate, agent
		}
	}

	if selected == nil {
		return domain.AgentAssignment{}, input.blocked(stage, explanation, blockedBy), nil
	}
	explanation.Selected = selected.ID
	explanation.Reason = selectionReason(stage, *selected, selectedAgent, input.DefaultAgent)

	binding, err := input.Registry.Binding(*selected)
	if err != nil {
		// A direct profile is not in the registry; its binding is itself.
		binding = domain.ProfileBinding{
			ID: selected.ID, Version: selected.Version, Digest: selected.Digest,
			Capabilities: selected.Capabilities, TrustRequirement: selected.TrustRequirement,
			Constraints: profileConstraints(selected.Constraints),
		}
	}
	contextPolicy, err := input.contextPolicyFor(stage, *selected)
	if err != nil {
		return domain.AgentAssignment{}, nil, err
	}
	pack, err := CompileContext(ContextInput{
		Stage:        stage,
		Role:         stage.Role,
		Contract:     input.Contract,
		Model:        input.Model,
		Facts:        input.Facts,
		Policy:       contextPolicy,
		Upstream:     input.Upstream[stage.ID],
		Independence: independenceBindings(stage, assigned, selectedAgent, *selected),
	})
	if err != nil {
		return domain.AgentAssignment{}, nil, err
	}
	assignment := domain.AgentAssignment{
		SchemaVersion:        domain.SchemaVersion,
		ID:                   assignmentID(input.Plan, stage),
		Plan:                 domain.PlanRef{ID: input.Plan.ID, Revision: input.Plan.Revision, Digest: input.Plan.Digest},
		StageID:              stage.ID,
		Role:                 stage.Role,
		RequiredCapabilities: stage.RequiresCapabilities,
		Profile:              binding,
		Agent: domain.AgentBinding{
			ID: selectedAgent.ID, ProviderKind: selectedAgent.ProviderKind, VendorFamily: selectedAgent.VendorFamily,
			TrustMode: selectedAgent.TrustMode, Model: effectiveModel(*selected, selectedAgent),
		},
		InvocationMode:   stage.InvocationMode,
		TrustRequirement: effectiveTrust(stage, *selected),
		Contract:         domain.ObjectRevision{ID: input.Contract.ID, Revision: input.Contract.Revision},
		Context:          pack,
		Budget:           tightenedByProfile(stage.Budget, selected.Constraints),
		Independence:     independenceBindings(stage, assigned, selectedAgent, *selected),
		Selection:        explanation,
	}
	if _, err := domain.Encode(assignment); err != nil {
		return domain.AgentAssignment{}, nil, fmt.Errorf("resolved assignment for stage %q is invalid: %w", stage.ID, err)
	}
	return assignment, nil, nil
}

// eligibility answers why a profile may NOT perform a stage. An empty result is
// eligibility; every rejection carries the reason it will be recorded under.
func eligibility(stage domain.PlanStage, profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor, assigned map[string]domain.AgentAssignment) ([]string, []BlockKind) {
	var reasons []string
	var kinds []BlockKind
	// 1. The profile must not escalate its worker at all. An escalating profile
	// is ineligible everywhere, not merely for this stage.
	if err := RefuseEscalation(profile, agent); err != nil {
		return []string{err.Error()}, []BlockKind{BlockNoEligibleProfile}
	}
	// 2. Capability.
	for _, capability := range stage.RequiresCapabilities {
		if !profileAdvertises(profile, capability) {
			reasons = append(reasons, "profile does not advertise "+string(capability))
			kinds = append(kinds, BlockNoEligibleProfile)
		}
	}
	// 3. Trust. A stage that requires protected execution is not satisfied by
	// an operator-trusted worker, whatever else it can do.
	required := effectiveTrust(stage, profile)
	if trustStrength(agent.TrustMode) < trustStrength(required) {
		reasons = append(reasons, fmt.Sprintf("agent trust %q is below the required %q", agent.TrustMode, required))
		kinds = append(kinds, BlockNoEligibleProfile)
	}
	// 4. Invocation mode. A provider that cannot prove the required mode is
	// ineligible; it is never degraded into a more permissive one.
	if stage.InvocationMode != "" && !agent.SupportsInvocationMode(stage.InvocationMode) {
		reasons = append(reasons, fmt.Sprintf("agent cannot perform a %q invocation", stage.InvocationMode))
		kinds = append(kinds, BlockInvocationMode)
	}
	// 5. Availability, which is an observation and not a promise: readiness at
	// planning time is not readiness at execution time, and the runtime checks
	// again when work actually starts.
	if !agent.Available {
		reasons = append(reasons, "agent is not currently available: "+boundedReason(agent.Detail))
		kinds = append(kinds, BlockUnavailable)
	}
	// 6. Independence, against the assignments that already exist.
	if violation, blocked := independenceViolation(stage, profile, agent, assigned); blocked {
		reasons = append(reasons, violation)
		kinds = append(kinds, BlockIndependence)
	}
	return reasons, kinds
}

// independenceViolation reports whether assigning this worker would break the
// stage's independence obligation.
//
// The comparison is on the CLASS VALUE in the required dimension, never on the
// identity that produced it: two adapters over one vendor share a vendor family,
// and a revision that renames a profile does not make a producer independent of
// itself.
func independenceViolation(stage domain.PlanStage, profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor, assigned map[string]domain.AgentAssignment) (string, bool) {
	if stage.Independence == nil {
		return "", false
	}
	mine := independenceClass(stage.Independence.Dimension, profile, agent)
	for _, other := range stage.Independence.DifferentFrom {
		assignment, ok := assigned[other]
		if !ok {
			// The stage it must differ from has no assignment, so the
			// obligation cannot be PROVEN - and unproven is not satisfied.
			// Skipping here passed vacuously and stamped the result as
			// independently satisfied, which is the one outcome an independence
			// obligation exists to prevent. The plan graph makes every peer a
			// dependency, so reaching this means the producer could not be
			// resolved at all.
			return fmt.Sprintf("independence in dimension %q cannot be proven: stage %q has no resolved worker",
				stage.Independence.Dimension, other), true
		}
		theirs := assignedIndependenceClass(stage.Independence.Dimension, assignment)
		// An UNKNOWN or ABSENT class proves nothing. This is checked before the
		// comparison because two unknowns are equal and would otherwise be
		// reported as "not independent" rather than as unprovable - and, worse,
		// an empty class on one side alone would compare unequal and pass.
		if mine == "" || theirs == "" || mine == "unknown" || theirs == "unknown" {
			return fmt.Sprintf("independence in dimension %q cannot be proven against stage %q: one side's class is unknown",
				stage.Independence.Dimension, other), true
		}
		if mine == theirs {
			return fmt.Sprintf("%s %q is not independent of stage %q in dimension %q",
				dimensionNoun(stage.Independence.Dimension), mine, other, stage.Independence.Dimension), true
		}
	}
	return "", false
}

// profileConstraints carries a profile's narrowing into the frozen assignment,
// and only when it states one: an empty object in every assignment document
// would be noise in a content-addressed artifact.
func profileConstraints(constraints domain.ProfileConstraints) *domain.ProfileConstraints {
	if constraints == (domain.ProfileConstraints{}) {
		return nil
	}
	return &constraints
}

// tightenedByProfile narrows a stage budget by the profile's constraints. It
// only ever narrows: a profile that stated a HIGHER ceiling than the stage
// would be escalating, and the smaller of the two is what an assignment
// carries.
func tightenedByProfile(budget domain.StageBudget, constraints domain.ProfileConstraints) domain.StageBudget {
	if wall := constraints.MaxWallSeconds; wall != nil && *wall > 0 && (budget.MaxWallSeconds <= 0 || *wall < budget.MaxWallSeconds) {
		budget.MaxWallSeconds = *wall
	}
	if attempts := constraints.MaxExecutionAttempts; attempts != nil && *attempts > 0 && (budget.MaxExecutionAttempts <= 0 || *attempts < budget.MaxExecutionAttempts) {
		budget.MaxExecutionAttempts = *attempts
	}
	return budget
}

func independenceClass(dimension domain.IndependenceDimension, profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor) string {
	switch dimension {
	case domain.IndependenceAgentProfile:
		return profile.ID + "@" + profile.Digest
	case domain.IndependenceExecutionAgent:
		return agent.ID
	case domain.IndependenceProviderKind:
		return agent.ProviderKind
	case domain.IndependenceVendorFamily:
		return agent.VendorFamily
	default:
		// A human leg is never a worker. Reaching here with the human dimension
		// would mean the graph laws let an agent stage require one, which they
		// do not.
		return "unknown"
	}
}

func assignedIndependenceClass(dimension domain.IndependenceDimension, assignment domain.AgentAssignment) string {
	switch dimension {
	case domain.IndependenceAgentProfile:
		return assignment.Profile.ID + "@" + assignment.Profile.Digest
	case domain.IndependenceExecutionAgent:
		return assignment.Agent.ID
	case domain.IndependenceProviderKind:
		return assignment.Agent.ProviderKind
	case domain.IndependenceVendorFamily:
		return assignment.Agent.VendorFamily
	default:
		return "unknown"
	}
}

func dimensionNoun(dimension domain.IndependenceDimension) string {
	switch dimension {
	case domain.IndependenceAgentProfile:
		return "profile"
	case domain.IndependenceExecutionAgent:
		return "agent"
	case domain.IndependenceProviderKind:
		return "provider kind"
	case domain.IndependenceVendorFamily:
		return "vendor family"
	default:
		return "class"
	}
}

// independenceBindings records what was actually compared, so a later reader can
// see the separation rather than trust that it happened.
func independenceBindings(stage domain.PlanStage, assigned map[string]domain.AgentAssignment, agent domain.ExecutionAgentDescriptor, profile domain.AgentProfile) []domain.IndependenceBinding {
	if stage.Independence == nil {
		return nil
	}
	binding := domain.IndependenceBinding{
		Dimension:     stage.Independence.Dimension,
		DifferentFrom: stage.Independence.DifferentFrom,
		Class:         independenceClass(stage.Independence.Dimension, profile, agent),
		SatisfiedBy:   domain.IndependenceSatisfiedByWorker,
	}
	for _, other := range stage.Independence.DifferentFrom {
		if assignment, ok := assigned[other]; ok {
			binding.OtherClasses = append(binding.OtherClasses, assignedIndependenceClass(stage.Independence.Dimension, assignment))
		}
	}
	sort.Strings(binding.OtherClasses)
	return []domain.IndependenceBinding{binding}
}

// blocked turns "nothing was eligible" into the typed state an operator can act
// on. The kind is the STRONGEST explanation available: an independence shortage
// is more actionable than "no eligible profile", because it names what would
// have to change.
func (input ResolveInput) blocked(stage domain.PlanStage, explanation domain.ResolutionExplanation, kinds map[BlockKind]bool) *Blocked {
	kind := BlockNoEligibleProfile
	switch {
	case kinds[BlockIndependence]:
		kind = BlockIndependence
	case kinds[BlockInvocationMode]:
		kind = BlockInvocationMode
	case kinds[BlockUnavailable]:
		kind = BlockUnavailable
	}
	reason := blockReason(kind, stage)
	explanation.Reason = reason
	blocked := &Blocked{StageID: stage.ID, Role: stage.Role, Kind: kind, Reason: reason, Explain: explanation}
	if stage.Independence != nil && kind == BlockIndependence {
		// The substitution is offered only where POLICY permits it. Where it
		// does not, the plan blocks until a genuinely eligible independent
		// producer exists - which is the answer, not a failure to find one.
		blocked.HumanSubstitutionPermitted = stage.Independence.HumanSubstitutionPermitted
	}
	return blocked
}

func blockReason(kind BlockKind, stage domain.PlanStage) string {
	switch kind {
	case BlockIndependence:
		return fmt.Sprintf("no eligible worker is independent of %s in dimension %q; assigning the same one would silently degrade a required independence class",
			strings.Join(stage.Independence.DifferentFrom, ", "), stage.Independence.Dimension)
	case BlockInvocationMode:
		return fmt.Sprintf("no configured agent can perform a %q invocation, and a provider is never degraded into a more permissive mode", stage.InvocationMode)
	case BlockUnavailable:
		return "every eligible worker is currently unavailable; readiness is an observation, so this clears when the agent does"
	default:
		return fmt.Sprintf("no installed profile can perform role %q with the required capabilities", stage.Role)
	}
}

// preferred breaks ties between eligible candidates, deterministically and
// without an opinion about quality.
//
// The order is stated: an explicitly preferred profile, then the operator's
// default agent, then the profile id. There is no benchmark, no score and no
// model-quality heuristic - #70 owns that, and it needs evidence this product
// does not have yet.
func preferred(candidate domain.AgentProfile, candidateAgent domain.ExecutionAgentDescriptor, current domain.AgentProfile, currentAgent domain.ExecutionAgentDescriptor, stage domain.PlanStage, defaultAgent string) bool {
	if stage.Profile != "" {
		return candidate.ID == stage.Profile && current.ID != stage.Profile
	}
	if defaultAgent != "" && candidateAgent.ID != currentAgent.ID {
		if candidateAgent.ID == defaultAgent {
			return true
		}
		if currentAgent.ID == defaultAgent {
			return false
		}
	}
	return candidate.ID < current.ID
}

func selectionReason(stage domain.PlanStage, profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor, defaultAgent string) string {
	switch {
	case stage.Profile == profile.ID:
		return "the plan pinned this profile and it is eligible"
	case agent.ID == defaultAgent:
		return "eligible, and backed by the operator's default agent"
	default:
		return "the first eligible profile in deterministic order"
	}
}

// missingArtifact names an operator artifact this stage requires and the
// registry does not have. Empty means everything it names is installed.
func (input ResolveInput) missingArtifact(stage domain.PlanStage) string {
	if stage.Profile != "" {
		if _, err := input.Registry.Profile(stage.Profile); err != nil {
			return fmt.Sprintf("the stage pins agent profile %q and it is not installed", stage.Profile)
		}
	}
	if stage.ContextPolicy != "" {
		if _, err := input.Registry.ContextPolicy(stage.ContextPolicy); err != nil {
			return fmt.Sprintf("the stage names context policy %q and it is not installed", stage.ContextPolicy)
		}
	}
	return ""
}

func (input ResolveInput) contextPolicyFor(stage domain.PlanStage, profile domain.AgentProfile) (*domain.ContextPolicy, error) {
	id := stage.ContextPolicy
	if id == "" {
		id = profile.ContextPolicy
	}
	if id == "" {
		return nil, nil
	}
	policy, err := input.Registry.ContextPolicy(id)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func profileAdvertises(profile domain.AgentProfile, capability domain.EngineeringCapability) bool {
	for _, held := range profile.Capabilities {
		if held == capability {
			return true
		}
	}
	return false
}

// effectiveTrust is the stronger of what the stage requires and what the
// profile requires. Taking the maximum means a profile can tighten and a stage
// can tighten, and neither can loosen the other.
func effectiveTrust(stage domain.PlanStage, profile domain.AgentProfile) domain.TrustRequirement {
	if trustStrength(profile.TrustRequirement) > trustStrength(stage.TrustRequirement) {
		return profile.TrustRequirement
	}
	if stage.TrustRequirement == "" {
		return domain.TrustRequirementOperatorTrusted
	}
	return stage.TrustRequirement
}

func effectiveModel(profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor) string {
	if strings.TrimSpace(profile.Model) != "" {
		return profile.Model
	}
	return agent.Model
}

// assignmentID is derived from the exact plan revision and stage, so the same
// stage of the same revision is the same assignment - and a new revision
// produces a new one rather than mutating work already in flight.
func assignmentID(plan domain.EngineeringPlan, stage domain.PlanStage) string {
	return fmt.Sprintf("assignment-%s-r%d-%s", plan.ID, plan.Revision, stage.ID)
}

func boundedReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "no readiness detail was reported"
	}
	if len(detail) > 200 {
		return detail[:200]
	}
	return detail
}
