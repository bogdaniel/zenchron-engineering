// Package policy deterministically resolves fact-driven policy rules into a
// bounded engineering work contract. It contains no execution-provider logic.
package policy

import (
	"fmt"
	"sort"

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
	if err := state.validateReferences(); err != nil {
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
	}
}

func (r *resolution) addUnknownResolution(fact domain.EngineeringFact) error {
	id := "resolve-uncertain-" + fact.ID
	return r.addRequirement(r.obligations, r.obligationDefinitions, id, domain.PolicyRequirement{Statement: "Resolve uncertainty for engineering fact " + fact.Key + "."}, "unknown fact "+fact.ID)
}

func (r *resolution) addEffect(ruleID string, effect domain.PolicyEffect) error {
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
	target[id] = domain.Requirement{Statement: definition.statement}
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
	return policyRequirement{statement: requirement.Statement, requiredClaims: claims}
}

func samePolicyRequirement(left, right policyRequirement) bool {
	return left.statement == right.statement && sameStrings(left.requiredClaims, right.requiredClaims)
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
