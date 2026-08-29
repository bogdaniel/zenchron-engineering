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
// assess. Compile supplies the stable work context for the next revision;
// observed facts and observed scope are always derived here rather than
// accepted from its Facts field.
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
	ObservedFacts    analysis.FactSet
	Material         bool
	Deviations       []Deviation
	Contract         *domain.EngineeringWorkContract
	SuspendedActions []domain.Action
	StaleEvidence    map[string]domain.EvidenceBundle
}

// Reassess derives observed facts, compares them with the current governance
// envelope, and recompiles only for a material expansion. It may add
// obligations automatically, but delegates the prohibition on permission
// expansion to policy.Compile.
func Reassess(input Input) (Result, error) {
	if err := validateInput(input); err != nil {
		return Result{}, err
	}
	analyzer := input.Analyzer
	if analyzer.IsZero() {
		analyzer = analysis.NewAnalyzer()
	}
	facts, err := analyzer.Observe(input.Compile.ProjectModel, input.Compile.Subject, input.ObservedChange)
	if err != nil {
		return Result{}, err
	}

	compileInput := input.Compile
	compileInput.Facts = facts.Sorted()
	compileInput.Scope.Stage = domain.StageObserved
	compileInput.Scope.AllowedPaths = append(append([]string(nil), input.CurrentContract.Scope.AllowedPaths...), input.ObservedChange.Paths...)
	compileInput.Scope.AllowedPaths = sortedUnique(compileInput.Scope.AllowedPaths)
	compileInput.Scope.ProhibitedPaths = sortedUnique(append(
		append([]string(nil), input.CurrentContract.Scope.ProhibitedPaths...),
		compileInput.Scope.ProhibitedPaths...,
	))
	compileInput.PreviousContract = &input.CurrentContract
	candidate, err := policy.Compile(compileInput)
	if err != nil {
		return Result{}, fmt.Errorf("recompile observed scope: %w", err)
	}

	deviations := scopeDeviations(input.CurrentContract.Scope, input.ObservedChange)
	deviations = append(deviations, impactDeviations(input.CurrentContract, candidate)...)
	deviations = sortedDeviations(deviations)
	result := Result{ObservedFacts: facts, Material: len(deviations) > 0, Deviations: deviations}
	if !result.Material {
		return result, nil
	}

	result.Contract = &candidate
	result.SuspendedActions = suspendedActions(input.CurrentContract, candidate)
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
	for id := range candidate.Invariants {
		if _, exists := current.Invariants[id]; !exists {
			deviations = append(deviations, Deviation{Kind: "additional_invariant", Detail: id})
		}
	}
	for id := range candidate.Obligations {
		if _, exists := current.Obligations[id]; !exists {
			deviations = append(deviations, Deviation{Kind: "additional_obligation", Detail: id})
		}
	}
	for id := range candidate.RequiredClaims {
		if _, exists := current.RequiredClaims[id]; !exists {
			deviations = append(deviations, Deviation{Kind: "additional_claim", Detail: id})
		}
	}
	for _, action := range candidate.Prohibitions {
		if !containsAction(current.Prohibitions, action) {
			deviations = append(deviations, Deviation{Kind: "additional_prohibition", Detail: action.Type + ":" + action.Target})
		}
	}
	for _, condition := range candidate.AuthorityConditions {
		if !containsCondition(current.AuthorityConditions, condition) {
			deviations = append(deviations, Deviation{Kind: "additional_authority_condition", Detail: condition.Action.Type + ":" + condition.Action.Target})
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

func containsAction(actions []domain.Action, target domain.Action) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
}

func containsCondition(conditions []domain.AuthorityCondition, target domain.AuthorityCondition) bool {
	for _, condition := range conditions {
		if condition.Action == target.Action && sameStrings(condition.RequiredClaims, target.RequiredClaims) {
			return true
		}
	}
	return false
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
