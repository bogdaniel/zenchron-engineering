package planning

// The deterministic plan compiler.
//
// It answers one question: given engineering intent, the facts, the compiled
// policy obligations and an optional reusable template, WHAT WORK IS REQUIRED?
// It answers it without a model, which is the point of building it first: every
// policy, independence, gate and budget law below is testable against fixtures
// with no live provider anywhere, and the reasoning planner in Phase 5 becomes
// a source of PROPOSALS that this same code validates.
//
// The order of authority never changes:
//
//	template     may propose stages, dependencies and preferences
//	policy       adds obligations and may not be weakened by anything
//	reasoning    proposes; it is never authority
//	this file    compiles and refuses
//	operator     approves
//
// A plan that would leave a compiled policy obligation unmet is refused rather
// than emitted, because a plan is what the operator approves: an obligation
// dropped here would be an obligation nobody sees again.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// CompilerVersion identifies this v0.1 plan compiler. It is recorded in plan
// provenance so a plan can be explained by the exact rules that produced it.
const CompilerVersion = "plan-compiler-v0.1"

// Default stage ids the compiler creates when nothing named one. They are
// stable and stated, so two compilations of the same input produce the same
// plan document rather than two documents differing by a generated name.
const (
	stageImplementation = "implementation"
	stageAssurance      = "assurance"
)

// CompileInput is everything the compiler reads. Facts, contract, template and
// envelope are explicit inputs so a plan can be reproduced from what it
// records.
type CompileInput struct {
	PlanID    string
	Revision  int
	Objective string
	Subject   domain.Subject
	// Contract is the COMPILED work contract. Its plan_requirements are the
	// policy obligations, already resolved by the one policy compiler; this
	// file never reads a policy document.
	Contract domain.EngineeringWorkContract
	Model    domain.ProjectModel
	Facts    []domain.EngineeringFact
	// Template is the operator's reusable process, or nil.
	Template *domain.EngineeringPlanTemplate
	// Envelope is the OPERATOR ceiling. A template may tighten it; nothing may
	// widen it here.
	Envelope domain.PlanBudgetEnvelope
	// Proposed is a reasoning agent's proposed stage set, or nil for a purely
	// deterministic compilation. It is a PROPOSAL: every law below applies to
	// it unchanged, and a proposal that violates one is refused.
	Proposed []domain.PlanStage
	// Reasoning is the provenance of the invocation that produced Proposed.
	Reasoning *domain.PlanReasoningProvenance
	// Previous is the revision this compilation replaces, when it is a
	// revision rather than a first plan.
	Previous   *domain.EngineeringPlan
	ProposalID string
}

// Compile produces a validated plan revision.
func Compile(input CompileInput) (domain.EngineeringPlan, error) {
	if err := input.validate(); err != nil {
		return domain.EngineeringPlan{}, err
	}
	requirements := domain.PlanRequirements{}
	if input.Contract.PlanRequirements != nil {
		requirements = *input.Contract.PlanRequirements
	}

	stages, err := input.baseStages()
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	stages = applyPolicyObligations(stages, requirements, input.Contract)
	stages = ensureAssuranceGate(stages, input.Contract)
	stages = fillStageDefaults(stages, input)
	stages = canonicalOrder(stages)

	envelope, err := resolveEnvelope(input, stages)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	stages = shareBudget(stages, envelope)

	plan := domain.EngineeringPlan{
		SchemaVersion:  domain.SchemaVersion,
		ID:             input.PlanID,
		Revision:       input.Revision,
		Objective:      input.Objective,
		Subject:        input.Subject,
		BudgetEnvelope: envelope,
		Stages:         stages,
		Provenance: domain.PlanProvenance{
			CompilerVersion: CompilerVersion,
			ProjectModel:    domain.ObjectRevision{ID: input.Model.ID, Revision: input.Model.Revision},
			Policy:          input.Contract.Provenance.Policy,
			Contract:        domain.ObjectRevision{ID: input.Contract.ID, Revision: input.Contract.Revision},
			Reasoning:       input.Reasoning,
			ProposalID:      input.ProposalID,
		},
	}
	if input.Template != nil {
		plan.Provenance.Template = &domain.TemplateRef{ID: input.Template.ID, Version: input.Template.Version, Digest: input.Template.Digest}
	}
	if input.Previous != nil {
		previous := input.Previous.Revision
		plan.Provenance.PreviousRevision = &previous
	}
	if err := Validate(plan, ValidationInput{Contract: input.Contract, Envelope: input.Envelope, Previous: input.Previous}); err != nil {
		return domain.EngineeringPlan{}, err
	}
	digest, err := plan.ContentDigest()
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	plan.Digest = digest
	if _, err := domain.Encode(plan); err != nil {
		return domain.EngineeringPlan{}, fmt.Errorf("compiled plan is invalid: %w", err)
	}
	return plan, nil
}

func (input CompileInput) validate() error {
	switch {
	case strings.TrimSpace(input.PlanID) == "":
		return fmt.Errorf("a plan needs an id")
	case input.Revision < 1:
		return fmt.Errorf("a plan revision starts at 1, got %d", input.Revision)
	case strings.TrimSpace(input.Objective) == "":
		return fmt.Errorf("a plan needs an objective")
	case input.Subject.Repository == "" || input.Subject.Revision == "":
		return fmt.Errorf("a plan subject needs a repository and a revision")
	case input.Contract.ID == "" || input.Contract.Revision == "":
		return fmt.Errorf("a plan compiles against an exact work contract revision")
	case input.Contract.Subject.Repository != input.Subject.Repository:
		return fmt.Errorf("the work contract governs %q and the plan subject is %q", input.Contract.Subject.Repository, input.Subject.Repository)
	}
	if input.Previous != nil && input.Previous.ID != input.PlanID {
		return fmt.Errorf("a revision replaces the SAME plan: %q cannot revise %q", input.PlanID, input.Previous.ID)
	}
	if input.Previous != nil && input.Revision <= input.Previous.Revision {
		return fmt.Errorf("revision %d does not follow revision %d", input.Revision, input.Previous.Revision)
	}
	return nil
}

// baseStages is what the plan starts from before policy is applied: a
// reasoning proposal, a template, or the minimum deterministic decomposition.
//
// The minimum is deliberately SMALL. A trivial change compiles to one
// implementation stage and the assurance the contract already requires; adding
// an architect or a security reviewer that no obligation asked for would be the
// rigid workflow #64 exists to avoid.
func (input CompileInput) baseStages() ([]domain.PlanStage, error) {
	switch {
	case len(input.Proposed) > 0:
		return append([]domain.PlanStage{}, input.Proposed...), nil
	case input.Template != nil:
		return templateStages(*input.Template, input.Facts), nil
	}
	stages := []domain.PlanStage{{
		ID:        stageImplementation,
		Kind:      domain.StageAgent,
		Role:      domain.RoleImplementer,
		Objective: input.Objective,
		Rationale: "the objective requires a change, and nothing in policy or a template asked for more",
	}}
	if claims := contractClaims(input.Contract); len(claims) > 0 {
		stages = append(stages, domain.PlanStage{
			ID:             stageAssurance,
			Kind:           domain.StageAssuranceGate,
			DependsOn:      []string{stageImplementation},
			RequiredClaims: claims,
			Rationale:      "the work contract already requires this evidence; the gate references it and creates no run",
		})
	}
	return stages, nil
}

// templateStages converts the selected template stages into plan stages. A
// template contributes SHAPE - ids, kinds, roles, dependencies, preferences -
// and never authority.
func templateStages(template domain.EngineeringPlanTemplate, facts []domain.EngineeringFact) []domain.PlanStage {
	selected := TemplateStagesFor(template, facts)
	stages := make([]domain.PlanStage, 0, len(selected))
	for _, stage := range selected {
		stages = append(stages, domain.PlanStage{
			ID:                   stage.ID,
			Kind:                 stage.Kind,
			Role:                 stage.Role,
			Objective:            stage.Objective,
			DependsOn:            stage.DependsOn,
			RequiresCapabilities: stage.RequiresCapabilities,
			Independence:         stage.Independence,
			Profile:              stage.Profile,
			ContextPolicy:        stage.ContextPolicy,
			RequiredClaims:       stage.RequiredClaims,
			Action:               stage.Action,
			Rationale:            "requested by plan template " + template.ID,
		})
	}
	return stages
}

// contractClaims are the claim ids the work contract requires, in canonical
// order. A gate references them; it does not invent a second evidence model.
func contractClaims(contract domain.EngineeringWorkContract) []string {
	claims := make([]string, 0, len(contract.RequiredClaims))
	for id := range contract.RequiredClaims {
		claims = append(claims, id)
	}
	sort.Strings(claims)
	return claims
}

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

// ---------------------------------------------------------------------------
// Policy obligations
// ---------------------------------------------------------------------------

// applyPolicyObligations makes the plan satisfy every compiled obligation.
//
// It only ever ADDS or STRENGTHENS. A template that named a security reviewer
// gets that stage strengthened to the policy's independence dimension; a
// template that named none gets the stage added. Nothing here can remove a
// stage or weaken a requirement, which is the mechanism behind "policy wins or
// the plan fails closed".
func applyPolicyObligations(stages []domain.PlanStage, requirements domain.PlanRequirements, contract domain.EngineeringWorkContract) []domain.PlanStage {
	for _, role := range requirements.Roles {
		stages = applyRoleObligation(stages, role)
	}
	stages = applyCapabilityObligations(stages, requirements.Capabilities)
	for _, gate := range requirements.Gates {
		stages = applyGateObligation(stages, gate, contract)
	}
	return stages
}

func applyRoleObligation(stages []domain.PlanStage, requirement domain.RoleRequirement) []domain.PlanStage {
	producers := materialProducerStages(stages)
	index := -1
	for i, stage := range stages {
		if stage.Kind == domain.StageAgent && stage.Role == requirement.Role {
			index = i
			break
		}
	}
	if index < 0 {
		// The obligation names a responsibility no stage fulfils, so the plan
		// gains one. It depends on the material producers because a reviewing
		// role reviews something: a review stage with no dependency would be
		// schedulable before the work it reviews exists.
		stage := domain.PlanStage{
			ID:        stageIDForRole(stages, requirement.Role),
			Kind:      domain.StageAgent,
			Role:      requirement.Role,
			DependsOn: producers,
			Rationale: "required by EngineeringPolicy: " + requirement.Statement,
		}
		stages = append(stages, stage)
		index = len(stages) - 1
	}
	stage := stages[index]
	stage.RequiresCapabilities = mergeCapabilities(stage.RequiresCapabilities, requirement.Capabilities)
	if trustStrength(requirement.TrustRequirement) > trustStrength(stage.TrustRequirement) {
		stage.TrustRequirement = requirement.TrustRequirement
	}
	if requirement.Independence != nil {
		stage.Independence = strengthenIndependence(stage.Independence, *requirement.Independence, producers, stage.ID)
	}
	if stage.Rationale == "" {
		stage.Rationale = "required by EngineeringPolicy: " + requirement.Statement
	}
	stages[index] = stage
	return stages
}

// strengthenIndependence takes the stronger dimension and the union of the
// stages a producer must differ from. A weaker existing requirement is never
// kept: silently degrading a required independence class is the exact failure
// #64 names.
func strengthenIndependence(existing *domain.IndependenceRequirement, required domain.IndependenceRequirement, producers []string, self string) *domain.IndependenceRequirement {
	result := required
	if existing != nil && dimensionStrength(existing.Dimension) > dimensionStrength(required.Dimension) {
		result.Dimension = existing.Dimension
	}
	from := map[string]bool{}
	if existing != nil {
		for _, id := range existing.DifferentFrom {
			from[id] = true
		}
	}
	for _, id := range required.DifferentFrom {
		from[id] = true
	}
	// Policy names a RELATIONSHIP - independent of the material producer - and
	// the compiler binds it to the exact stages that produce material change in
	// this plan. That binding is what makes the obligation checkable.
	for _, id := range producers {
		from[id] = true
	}
	delete(from, self)
	result.DifferentFrom = sortedKeysOf(from)
	// The substitution PERMISSION comes from policy alone. An existing stage
	// requirement cannot introduce one, so the merge takes policy's value.
	result.HumanSubstitutionPermitted = required.HumanSubstitutionPermitted
	return &result
}

func applyCapabilityObligations(stages []domain.PlanStage, capabilities []domain.EngineeringCapability) []domain.PlanStage {
	for _, capability := range capabilities {
		if capabilityCovered(stages, capability) {
			continue
		}
		// A plan-level capability obligation attaches to the first material
		// producer, because that is the stage the plan cannot omit. If the plan
		// has no producer at all, it attaches to the first agent stage.
		index := firstProducerIndex(stages)
		if index < 0 {
			continue
		}
		stages[index].RequiresCapabilities = mergeCapabilities(stages[index].RequiresCapabilities, []domain.EngineeringCapability{capability})
	}
	return stages
}

func applyGateObligation(stages []domain.PlanStage, gate domain.GateRequirement, contract domain.EngineeringWorkContract) []domain.PlanStage {
	claims := gate.RequiredClaims
	if len(claims) == 0 && gate.Kind == domain.StageAssuranceGate {
		claims = contractClaims(contract)
	}
	for i, stage := range stages {
		if stage.Kind != gate.Kind || !sameAction(stage.Action, gate.Action) {
			continue
		}
		stages[i].RequiredClaims = mergeStrings(stage.RequiredClaims, claims)
		return stages
	}
	// The gate depends on everything that could still change what it judges.
	// An assurance gate placed before the last producer would be assurance of
	// something other than the work.
	stage := domain.PlanStage{
		ID:             stageIDForGate(stages, gate.Kind),
		Kind:           gate.Kind,
		DependsOn:      agentStages(stages),
		RequiredClaims: claims,
		Action:         gate.Action,
		Rationale:      "required by EngineeringPolicy: " + gate.Statement,
	}
	return append(stages, stage)
}

// ensureAssuranceGate adds the assurance the CONTRACT already requires when a
// plan has none.
//
// It is a completion in one direction: the gate references claims the contract
// defines, depends on every agent stage, and creates no run. A plan that
// produced a change and referenced no assurance at all would leave the evidence
// the contract requires outside the plan an operator approved - which is not a
// smaller plan, it is a plan that says less than the truth.
func ensureAssuranceGate(stages []domain.PlanStage, contract domain.EngineeringWorkContract) []domain.PlanStage {
	claims := contractClaims(contract)
	if len(claims) == 0 {
		return stages
	}
	for _, stage := range stages {
		if stage.Kind == domain.StageAssuranceGate {
			return stages
		}
	}
	producers := agentStages(stages)
	if len(producers) == 0 {
		return stages
	}
	return append(stages, domain.PlanStage{
		ID:             uniqueStageID(stages, stageAssurance),
		Kind:           domain.StageAssuranceGate,
		DependsOn:      producers,
		RequiredClaims: claims,
		Rationale:      "the work contract requires this evidence; the gate references it and creates no run",
	})
}

// ---------------------------------------------------------------------------
// Defaults and canonical form
// ---------------------------------------------------------------------------

// fillStageDefaults completes what the plan states from what the role means.
// Every default is derived from stated data - the role catalogue, the contract,
// the plan objective - so two compilations agree.
func fillStageDefaults(stages []domain.PlanStage, input CompileInput) []domain.PlanStage {
	for i, stage := range stages {
		if stage.Kind != domain.StageAgent {
			// A gate carries no worker requirement at all. Clearing rather than
			// ignoring means a template that stated one cannot leave a residue
			// that later reads as a worker requirement.
			stage.RequiresCapabilities = nil
			stage.TrustRequirement = ""
			stage.InvocationMode = ""
			stage.Profile = ""
			// An assurance gate that names no claim is completed from the work
			// contract's own required claims. It is a safe completion in one
			// direction only: the gate can gain claims the contract already
			// requires and can never lose one, so a proposal that simply said
			// "and then assurance" means the assurance this contract defines
			// rather than a gate nothing could satisfy. Where the contract
			// requires nothing, the refusal stands - there would be nothing to
			// prove.
			if stage.Kind == domain.StageAssuranceGate && len(stage.RequiredClaims) == 0 {
				stage.RequiredClaims = contractClaims(input.Contract)
			}
			stages[i] = stage
			continue
		}
		stage.RequiresCapabilities = mergeCapabilities(stage.RequiresCapabilities, RoleCapabilities(stage.Role))
		// An independence requirement that names no stage is BOUND to the
		// material producers, exactly as a policy obligation is. Policy states a
		// relationship - "independent of whoever produced the change" - and so
		// does a proposal that asks for independence without knowing which stage
		// will produce; binding it is what makes the obligation checkable, and
		// it can only ever add constraints.
		if stage.Independence != nil && len(stage.Independence.DifferentFrom) == 0 {
			independence := *stage.Independence
			independence.DifferentFrom = producersExcept(stages, stage.ID)
			stage.Independence = &independence
		}
		if stage.InvocationMode == "" {
			stage.InvocationMode = RequiredInvocationMode(stage.Role)
		}
		if stage.Objective == "" {
			stage.Objective = input.Objective
		}
		stages[i] = stage
	}
	return stages
}

// canonicalOrder sorts stages into dependency order, breaking ties by id.
//
// It is not cosmetic. The plan document is content-addressed, an assignment
// pins that digest, and an operator approves what they read - so two
// compilations of the same input have to produce the same document, not the
// same set in whatever order a map iteration produced.
func canonicalOrder(stages []domain.PlanStage) []domain.PlanStage {
	byID := make(map[string]domain.PlanStage, len(stages))
	remaining := make(map[string]int, len(stages))
	for _, stage := range stages {
		byID[stage.ID] = stage
		remaining[stage.ID] = len(stage.DependsOn)
	}
	// A dependency on a stage that does not exist cannot be ordered. It is left
	// for validation to refuse with a useful message rather than dropped here.
	for _, stage := range stages {
		for _, dependency := range stage.DependsOn {
			if _, ok := byID[dependency]; !ok {
				return stages
			}
		}
	}
	dependents := map[string][]string{}
	for _, stage := range stages {
		for _, dependency := range stage.DependsOn {
			dependents[dependency] = append(dependents[dependency], stage.ID)
		}
	}
	ready := []string{}
	for id, count := range remaining {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	ordered := make([]domain.PlanStage, 0, len(stages))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		ordered = append(ordered, byID[id])
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
	if len(ordered) != len(stages) {
		// A cycle. Validation refuses it and names the loop; ordering it here
		// would only hide which stages are involved.
		return stages
	}
	for i := range ordered {
		ordered[i].DependsOn = sortedStrings(ordered[i].DependsOn)
	}
	return ordered
}

// ---------------------------------------------------------------------------
// Budgets
// ---------------------------------------------------------------------------

// resolveEnvelope intersects the operator ceiling with a template's request.
//
// A template may TIGHTEN. It may not widen, and the refusal is silent
// clamping's opposite: the tighter of the two always wins, so a template asking
// for more simply gets the operator's number.
func resolveEnvelope(input CompileInput, stages []domain.PlanStage) (domain.PlanBudgetEnvelope, error) {
	envelope := input.Envelope
	if envelope.MaxChildRuns <= 0 || envelope.MaxConcurrency <= 0 || envelope.MaxProviderInvocations <= 0 {
		return domain.PlanBudgetEnvelope{}, fmt.Errorf("a plan needs an operator budget envelope: max_child_runs, max_concurrency and max_provider_invocations are all required")
	}
	if input.Template != nil && input.Template.BudgetEnvelope != nil {
		envelope = tighten(envelope, *input.Template.BudgetEnvelope)
	}
	agents := 0
	for _, stage := range stages {
		if stage.Kind == domain.StageAgent {
			agents++
		}
	}
	if agents > envelope.MaxChildRuns {
		return domain.PlanBudgetEnvelope{}, fmt.Errorf("the plan needs %d child runs and the envelope allows %d: raise max_child_runs through operator configuration or approve a smaller plan", agents, envelope.MaxChildRuns)
	}
	if envelope.MaxProviderInvocations < agents {
		return domain.PlanBudgetEnvelope{}, fmt.Errorf("the plan needs at least %d provider invocations and the envelope allows %d", agents, envelope.MaxProviderInvocations)
	}
	return envelope, nil
}

// tighten takes the smaller of every stated bound. An absent bound in the
// proposal leaves the ceiling unchanged; a monetary ceiling that the operator
// did not state stays UNKNOWN rather than becoming a number.
func tighten(ceiling, proposal domain.PlanBudgetEnvelope) domain.PlanBudgetEnvelope {
	result := ceiling
	result.MaxChildRuns = minPositive(ceiling.MaxChildRuns, proposal.MaxChildRuns)
	result.MaxConcurrency = minPositive(ceiling.MaxConcurrency, proposal.MaxConcurrency)
	result.MaxProviderInvocations = minPositive(ceiling.MaxProviderInvocations, proposal.MaxProviderInvocations)
	result.MaxWallSeconds = minPositive(ceiling.MaxWallSeconds, proposal.MaxWallSeconds)
	switch {
	case proposal.MaxCostMicros == nil:
	case ceiling.MaxCostMicros == nil:
		// The operator stated no monetary ceiling, so there is none to tighten:
		// an unknown ceiling is not zero, and a proposal cannot invent one.
	case *proposal.MaxCostMicros < *ceiling.MaxCostMicros:
		tighter := *proposal.MaxCostMicros
		result.MaxCostMicros = &tighter
	}
	return result
}

func minPositive(ceiling, proposal int) int {
	if proposal > 0 && proposal < ceiling {
		return proposal
	}
	return ceiling
}

// shareBudget gives each agent stage a bound derived from the envelope. It is
// a SHARE, not a new budget: the per-run budgets the runtime already enforces
// remain authoritative, and this bounds what one stage may ask of them.
func shareBudget(stages []domain.PlanStage, envelope domain.PlanBudgetEnvelope) []domain.PlanStage {
	agents := 0
	for _, stage := range stages {
		if stage.Kind == domain.StageAgent {
			agents++
		}
	}
	if agents == 0 {
		return stages
	}
	attempts := envelope.MaxProviderInvocations / agents
	if attempts < 1 {
		attempts = 1
	}
	wall := 0
	if envelope.MaxWallSeconds > 0 {
		wall = envelope.MaxWallSeconds / agents
	}
	for i, stage := range stages {
		if stage.Kind != domain.StageAgent {
			continue
		}
		if stage.Budget.MaxExecutionAttempts <= 0 || stage.Budget.MaxExecutionAttempts > attempts {
			stage.Budget.MaxExecutionAttempts = attempts
		}
		if wall > 0 && (stage.Budget.MaxWallSeconds <= 0 || stage.Budget.MaxWallSeconds > wall) {
			stage.Budget.MaxWallSeconds = wall
		}
		stages[i] = stage
	}
	return stages
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// producersExcept is the material producers other than one stage. A reviewer
// that produced nothing is independent of itself trivially, and naming itself
// would make the obligation unsatisfiable.
func producersExcept(stages []domain.PlanStage, self string) []string {
	var producers []string
	for _, id := range materialProducerStages(stages) {
		if id != self {
			producers = append(producers, id)
		}
	}
	return producers
}

func materialProducerStages(stages []domain.PlanStage) []string {
	var producers []string
	for _, stage := range stages {
		if stage.Kind == domain.StageAgent && ProducesMaterialChange(stage.Role) {
			producers = append(producers, stage.ID)
		}
	}
	sort.Strings(producers)
	return producers
}

func firstProducerIndex(stages []domain.PlanStage) int {
	for i, stage := range stages {
		if stage.Kind == domain.StageAgent && ProducesMaterialChange(stage.Role) {
			return i
		}
	}
	for i, stage := range stages {
		if stage.Kind == domain.StageAgent {
			return i
		}
	}
	return -1
}

func agentStages(stages []domain.PlanStage) []string {
	var ids []string
	for _, stage := range stages {
		if stage.Kind == domain.StageAgent {
			ids = append(ids, stage.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func capabilityCovered(stages []domain.PlanStage, capability domain.EngineeringCapability) bool {
	for _, stage := range stages {
		if stage.Kind != domain.StageAgent {
			continue
		}
		for _, held := range stage.RequiresCapabilities {
			if held == capability {
				return true
			}
		}
	}
	return false
}

// stageIDForRole names a policy-added stage after the responsibility it
// fulfils, disambiguating only when it has to. A generated name would make two
// compilations of the same input produce two different documents.
func stageIDForRole(stages []domain.PlanStage, role domain.EngineeringRole) string {
	return uniqueStageID(stages, strings.ReplaceAll(string(role), "_", "-"))
}

func stageIDForGate(stages []domain.PlanStage, kind domain.StageKind) string {
	base := "assurance"
	if kind == domain.StageHumanDecisionGate {
		base = "human-decision"
	}
	return uniqueStageID(stages, base)
}

func uniqueStageID(stages []domain.PlanStage, base string) string {
	taken := map[string]bool{}
	for _, stage := range stages {
		taken[stage.ID] = true
	}
	if !taken[base] {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if !taken[candidate] {
			return candidate
		}
	}
}

func mergeCapabilities(existing, added []domain.EngineeringCapability) []domain.EngineeringCapability {
	seen := map[domain.EngineeringCapability]bool{}
	for _, capability := range append(append([]domain.EngineeringCapability{}, existing...), added...) {
		seen[capability] = true
	}
	result := make([]domain.EngineeringCapability, 0, len(seen))
	for _, capability := range domain.EngineeringCapabilities() {
		if seen[capability] {
			result = append(result, capability)
		}
	}
	return result
}

func mergeStrings(existing, added []string) []string {
	seen := map[string]bool{}
	for _, value := range append(append([]string{}, existing...), added...) {
		if value != "" {
			seen[value] = true
		}
	}
	return sortedKeysOf(seen)
}

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func sortedKeysOf(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sameAction(left, right *domain.Action) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}
