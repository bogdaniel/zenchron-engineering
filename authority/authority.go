// Package authority deterministically evaluates whether one protected action
// is currently authorized by a work contract and its evidence basis.
package authority

import (
	"fmt"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Input contains the complete, explicit state used to evaluate one action.
// EvidenceBundles is keyed by bundle identity so the decision basis is
// reproducible and duplicate bundle identities cannot be ambiguous.
type Input struct {
	DecisionID       string
	DecisionRevision string
	Contract         domain.EngineeringWorkContract
	Action           domain.Action
	Capability       domain.CapabilityStatus
	ChangeProducer   domain.EvidenceProducer
	EvidenceBundles  map[string]domain.EvidenceBundle
}

// Evaluate computes an action-scoped decision without calling an execution,
// assurance, or model provider. It treats a passing item as satisfying a claim
// only when its bundle exactly matches the contract's subject, revision, and
// policy revision, and it enforces claims that require independence from the
// producer of the change.
func Evaluate(input Input) (domain.AuthorityDecision, error) {
	if err := validateInput(input); err != nil {
		return domain.AuthorityDecision{}, err
	}

	decision := domain.AuthorityDecision{
		SchemaVersion: domain.SchemaVersion,
		ID:            input.DecisionID,
		Revision:      input.DecisionRevision,
		Subject:       input.Contract.Subject,
		Contract:      domain.ObjectRevision{ID: input.Contract.ID, Revision: input.Contract.Revision},
		Basis: domain.DecisionBasis{
			EvidenceBundles: decisionBasis(input.EvidenceBundles),
			ChangeProducer:  input.ChangeProducer,
		},
		Action:     input.Action,
		Capability: domain.Capability{Status: input.Capability},
		Permission: domain.Permission{Status: permissionFor(input.Contract, input.Action)},
	}

	requiredClaims := requiredClaimsFor(input.Contract, input.Action)
	classification := classifyClaims(input, requiredClaims)
	decision.Satisfied = classification.satisfied
	decision.Missing = classification.missing
	decision.Stale = classification.stale
	decision.Blocking = classification.blocking

	if decision.Permission.Status == domain.PermissionDenied {
		decision.Blocking = append(decision.Blocking, permissionRequirement(input.Action))
	}
	switch input.Capability {
	case domain.CapabilityUnavailable:
		decision.Blocking = append(decision.Blocking, capabilityRequirement(input.Action))
	case domain.CapabilityUnknown:
		decision.Missing = append(decision.Missing, capabilityRequirement(input.Action))
	}

	decision.Satisfied = sortedUnique(decision.Satisfied)
	decision.Missing = sortedUnique(decision.Missing)
	decision.Stale = sortedUnique(decision.Stale)
	decision.Blocking = sortedUnique(decision.Blocking)
	decision.Status = statusFor(decision, input.Contract.RequiredClaims)

	if _, err := domain.Encode(decision); err != nil {
		return domain.AuthorityDecision{}, fmt.Errorf("computed authority decision is invalid: %w", err)
	}
	return decision, nil
}

func validateInput(input Input) error {
	if input.DecisionID == "" || input.DecisionRevision == "" {
		return fmt.Errorf("decision id and revision are required")
	}
	if input.Action.Type == "" || input.Action.Target == "" {
		return fmt.Errorf("action type and target are required")
	}
	if input.Capability != domain.CapabilityAvailable && input.Capability != domain.CapabilityUnavailable && input.Capability != domain.CapabilityUnknown {
		return fmt.Errorf("unsupported capability status %q", input.Capability)
	}
	if _, err := domain.Encode(input.Contract); err != nil {
		return fmt.Errorf("invalid work contract: %w", err)
	}
	if err := validateContractReferences(input.Contract); err != nil {
		return err
	}
	if input.ChangeProducer.ID == "" || !knownProducerType(input.ChangeProducer.Type) {
		return fmt.Errorf("change producer identity and supported type are required")
	}
	for id, bundle := range input.EvidenceBundles {
		if id == "" || id != bundle.ID {
			return fmt.Errorf("evidence bundle key %q does not match bundle identity %q", id, bundle.ID)
		}
		if _, err := domain.Encode(bundle); err != nil {
			return fmt.Errorf("invalid evidence bundle %q: %w", id, err)
		}
	}
	return nil
}

func validateContractReferences(contract domain.EngineeringWorkContract) error {
	conditions := make(map[domain.Action]struct{}, len(contract.AuthorityConditions))
	for _, condition := range contract.AuthorityConditions {
		if _, exists := conditions[condition.Action]; exists {
			return fmt.Errorf("contract has conflicting authority conditions for %s:%s", condition.Action.Type, condition.Action.Target)
		}
		conditions[condition.Action] = struct{}{}
		for _, claimID := range condition.RequiredClaims {
			if _, exists := contract.RequiredClaims[claimID]; !exists {
				return fmt.Errorf("authority condition for %s:%s references undefined required claim %q", condition.Action.Type, condition.Action.Target, claimID)
			}
		}
	}
	return nil
}

func knownProducerType(kind domain.ProducerType) bool {
	return kind == domain.ProducerExecutionProvider || kind == domain.ProducerAssuranceProvider || kind == domain.ProducerDeterministicTool || kind == domain.ProducerHuman
}

func decisionBasis(bundles map[string]domain.EvidenceBundle) map[string]domain.RevisionReference {
	basis := make(map[string]domain.RevisionReference, len(bundles))
	for id, bundle := range bundles {
		basis[id] = domain.RevisionReference{Revision: bundle.Revision}
	}
	return basis
}

func permissionFor(contract domain.EngineeringWorkContract, action domain.Action) domain.PermissionStatus {
	for _, prohibited := range contract.Prohibitions {
		if prohibited == action {
			return domain.PermissionDenied
		}
	}
	for _, permitted := range contract.Permissions {
		if permitted == action {
			return domain.PermissionGranted
		}
	}
	return domain.PermissionDenied
}

func requiredClaimsFor(contract domain.EngineeringWorkContract, action domain.Action) []string {
	for _, condition := range contract.AuthorityConditions {
		if condition.Action == action {
			return sortedUnique(condition.RequiredClaims)
		}
	}
	return nil
}

type claimClassification struct {
	satisfied []string
	missing   []string
	stale     []string
	blocking  []string
}

func classifyClaims(input Input, claimIDs []string) claimClassification {
	var result claimClassification
	for _, claimID := range claimIDs {
		claim := input.Contract.RequiredClaims[claimID]
		state := claimState(input, claimID, claim)
		switch state {
		case claimSatisfied:
			result.satisfied = append(result.satisfied, claimID)
		case claimBlocked:
			result.blocking = append(result.blocking, claimID)
		case claimStale:
			result.stale = append(result.stale, claimID)
		default:
			result.missing = append(result.missing, claimID)
		}
	}
	return result
}

type evidenceClaimState uint8

const (
	claimMissing evidenceClaimState = iota
	claimSatisfied
	claimBlocked
	claimStale
)

func claimState(input Input, claimID string, claim domain.RequiredClaim) evidenceClaimState {
	var hasStale, hasPassing, hasFailed bool
	for _, bundle := range input.EvidenceBundles {
		for _, item := range bundle.Evidence {
			if item.ClaimID != claimID || item.EvidenceClass != claim.EvidenceClass {
				continue
			}
			// A human approval is a typed human assertion. Non-human items that
			// happen to use the human_approval evidence class are ineligible for
			// this claim, regardless of their binding, lifecycle, or result.
			if claim.EvidenceClass == "human_approval" && item.Producer.Type != domain.ProducerHuman {
				continue
			}
			if !matchesContractBinding(bundle, input.Contract) || item.Lifecycle.Status == domain.EvidenceStale {
				hasStale = true
				continue
			}
			if item.Lifecycle.Status != domain.EvidenceValid {
				continue
			}
			if item.Result.Status == domain.EvidenceFailed {
				hasFailed = true
			}
			if item.Result.Status == domain.EvidencePassed &&
				(!claim.IndependentFromChangeProducer || item.Producer.ID != input.ChangeProducer.ID) {
				hasPassing = true
			}
		}
	}
	if hasFailed {
		return claimBlocked
	}
	if hasPassing {
		return claimSatisfied
	}
	if hasStale {
		return claimStale
	}
	return claimMissing
}

func matchesContractBinding(bundle domain.EvidenceBundle, contract domain.EngineeringWorkContract) bool {
	return bundle.Subject == contract.Subject &&
		bundle.Contract == (domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}) &&
		bundle.Policy == contract.Provenance.Policy
}

func statusFor(decision domain.AuthorityDecision, claims map[string]domain.RequiredClaim) domain.AuthorityStatus {
	if len(decision.Blocking) > 0 {
		return domain.AuthorityBlocked
	}
	if len(decision.Stale) > 0 {
		return domain.AuthorityStale
	}
	if len(decision.Missing) == 0 {
		return domain.AuthorityAuthorized
	}
	for _, requirement := range decision.Missing {
		claim, isClaim := claims[requirement]
		if !isClaim || claim.EvidenceClass != "human_approval" {
			return domain.AuthorityIncomplete
		}
	}
	return domain.AuthorityAwaitingAuthority
}

func permissionRequirement(action domain.Action) string {
	return "permission:" + action.Type + ":" + action.Target
}

func capabilityRequirement(action domain.Action) string {
	return "capability:" + action.Type + ":" + action.Target
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
