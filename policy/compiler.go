// Package policy deterministically resolves fact-driven policy rules into a
// bounded engineering work contract. It contains no execution-provider logic.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// CompilerVersion identifies this v0.1 deterministic compiler.
const CompilerVersion = "compiler-v0.1"

// CompileInput provides the non-policy work context needed to create a
// contract. Facts, model, and policy remain explicit compiler inputs so a
// caller can reproduce the resulting contract.
type CompileInput struct {
	ContractID       string
	ContractRevision string
	Objective        string
	AcceptanceIntent []string
	Subject          domain.Subject
	Scope            domain.ContractScope
	ProjectModel     domain.ProjectModel
	Policy           domain.EngineeringPolicy
	Facts            []domain.EngineeringFact
	PreviousContract *domain.EngineeringWorkContract
}

// Compile resolves all matching policy rules without relying on map or input
// ordering. Conflicting policy outcomes are rejected rather than selected by
// an implicit precedence rule.
func Compile(input CompileInput) (domain.EngineeringWorkContract, error) {
	if err := validateInput(input); err != nil {
		return domain.EngineeringWorkContract{}, err
	}

	facts, err := normalizedFacts(input.Facts, input.Subject)
	if err != nil {
		return domain.EngineeringWorkContract{}, err
	}
	state := newResolution()
	for _, fact := range facts {
		if fact.Value == domain.FactUnknown {
			if err := state.addUnknownResolution(fact); err != nil {
				return domain.EngineeringWorkContract{}, err
			}
		}
	}

	ruleIDs := make([]string, 0, len(input.Policy.Rules))
	for id := range input.Policy.Rules {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)
	for _, id := range ruleIDs {
		rule := input.Policy.Rules[id]
		if !matchesAny(rule.When, facts) {
			continue
		}
		if err := state.addEffect(id, rule.Effect); err != nil {
			return domain.EngineeringWorkContract{}, err
		}
	}
	// Source-derived acceptance becomes EXPLICIT OBLIGATIONS. Acceptance intent
	// used to travel through a contract as prose that nothing could test, so a
	// run could satisfy every claim policy named and still have discharged
	// nothing about the work it was asked to do. Each criterion becomes an
	// obligation with a content-derived stable id, marked material, discharged
	// by the claims policy designated for acceptance.
	if err := state.addAcceptanceObligations(input.AcceptanceIntent); err != nil {
		return domain.EngineeringWorkContract{}, err
	}
	if err := state.validateReferences(); err != nil {
		return domain.EngineeringWorkContract{}, err
	}
	// Fail closed: a material obligation nothing can discharge would read as
	// satisfied forever, which is exactly the silence this model exists to
	// remove.
	if err := state.validateMaterialDischarge(); err != nil {
		return domain.EngineeringWorkContract{}, err
	}
	if err := rejectPermissionExpansion(input.PreviousContract, state.permissions); err != nil {
		return domain.EngineeringWorkContract{}, err
	}

	contract := domain.EngineeringWorkContract{
		SchemaVersion:    domain.SchemaVersion,
		ID:               input.ContractID,
		Revision:         input.ContractRevision,
		Objective:        input.Objective,
		AcceptanceIntent: sortedUnique(input.AcceptanceIntent),
		Subject:          input.Subject,
		Scope: domain.ContractScope{
			Stage:           input.Scope.Stage,
			AllowedPaths:    sortedUnique(input.Scope.AllowedPaths),
			ProhibitedPaths: sortedUnique(input.Scope.ProhibitedPaths),
		},
		Facts:               factIDs(facts),
		Invariants:          state.invariants,
		Obligations:         state.obligations,
		RequiredClaims:      state.claims,
		Permissions:         sortedActions(state.permissions),
		Prohibitions:        sortedActions(state.prohibitions),
		AuthorityConditions: sortedConditions(state.conditions),
		PlanRequirements:    state.planRequirements(),
		Provenance: domain.ContractProvenance{
			ProjectModel:             domain.ObjectRevision{ID: input.ProjectModel.ID, Revision: input.ProjectModel.Revision},
			Policy:                   domain.ObjectRevision{ID: input.Policy.ID, Revision: input.Policy.Revision},
			CompilerVersion:          CompilerVersion,
			PreviousContractRevision: previousRevision(input.PreviousContract),
		},
	}
	if _, err := domain.Encode(contract); err != nil {
		return domain.EngineeringWorkContract{}, fmt.Errorf("compiled contract is invalid: %w", err)
	}
	return contract, nil
}

func validateInput(input CompileInput) error {
	if input.ContractID == "" || input.ContractRevision == "" || input.Objective == "" {
		return fmt.Errorf("contract id, revision, and objective are required")
	}
	if input.Subject.Repository == "" || input.Subject.Revision == "" {
		return fmt.Errorf("contract subject requires repository and revision")
	}
	if len(input.AcceptanceIntent) == 0 || len(input.Scope.AllowedPaths) == 0 {
		return fmt.Errorf("acceptance intent and allowed scope paths are required")
	}
	if input.Scope.Stage != domain.StagePredicted && input.Scope.Stage != domain.StageObserved && input.Scope.Stage != domain.StageVerified {
		return fmt.Errorf("invalid scope stage %q", input.Scope.Stage)
	}
	if input.ProjectModel.Subject.Repository != input.Subject.Repository {
		return fmt.Errorf("ProjectModel repository does not match contract subject")
	}
	if input.Scope.Stage == domain.StagePredicted && input.ProjectModel.Subject != input.Subject {
		return fmt.Errorf("predicted contract subject does not match ProjectModel subject")
	}
	if input.PreviousContract != nil {
		if input.PreviousContract.Subject.Repository != input.Subject.Repository {
			return fmt.Errorf("previous contract repository does not match contract subject")
		}
		if input.PreviousContract.ID != input.ContractID {
			return fmt.Errorf("previous contract id does not match contract id")
		}
	}
	if _, err := domain.Encode(input.ProjectModel); err != nil {
		return fmt.Errorf("invalid ProjectModel: %w", err)
	}
	if _, err := domain.Encode(input.Policy); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

func normalizedFacts(facts []domain.EngineeringFact, subject domain.Subject) ([]domain.EngineeringFact, error) {
	byID := make(map[string]domain.EngineeringFact, len(facts))
	for _, fact := range facts {
		if fact.Subject != subject {
			return nil, fmt.Errorf("fact %q subject does not match contract subject", fact.ID)
		}
		if _, err := domain.Encode(fact); err != nil {
			return nil, fmt.Errorf("invalid fact %q: %w", fact.ID, err)
		}
		if _, exists := byID[fact.ID]; exists {
			return nil, fmt.Errorf("duplicate fact identity %q", fact.ID)
		}
		byID[fact.ID] = fact
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]domain.EngineeringFact, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result, nil
}

func matchesAny(condition domain.PolicyCondition, facts []domain.EngineeringFact) bool {
	for _, fact := range facts {
		if fact.Key != condition.Fact || fact.Value != condition.Equals {
			continue
		}
		if condition.Stage != nil && fact.Stage != *condition.Stage ||
			condition.Confidence != nil && fact.Confidence != *condition.Confidence {
			continue
		}
		if condition.Provenance != nil &&
			(condition.Provenance.Type != nil && fact.Provenance.Type != *condition.Provenance.Type ||
				condition.Provenance.Producer != nil && fact.Provenance.Producer != *condition.Provenance.Producer) {
			continue
		}
		return true
	}
	return false
}

type resolution struct {
	invariants            map[string]domain.Requirement
	invariantDefinitions  map[string]policyRequirement
	obligations           map[string]domain.Requirement
	obligationDefinitions map[string]policyRequirement
	claims                map[string]domain.RequiredClaim
	permissions           map[domain.Action]string
	prohibitions          map[domain.Action]string
	conditions            map[domain.Action]conditionEntry
	references            map[string]string
	// acceptanceDischarge is what policy says it takes to believe the run's own
	// acceptance criteria were met.
	acceptanceDischarge []string
	// roles, capabilities and gates are the PLAN-SHAPED obligations. They
	// resolve here, through this compiler, rather than in a planner-local rule
	// engine: role, capability, independence and gate requirements are
	// governance, and this repository has exactly one governance compiler.
	roles        map[domain.EngineeringRole]domain.RoleRequirement
	capabilities map[domain.EngineeringCapability]bool
	gates        map[string]domain.GateRequirement
}

type conditionEntry struct {
	claims []string
	rule   string
}

// policyRequirement is the normalized policy-only definition used to detect
// conflicts before requirements are emitted in the WorkContract shape.
type policyRequirement struct {
	statement      string
	requiredClaims []string
	material       bool
}

func newResolution() *resolution {
	return &resolution{
		invariants:            make(map[string]domain.Requirement),
		invariantDefinitions:  make(map[string]policyRequirement),
		obligations:           make(map[string]domain.Requirement),
		obligationDefinitions: make(map[string]policyRequirement),
		claims:                make(map[string]domain.RequiredClaim),
		permissions:           make(map[domain.Action]string),
		prohibitions:          make(map[domain.Action]string),
		conditions:            make(map[domain.Action]conditionEntry),
		references:            make(map[string]string),
		roles:                 make(map[domain.EngineeringRole]domain.RoleRequirement),
		capabilities:          make(map[domain.EngineeringCapability]bool),
		gates:                 make(map[string]domain.GateRequirement),
	}
}

func (r *resolution) addUnknownResolution(fact domain.EngineeringFact) error {
	id := "resolve-uncertain-" + fact.ID
	return r.addRequirement(r.obligations, r.obligationDefinitions, id, domain.PolicyRequirement{Statement: "Resolve uncertainty for engineering fact " + fact.Key + "."}, "unknown fact "+fact.ID)
}

func (r *resolution) addEffect(ruleID string, effect domain.PolicyEffect) error {
	if effect.AcceptanceDischargeClaims != nil {
		r.acceptanceDischarge = append(r.acceptanceDischarge, *effect.AcceptanceDischargeClaims...)
	}
	if effect.Invariants != nil {
		for id, requirement := range *effect.Invariants {
			if err := r.addRequirement(r.invariants, r.invariantDefinitions, id, requirement, ruleID); err != nil {
				return err
			}
		}
	}
	if effect.Obligations != nil {
		for id, requirement := range *effect.Obligations {
			if err := r.addRequirement(r.obligations, r.obligationDefinitions, id, requirement, ruleID); err != nil {
				return err
			}
		}
	}
	if effect.RequiredClaims != nil {
		for id, claim := range *effect.RequiredClaims {
			if existing, exists := r.claims[id]; exists && existing != claim {
				return fmt.Errorf("conflicting required claim %q from rule %q", id, ruleID)
			}
			r.claims[id] = claim
		}
	}
	if effect.Permissions != nil {
		for _, action := range *effect.Permissions {
			r.permissions[action] = ruleID
		}
	}
	if effect.Prohibitions != nil {
		for _, action := range *effect.Prohibitions {
			r.prohibitions[action] = ruleID
		}
	}
	if effect.EngineeringRequirements != nil {
		if err := r.addEngineeringRequirements(ruleID, *effect.EngineeringRequirements); err != nil {
			return err
		}
	}
	if effect.AuthorityConditions != nil {
		for _, condition := range *effect.AuthorityConditions {
			claims := sortedUnique(condition.RequiredClaims)
			if existing, exists := r.conditions[condition.Action]; exists && !sameStrings(existing.claims, claims) {
				return fmt.Errorf("conflicting authority conditions for %s:%s from rules %q and %q", condition.Action.Type, condition.Action.Target, existing.rule, ruleID)
			}
			r.conditions[condition.Action] = conditionEntry{claims: claims, rule: ruleID}
		}
	}
	return nil
}

func (r *resolution) addRequirement(target map[string]domain.Requirement, definitions map[string]policyRequirement, id string, requirement domain.PolicyRequirement, source string) error {
	definition := normalizePolicyRequirement(requirement)
	if existing, exists := definitions[id]; exists && !samePolicyRequirement(existing, definition) {
		return fmt.Errorf("conflicting requirement %q from %s", id, source)
	}
	definitions[id] = definition
	// The discharge relationship policy already expressed is CARRIED into the
	// contract instead of being dropped. Without it a compiled obligation is a
	// sentence nobody can test, and a protected action can be authorized while
	// it is outstanding.
	target[id] = domain.Requirement{
		Statement:      definition.statement,
		RequiredClaims: definition.requiredClaims,
		Material:       definition.material,
	}
	for _, claim := range definition.requiredClaims {
		r.references[claim] = source
	}
	return nil
}

func normalizePolicyRequirement(requirement domain.PolicyRequirement) policyRequirement {
	var claims []string
	if requirement.RequiredClaims != nil {
		claims = sortedUnique(*requirement.RequiredClaims)
	}
	return policyRequirement{statement: requirement.Statement, requiredClaims: claims, material: requirement.Material}
}

func samePolicyRequirement(left, right policyRequirement) bool {
	return left.statement == right.statement &&
		left.material == right.material &&
		sameStrings(left.requiredClaims, right.requiredClaims)
}

func (r *resolution) validateReferences() error {
	permissionActions := make([]domain.Action, 0, len(r.permissions))
	for action := range r.permissions {
		permissionActions = append(permissionActions, action)
	}
	sort.Slice(permissionActions, func(i, j int) bool { return actionKey(permissionActions[i]) < actionKey(permissionActions[j]) })
	for _, action := range permissionActions {
		if prohibitionRule, prohibited := r.prohibitions[action]; prohibited {
			return fmt.Errorf("conflicting permission and prohibition for %s:%s from rules %q and %q", action.Type, action.Target, r.permissions[action], prohibitionRule)
		}
	}
	for _, condition := range r.conditions {
		for _, claim := range condition.claims {
			r.references[claim] = condition.rule
		}
	}
	claims := make([]string, 0, len(r.references))
	for claim := range r.references {
		claims = append(claims, claim)
	}
	sort.Strings(claims)
	for _, claim := range claims {
		if _, exists := r.claims[claim]; !exists {
			return fmt.Errorf("policy outcome from rule %q references undefined required claim %q", r.references[claim], claim)
		}
	}
	return nil
}

func factIDs(facts []domain.EngineeringFact) []string {
	ids := make([]string, len(facts))
	for i, fact := range facts {
		ids[i] = fact.ID
	}
	return ids
}

func previousRevision(contract *domain.EngineeringWorkContract) *string {
	if contract == nil {
		return nil
	}
	return &contract.Revision
}

func rejectPermissionExpansion(previous *domain.EngineeringWorkContract, permissions map[domain.Action]string) error {
	if previous == nil {
		return nil
	}
	allowed := make(map[domain.Action]struct{}, len(previous.Permissions))
	for _, action := range previous.Permissions {
		allowed[action] = struct{}{}
	}
	for action := range permissions {
		if _, exists := allowed[action]; !exists {
			return fmt.Errorf("recompilation cannot add permission for %s:%s", action.Type, action.Target)
		}
	}
	return nil
}

func sortedUnique(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	sort.Strings(result)
	return uniqueStrings(result)
}

func uniqueStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortedActions(actions map[domain.Action]string) []domain.Action {
	result := make([]domain.Action, 0, len(actions))
	for action := range actions {
		result = append(result, action)
	}
	sort.Slice(result, func(i, j int) bool { return actionKey(result[i]) < actionKey(result[j]) })
	return result
}

func sortedConditions(conditions map[domain.Action]conditionEntry) []domain.AuthorityCondition {
	actions := make([]domain.Action, 0, len(conditions))
	for action := range conditions {
		actions = append(actions, action)
	}
	sort.Slice(actions, func(i, j int) bool { return actionKey(actions[i]) < actionKey(actions[j]) })
	result := make([]domain.AuthorityCondition, 0, len(actions))
	for _, action := range actions {
		result = append(result, domain.AuthorityCondition{Action: action, RequiredClaims: conditions[action].claims})
	}
	return result
}

func actionKey(action domain.Action) string {
	return action.Type + "\x00" + action.Target
}

// addAcceptanceObligations turns each source acceptance criterion into a stable,
// material obligation. The id is derived from the criterion's own text, so the
// same criterion is the same obligation in every contract that carries it and
// across every recompilation - never positional and never generated.
//
// A criterion whose discharge policy has not designated is left with NO claims
// on purpose: validateMaterialDischarge then refuses the contract. Silence about
// how something is proven is not the same as it being proven.
func (r *resolution) addAcceptanceObligations(criteria []string) error {
	discharge := sortedUnique(r.acceptanceDischarge)
	for _, criterion := range sortedUnique(criteria) {
		if strings.TrimSpace(criterion) == "" {
			continue
		}
		id := domain.AcceptanceObligationID(criterion)
		requirement := domain.PolicyRequirement{Statement: criterion, Material: true}
		if len(discharge) > 0 {
			claims := append([]string(nil), discharge...)
			requirement.RequiredClaims = &claims
		}
		if err := r.addRequirement(r.obligations, r.obligationDefinitions, id, requirement, "source acceptance criterion"); err != nil {
			return err
		}
	}
	return nil
}

// validateMaterialDischarge refuses a contract carrying a material obligation
// with no discharge claim. Such an obligation can never be met, so authorizing
// a protected action past it would be authorizing past an unanswerable
// question.
func (r *resolution) validateMaterialDischarge() error {
	// The rule is scoped to contracts that can actually authorize something. A
	// contract granting no permission and naming no authority condition cannot
	// authorize anything whatever its obligations say, so refusing to compile
	// it would refuse a run the facts simply did not govern. Where a protected
	// action DOES exist, a material obligation nothing can discharge is refused
	// outright rather than left to read as satisfied.
	if len(r.permissions) == 0 && len(r.conditions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(r.obligations))
	for id := range r.obligations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		obligation := r.obligations[id]
		if obligation.Material && len(obligation.RequiredClaims) == 0 {
			return fmt.Errorf("material obligation %q has no discharge claim: policy must name what discharges it", id)
		}
	}
	return nil
}

// MaterialDischargeClaims are the claims that must be satisfied before ANY
// protected action may be authorized, because they discharge obligations the
// contract marks material to acceptance. In M0 a material obligation applies to
// every protected action: that is the conservative reading, and narrowing it
// would need policy to say which actions an obligation gates.
func MaterialDischargeClaims(contract domain.EngineeringWorkContract) []string {
	var claims []string
	for _, obligation := range contract.Obligations {
		if obligation.Material {
			claims = append(claims, obligation.RequiredClaims...)
		}
	}
	return sortedUnique(claims)
}

// ---------------------------------------------------------------------------
// Plan-shaped obligations
// ---------------------------------------------------------------------------

// addEngineeringRequirements resolves the role, capability and gate obligations
// one matching rule states.
//
// Obligations are CONJUNCTIVE, which is what makes merging them safe rather
// than a precedence rule in disguise: two rules that both require a security
// reviewer are both satisfied by one reviewer that meets the union of what they
// asked for. So capabilities union, the independence dimension takes the
// STRONGER of the two, and the required trust takes the stronger. Nothing here
// can weaken an obligation another rule already stated.
//
// The one member that is a PERMISSION rather than an obligation -
// human_substitution_permitted - is intersected instead: a substitution is
// permitted only if every rule requiring that independence permits it. Merging
// it the other way would let one permissive rule unlock a substitution a
// stricter rule refused.
func (r *resolution) addEngineeringRequirements(ruleID string, requirements domain.PlanRequirements) error {
	for _, role := range requirements.Roles {
		if !domain.KnownRole(role.Role) {
			return fmt.Errorf("rule %q requires unknown engineering role %q", ruleID, role.Role)
		}
		for _, capability := range role.Capabilities {
			if !domain.KnownCapability(capability) {
				return fmt.Errorf("rule %q requires unknown capability %q for role %q", ruleID, capability, role.Role)
			}
		}
		if role.Independence != nil && !knownDimension(role.Independence.Dimension) {
			return fmt.Errorf("rule %q requires unknown independence dimension %q", ruleID, role.Independence.Dimension)
		}
		if role.TrustRequirement != "" && role.TrustRequirement != domain.TrustRequirementOperatorTrusted && role.TrustRequirement != domain.TrustRequirementProtected {
			return fmt.Errorf("rule %q requires unknown execution trust %q", ruleID, role.TrustRequirement)
		}
		existing, exists := r.roles[role.Role]
		if !exists {
			r.roles[role.Role] = normalizeRoleRequirement(role)
			continue
		}
		merged, err := mergeRoleRequirements(existing, normalizeRoleRequirement(role))
		if err != nil {
			return fmt.Errorf("rule %q: %w", ruleID, err)
		}
		r.roles[role.Role] = merged
	}
	for _, capability := range requirements.Capabilities {
		if !domain.KnownCapability(capability) {
			return fmt.Errorf("rule %q requires unknown capability %q", ruleID, capability)
		}
		r.capabilities[capability] = true
	}
	for _, gate := range requirements.Gates {
		if gate.Kind != domain.StageAssuranceGate && gate.Kind != domain.StageHumanDecisionGate {
			return fmt.Errorf("rule %q requires gate kind %q, which is not a gate: only %q and %q are gates, and an %q stage is worker execution",
				ruleID, gate.Kind, domain.StageAssuranceGate, domain.StageHumanDecisionGate, domain.StageAgent)
		}
		key := gateKey(gate)
		existing, exists := r.gates[key]
		if !exists {
			r.gates[key] = normalizeGateRequirement(gate)
			continue
		}
		existing.RequiredClaims = sortedUnique(append(existing.RequiredClaims, gate.RequiredClaims...))
		r.gates[key] = existing
	}
	// Every claim a gate names has to be a claim the contract actually defines.
	// Without this, a gate could reference a claim nothing can produce and would
	// read as merely outstanding forever rather than as unsatisfiable.
	for _, gate := range requirements.Gates {
		for _, claim := range gate.RequiredClaims {
			r.references[claim] = ruleID
		}
	}
	return nil
}

// gateKey identifies one gate obligation. Two rules asking for the same kind of
// gate over the same action are ONE gate whose claims are the union; two gates
// over different actions are different gates.
func gateKey(gate domain.GateRequirement) string {
	if gate.Action == nil {
		return string(gate.Kind)
	}
	return string(gate.Kind) + "\x00" + actionKey(*gate.Action)
}

func normalizeGateRequirement(gate domain.GateRequirement) domain.GateRequirement {
	gate.RequiredClaims = sortedUnique(gate.RequiredClaims)
	return gate
}

func normalizeRoleRequirement(role domain.RoleRequirement) domain.RoleRequirement {
	role.Capabilities = sortedUniqueCapabilities(role.Capabilities)
	if role.Independence != nil {
		independence := *role.Independence
		independence.DifferentFrom = sortedUnique(independence.DifferentFrom)
		role.Independence = &independence
	}
	return role
}

// mergeRoleRequirements takes the stronger of every obligation and the
// intersection of the one permission.
func mergeRoleRequirements(left, right domain.RoleRequirement) (domain.RoleRequirement, error) {
	merged := left
	merged.Capabilities = sortedUniqueCapabilities(append(append([]domain.EngineeringCapability{}, left.Capabilities...), right.Capabilities...))
	if left.Statement != right.Statement {
		merged.Statement = left.Statement + " " + right.Statement
	}
	switch {
	case right.Independence == nil:
	case left.Independence == nil:
		merged.Independence = right.Independence
	default:
		strongest := left.Independence
		if dimensionStrength(right.Independence.Dimension) > dimensionStrength(left.Independence.Dimension) {
			strongest = right.Independence
		}
		independence := *strongest
		independence.DifferentFrom = sortedUnique(append(append([]string{}, left.Independence.DifferentFrom...), right.Independence.DifferentFrom...))
		// A substitution is permitted only where BOTH rules permit it.
		independence.HumanSubstitutionPermitted = left.Independence.HumanSubstitutionPermitted && right.Independence.HumanSubstitutionPermitted
		merged.Independence = &independence
	}
	if trustStrength(right.TrustRequirement) > trustStrength(left.TrustRequirement) {
		merged.TrustRequirement = right.TrustRequirement
	}
	return merged, nil
}

// dimensionStrength orders the independence dimensions. A stronger dimension
// implies the weaker ones, so taking the maximum can never satisfy less than
// either rule asked for.
func dimensionStrength(dimension domain.IndependenceDimension) int {
	for strength, known := range domain.IndependenceDimensions() {
		if known == dimension {
			return strength
		}
	}
	return -1
}

func knownDimension(dimension domain.IndependenceDimension) bool {
	return dimensionStrength(dimension) >= 0
}

// trustStrength orders execution trust. Protected is stricter than
// operator-trusted, and an unstated requirement is weaker than both.
func trustStrength(trust domain.TrustRequirement) int {
	switch trust {
	case domain.TrustRequirementProtected:
		return 2
	case domain.TrustRequirementOperatorTrusted:
		return 1
	default:
		return 0
	}
}

// planRequirements emits the resolved plan-shaped obligations in canonical
// order, or nil when policy stated none. Nil is the point: a contract compiled
// from a policy without plan obligations stays byte-identical to one compiled
// before this member existed.
func (r *resolution) planRequirements() *domain.PlanRequirements {
	if len(r.roles) == 0 && len(r.capabilities) == 0 && len(r.gates) == 0 {
		return nil
	}
	requirements := domain.PlanRequirements{}
	for _, role := range domain.EngineeringRoles() {
		if requirement, ok := r.roles[role]; ok {
			requirements.Roles = append(requirements.Roles, requirement)
		}
	}
	for _, capability := range domain.EngineeringCapabilities() {
		if r.capabilities[capability] {
			requirements.Capabilities = append(requirements.Capabilities, capability)
		}
	}
	keys := make([]string, 0, len(r.gates))
	for key := range r.gates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		requirements.Gates = append(requirements.Gates, r.gates[key])
	}
	return &requirements
}

func sortedUniqueCapabilities(values []domain.EngineeringCapability) []domain.EngineeringCapability {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[domain.EngineeringCapability]bool, len(values))
	for _, value := range values {
		seen[value] = true
	}
	result := make([]domain.EngineeringCapability, 0, len(seen))
	// Canonical order is the ONTOLOGY's order, not the input's, so two rules
	// stating the same capabilities in different orders compile identically.
	for _, capability := range domain.EngineeringCapabilities() {
		if seen[capability] {
			result = append(result, capability)
		}
	}
	return result
}
