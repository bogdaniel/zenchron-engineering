// Package reassessment validates observed work against a work contract and
// recompiles the contract when material scope or impact expansion is found.
package reassessment

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/evidence"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

// Input contains the current governance envelope and the candidate change to
// assess. Compile supplies the subject, next revision, and pinned governance
// inputs for the next revision. Objective, acceptance intent, and prohibited
// scope remain stable from CurrentContract; observed facts and observed scope
// are always derived here rather than accepted from its Facts field.
type Input struct {
	CurrentContract   domain.EngineeringWorkContract
	Compile           policy.CompileInput
	ObservedChange    analysis.ObservedChange
	Analyzer          analysis.Analyzer
	EvidenceBundles   map[string]domain.EvidenceBundle
	EvidenceRevisions map[string]string
}

// Deviation records a deterministic reason that the current contract no
// longer covers the observed change.
type Deviation struct {
	Kind   string
	Detail string
}

// Result is the outcome of an observed-scope assessment. When Material is
// true, Contract is the next contract revision and SuspendedActions must not
// be attempted under the current contract.
type Result struct {
	ObservedFacts analysis.FactSet
	Material      bool
	Deviations    []Deviation
	// RequestedPrivilegeExpansion records permissions resolved from observed
	// facts that exceed the current contract's permission ceiling. They remain
	// ungranted in Contract and require authority outside reassessment.
	RequestedPrivilegeExpansion []domain.Action
	Contract                    *domain.EngineeringWorkContract
	SuspendedActions            []domain.Action
	StaleEvidence               map[string]domain.EvidenceBundle
}

// Reassess derives observed facts, compares them with the current governance
// envelope, and recompiles only for a material expansion. It may add
// obligations automatically. A newly resolved permission is returned as a
// governed request while the revised contract retains the current permission
// ceiling.
func Reassess(input Input) (Result, error) {
	if err := validateInput(input); err != nil {
		return Result{}, err
	}
	observed, err := analysis.NormalizeObservedChange(input.ObservedChange)
	if err != nil {
		return Result{}, err
	}
	analyzer := input.Analyzer
	if analyzer.IsZero() {
		analyzer = analysis.NewAnalyzer()
	}
	facts, err := analyzer.Observe(input.Compile.ProjectModel, input.Compile.Subject, observed)
	if err != nil {
		return Result{}, err
	}

	compileInput := input.Compile
	compileInput.Objective = input.CurrentContract.Objective
	compileInput.AcceptanceIntent = append([]string(nil), input.CurrentContract.AcceptanceIntent...)
	compileInput.Facts = facts.Sorted()
	compileInput.Scope.Stage = domain.StageObserved
	compileInput.Scope.ProhibitedPaths = append([]string(nil), input.CurrentContract.Scope.ProhibitedPaths...)
	compileInput.Scope.AllowedPaths = append([]string(nil), input.CurrentContract.Scope.AllowedPaths...)
	for _, path := range observed.Paths {
		if !matchesAny(path, compileInput.Scope.ProhibitedPaths) {
			compileInput.Scope.AllowedPaths = append(compileInput.Scope.AllowedPaths, path)
		}
	}
	compileInput.Scope.AllowedPaths = sortedUnique(compileInput.Scope.AllowedPaths)
	// Resolve the observed policy outcome before enforcing the ceiling so a
	// privilege request can be reported structurally rather than discarded as
	// a compiler-error string.
	compileInput.PreviousContract = nil
	candidate, err := policy.Compile(compileInput)
	if err != nil {
		return Result{}, fmt.Errorf("recompile observed scope: %w", err)
	}
	requestedPrivileges := actionDifference(candidate.Permissions, input.CurrentContract.Permissions)
	if len(requestedPrivileges) == 0 {
		compileInput.PreviousContract = &input.CurrentContract
		candidate, err = policy.Compile(compileInput)
		if err != nil {
			return Result{}, fmt.Errorf("recompile observed scope: %w", err)
		}
	} else {
		candidate.Permissions = actionIntersection(candidate.Permissions, input.CurrentContract.Permissions)
		candidate.Provenance.PreviousContractRevision = previousRevision(&input.CurrentContract)
		if _, err := domain.Encode(candidate); err != nil {
			return Result{}, fmt.Errorf("compile permission-capped contract: %w", err)
		}
	}

	deviations := scopeDeviations(input.CurrentContract.Scope, observed)
	if input.Compile.Subject.Revision != input.CurrentContract.Subject.Revision {
		deviations = append(deviations, Deviation{Kind: "subject_revision_changed", Detail: input.Compile.Subject.Revision})
	}
	deviations = append(deviations, impactDeviations(input.CurrentContract, candidate)...)
	for _, action := range requestedPrivileges {
		deviations = append(deviations, Deviation{Kind: "requested_privilege_expansion", Detail: actionKey(action)})
	}
	deviations = sortedDeviations(deviations)
	result := Result{ObservedFacts: facts, Material: len(deviations) > 0, Deviations: deviations, RequestedPrivilegeExpansion: requestedPrivileges}
	if !result.Material {
		return result, nil
	}

	result.Contract = &candidate
	result.SuspendedActions = suspendedActions(input.CurrentContract, candidate)
	result.SuspendedActions = append(result.SuspendedActions, requestedPrivileges...)
	sort.Slice(result.SuspendedActions, func(i, j int) bool {
		return actionKey(result.SuspendedActions[i]) < actionKey(result.SuspendedActions[j])
	})
	result.SuspendedActions = uniqueActions(result.SuspendedActions)
	result.StaleEvidence = make(map[string]domain.EvidenceBundle, len(input.EvidenceBundles))
	target := evidence.Binding{
		Subject:  candidate.Subject,
		Contract: domain.ObjectRevision{ID: candidate.ID, Revision: candidate.Revision},
		Policy:   candidate.Provenance.Policy,
	}
	for id, bundle := range input.EvidenceBundles {
		nextRevision, ok := input.EvidenceRevisions[id]
		if !ok {
			return Result{}, fmt.Errorf("missing next revision for evidence bundle %q", id)
		}
		currentBinding := evidence.Binding{
			Subject:  input.CurrentContract.Subject,
			Contract: domain.ObjectRevision{ID: input.CurrentContract.ID, Revision: input.CurrentContract.Revision},
			Policy:   input.CurrentContract.Provenance.Policy,
		}
		if evidence.BindingOf(bundle) != currentBinding {
			return Result{}, fmt.Errorf("evidence bundle %q does not bind to the current contract", id)
		}
		stale, err := evidence.MarkStaleForBindingChange(bundle, target, nextRevision)
		if err != nil {
			return Result{}, fmt.Errorf("stale evidence bundle %q: %w", id, err)
		}
		result.StaleEvidence[id] = stale
	}
	return result, nil
}

// Suspends reports whether action is affected by a material reassessment.
func (r Result) Suspends(action domain.Action) bool {
	for _, suspended := range r.SuspendedActions {
		if suspended == action {
			return true
		}
	}
	return false
}

func validateInput(input Input) error {
	if _, err := domain.Encode(input.CurrentContract); err != nil {
		return fmt.Errorf("invalid current contract: %w", err)
	}
	if input.Compile.ContractID != input.CurrentContract.ID {
		return fmt.Errorf("next contract id must match the current contract")
	}
	if input.Compile.ContractRevision == "" || input.Compile.ContractRevision == input.CurrentContract.Revision {
		return fmt.Errorf("next contract revision must differ from the current contract")
	}
	if input.Compile.Subject.Repository != input.CurrentContract.Subject.Repository {
		return fmt.Errorf("observed subject repository must match the current contract")
	}
	if input.Compile.Objective != input.CurrentContract.Objective {
		return fmt.Errorf("reassessment objective must match the current contract")
	}
	if !sameStrings(sortedUnique(input.Compile.AcceptanceIntent), sortedUnique(input.CurrentContract.AcceptanceIntent)) {
		return fmt.Errorf("reassessment acceptance intent must match the current contract")
	}
	if len(input.Compile.Scope.ProhibitedPaths) > 0 && !sameStrings(sortedUnique(input.Compile.Scope.ProhibitedPaths), sortedUnique(input.CurrentContract.Scope.ProhibitedPaths)) {
		return fmt.Errorf("reassessment prohibited paths must match the current contract")
	}
	if got := (domain.ObjectRevision{ID: input.Compile.ProjectModel.ID, Revision: input.Compile.ProjectModel.Revision}); got != input.CurrentContract.Provenance.ProjectModel {
		return fmt.Errorf("reassessment ProjectModel must match the current contract provenance")
	}
	if got := (domain.ObjectRevision{ID: input.Compile.Policy.ID, Revision: input.Compile.Policy.Revision}); got != input.CurrentContract.Provenance.Policy {
		return fmt.Errorf("reassessment policy must match the current contract provenance")
	}
	for id := range input.EvidenceRevisions {
		if _, ok := input.EvidenceBundles[id]; !ok {
			return fmt.Errorf("evidence revision supplied for unknown bundle %q", id)
		}
	}
	return nil
}

func scopeDeviations(scope domain.ContractScope, observed analysis.ObservedChange) []Deviation {
	if !observed.PathsKnown {
		return []Deviation{{Kind: "unknown_observed_scope", Detail: "observed paths could not be established"}}
	}
	var deviations []Deviation
	for _, path := range sortedUnique(observed.Paths) {
		if matchesAny(path, scope.ProhibitedPaths) {
			deviations = append(deviations, Deviation{Kind: "prohibited_path", Detail: path})
			continue
		}
		if !matchesAny(path, scope.AllowedPaths) {
			deviations = append(deviations, Deviation{Kind: "scope_expansion", Detail: path})
		}
	}
	return deviations
}

func impactDeviations(current, candidate domain.EngineeringWorkContract) []Deviation {
	var deviations []Deviation
	deviations = append(deviations, requirementDeviations("invariant", current.Invariants, candidate.Invariants)...)
	deviations = append(deviations, requirementDeviations("obligation", current.Obligations, candidate.Obligations)...)
	deviations = append(deviations, claimDeviations(current.RequiredClaims, candidate.RequiredClaims)...)
	deviations = append(deviations, actionDeviations("permission", current.Permissions, candidate.Permissions)...)
	deviations = append(deviations, actionDeviations("prohibition", current.Prohibitions, candidate.Prohibitions)...)
	deviations = append(deviations, conditionDeviations(current.AuthorityConditions, candidate.AuthorityConditions)...)
	return deviations
}

func requirementDeviations(kind string, current, candidate map[string]domain.Requirement) []Deviation {
	var deviations []Deviation
	for _, id := range sortedRequirementIDs(current, candidate) {
		before, hadBefore := current[id]
		after, hasAfter := candidate[id]
		switch {
		case !hadBefore:
			deviations = append(deviations, Deviation{Kind: "additional_" + kind, Detail: id})
		case !hasAfter:
			deviations = append(deviations, Deviation{Kind: "removed_" + kind, Detail: id})
		case before != after:
			deviations = append(deviations, Deviation{Kind: "changed_" + kind, Detail: id})
		}
	}
	return deviations
}

func claimDeviations(current, candidate map[string]domain.RequiredClaim) []Deviation {
	var deviations []Deviation
	for _, id := range sortedClaimIDs(current, candidate) {
		before, hadBefore := current[id]
		after, hasAfter := candidate[id]
		switch {
		case !hadBefore:
			deviations = append(deviations, Deviation{Kind: "additional_claim", Detail: id})
		case !hasAfter:
			deviations = append(deviations, Deviation{Kind: "removed_claim", Detail: id})
		case before != after:
			deviations = append(deviations, Deviation{Kind: "changed_claim", Detail: id})
		}
	}
	return deviations
}

func actionDeviations(kind string, current, candidate []domain.Action) []Deviation {
	var deviations []Deviation
	for _, action := range actionDifference(candidate, current) {
		deviations = append(deviations, Deviation{Kind: "additional_" + kind, Detail: actionKey(action)})
	}
	for _, action := range actionDifference(current, candidate) {
		deviations = append(deviations, Deviation{Kind: "removed_" + kind, Detail: actionKey(action)})
	}
	return deviations
}

func conditionDeviations(current, candidate []domain.AuthorityCondition) []Deviation {
	before := conditionsByAction(current)
	after := conditionsByAction(candidate)
	var deviations []Deviation
	for _, key := range sortedStringKeys(before, after) {
		left, hadLeft := before[key]
		right, hasRight := after[key]
		switch {
		case !hadLeft:
			deviations = append(deviations, Deviation{Kind: "additional_authority_condition", Detail: key})
		case !hasRight:
			deviations = append(deviations, Deviation{Kind: "removed_authority_condition", Detail: key})
		case !sameStrings(left.RequiredClaims, right.RequiredClaims):
			deviations = append(deviations, Deviation{Kind: "changed_authority_condition", Detail: key})
		}
	}
	return deviations
}

func suspendedActions(current, candidate domain.EngineeringWorkContract) []domain.Action {
	actions := append([]domain.Action(nil), current.Permissions...)
	actions = append(actions, current.Prohibitions...)
	actions = append(actions, candidate.Permissions...)
	actions = append(actions, candidate.Prohibitions...)
	for _, condition := range current.AuthorityConditions {
		actions = append(actions, condition.Action)
	}
	for _, condition := range candidate.AuthorityConditions {
		actions = append(actions, condition.Action)
	}
	sort.Slice(actions, func(i, j int) bool { return actionKey(actions[i]) < actionKey(actions[j]) })
	return uniqueActions(actions)
}

func matchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		path = strings.TrimPrefix(path, "./")
		pattern = strings.TrimPrefix(pattern, "./")
		if path == pattern || strings.HasSuffix(pattern, "/**") && (path == strings.TrimSuffix(pattern, "/**") || strings.HasPrefix(path, strings.TrimSuffix(pattern, "/**")+"/")) {
			return true
		}
	}
	return false
}

func actionDifference(left, right []domain.Action) []domain.Action {
	available := make(map[domain.Action]struct{}, len(right))
	for _, action := range right {
		available[action] = struct{}{}
	}
	var result []domain.Action
	for _, action := range left {
		if _, exists := available[action]; !exists {
			result = append(result, action)
		}
	}
	sort.Slice(result, func(i, j int) bool { return actionKey(result[i]) < actionKey(result[j]) })
	return uniqueActions(result)
}

func actionIntersection(left, right []domain.Action) []domain.Action {
	available := make(map[domain.Action]struct{}, len(right))
	for _, action := range right {
		available[action] = struct{}{}
	}
	// EngineeringWorkContract requires permissions to encode as a JSON array.
	// Keep the empty intersection non-nil so a permission ceiling of zero
	// serializes as [] rather than null.
	result := make([]domain.Action, 0, len(left))
	for _, action := range left {
		if _, exists := available[action]; exists {
			result = append(result, action)
		}
	}
	sort.Slice(result, func(i, j int) bool { return actionKey(result[i]) < actionKey(result[j]) })
	return uniqueActions(result)
}

func sortedRequirementIDs(left, right map[string]domain.Requirement) []string {
	ids := make(map[string]struct{}, len(left)+len(right))
	for id := range left {
		ids[id] = struct{}{}
	}
	for id := range right {
		ids[id] = struct{}{}
	}
	return sortedMapKeys(ids)
}

func sortedClaimIDs(left, right map[string]domain.RequiredClaim) []string {
	ids := make(map[string]struct{}, len(left)+len(right))
	for id := range left {
		ids[id] = struct{}{}
	}
	for id := range right {
		ids[id] = struct{}{}
	}
	return sortedMapKeys(ids)
}

func conditionsByAction(conditions []domain.AuthorityCondition) map[string]domain.AuthorityCondition {
	result := make(map[string]domain.AuthorityCondition, len(conditions))
	for _, condition := range conditions {
		condition.RequiredClaims = sortedUnique(condition.RequiredClaims)
		result[actionKey(condition.Action)] = condition
	}
	return result
}

func sortedStringKeys(left, right map[string]domain.AuthorityCondition) []string {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return sortedMapKeys(keys)
}

func sortedMapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func previousRevision(contract *domain.EngineeringWorkContract) *string {
	if contract == nil {
		return nil
	}
	revision := contract.Revision
	return &revision
}

func sortedDeviations(deviations []Deviation) []Deviation {
	sort.Slice(deviations, func(i, j int) bool {
		return deviations[i].Kind+"\x00"+deviations[i].Detail < deviations[j].Kind+"\x00"+deviations[j].Detail
	})
	return deviations
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
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

func actionKey(action domain.Action) string { return action.Type + "\x00" + action.Target }

func uniqueActions(actions []domain.Action) []domain.Action {
	result := actions[:0]
	for _, action := range actions {
		if len(result) == 0 || action != result[len(result)-1] {
			result = append(result, action)
		}
	}
	return result
}
