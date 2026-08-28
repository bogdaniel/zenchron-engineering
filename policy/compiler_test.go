package policy_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

func TestCompileProducesDifferentContractsDeterministically(t *testing.T) {
	low := compile(t, fact("docs.changed", domain.FactTrue))
	normal := compile(t, fact("payments.behavior_changed", domain.FactTrue))
	security := compile(t, fact("authentication.boundary_modified", domain.FactTrue))
	if len(low.Obligations) != 0 || len(normal.Obligations) != 1 || len(security.Obligations) != 2 {
		t.Fatalf("unexpected obligation levels: %d, %d, %d", len(low.Obligations), len(normal.Obligations), len(security.Obligations))
	}
	if _, ok := security.RequiredClaims["claim-security-review"]; !ok {
		t.Fatal("security contract omitted derived evidence class")
	}
	first, err := domain.Encode(security)
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.Encode(compile(t, fact("authentication.boundary_modified", domain.FactTrue)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("same inputs did not replay deterministically:\n%s\n%s", first, second)
	}
}

func TestCompileUnknownCreatesResolutionObligation(t *testing.T) {
	unknown := fact("payments.behavior_changed", domain.FactUnknown)
	contract := compile(t, unknown)
	if _, ok := contract.Obligations["resolve-uncertain-"+unknown.ID]; !ok {
		t.Fatalf("unknown fact disappeared: %#v", contract.Obligations)
	}
	if len(contract.Facts) != 1 || contract.Facts[0] != unknown.ID {
		t.Fatalf("contract did not retain the unknown fact: %#v", contract.Facts)
	}
}

func TestCompileRejectsConflicts(t *testing.T) {
	input := baseInput(fact("payments.behavior_changed", domain.FactTrue))
	input.Policy.Rules["deny"] = domain.PolicyRule{
		When: domain.PolicyCondition{Fact: "payments.behavior_changed", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{Prohibitions: actions(domain.Action{
			Type: "git.pull_request.create", Target: "main",
		})},
	}
	if _, err := policy.Compile(input); err == nil {
		t.Fatal("expected permission/prohibition conflict")
	}
}

func TestCompileRejectsPermissionExpansionDuringRecompilation(t *testing.T) {
	input := baseInput(fact("payments.behavior_changed", domain.FactTrue))
	input.PreviousContract = &domain.EngineeringWorkContract{
		ID:       input.ContractID,
		Revision: "1",
		Subject:  input.Subject,
	}
	if _, err := policy.Compile(input); err == nil {
		t.Fatal("expected recompilation permission expansion to fail")
	}
}

func compile(t *testing.T, facts ...domain.EngineeringFact) domain.EngineeringWorkContract {
	t.Helper()
	input := baseInput(facts...)
	contract, err := policy.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func baseInput(facts ...domain.EngineeringFact) policy.CompileInput {
	paymentClaims := map[string]domain.RequiredClaim{
		"claim-payment-tests": {EvidenceClass: "test_result", IndependentFromChangeProducer: true},
	}
	securityClaims := map[string]domain.RequiredClaim{
		"claim-auth-regression-tests": {EvidenceClass: "test_result", IndependentFromChangeProducer: true},
		"claim-security-review":       {EvidenceClass: "security_review", IndependentFromChangeProducer: true},
	}
	paymentObligations := map[string]domain.PolicyRequirement{"payment-tests": {Statement: "Payment tests pass."}}
	securityObligations := map[string]domain.PolicyRequirement{
		"auth-regression-tests": {Statement: "Authentication regression tests pass."},
		"security-review":       {Statement: "Independent security review passes."},
	}
	permissions := []domain.Action{{Type: "git.pull_request.create", Target: "main"}}
	conditions := []domain.AuthorityCondition{{Action: domain.Action{Type: "git.pull_request.create", Target: "main"}, RequiredClaims: []string{"claim-payment-tests"}}}
	return policy.CompileInput{
		ContractID:       "contract-1",
		ContractRevision: "1",
		Objective:        "Change behavior.",
		AcceptanceIntent: []string{"Works."},
		Subject:          domain.Subject{Repository: "acme/payments", Revision: "rev-a"},
		Scope: domain.ContractScope{
			Stage: domain.StagePredicted, AllowedPaths: []string{"internal/payments/retry.go"},
		},
		ProjectModel: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project-1", Revision: "1",
			Subject: domain.Subject{Repository: "acme/payments", Revision: "rev-a"},
		},
		Policy: domain.EngineeringPolicy{
			SchemaVersion: domain.SchemaVersion, ID: "policy-1", Revision: "1",
			Rules: map[string]domain.PolicyRule{
				"payment": {
					When:   domain.PolicyCondition{Fact: "payments.behavior_changed", Equals: domain.FactTrue},
					Effect: domain.PolicyEffect{RequiredClaims: &paymentClaims, Obligations: &paymentObligations, Permissions: &permissions, AuthorityConditions: &conditions},
				},
				"security": {
					When:   domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue},
					Effect: domain.PolicyEffect{RequiredClaims: &securityClaims, Obligations: &securityObligations},
				},
			},
		},
		Facts: facts,
	}
}

func fact(key string, value domain.FactValue) domain.EngineeringFact {
	return domain.EngineeringFact{
		SchemaVersion: domain.SchemaVersion,
		ID:            "fact-" + strings.ReplaceAll(key, ".", "-"),
		Key:           key,
		Value:         value,
		Stage:         domain.StagePredicted,
		Confidence:    domain.ConfidenceHigh,
		Subject:       domain.Subject{Repository: "acme/payments", Revision: "rev-a"},
		Provenance:    domain.FactProvenance{Type: "test", Producer: "test"},
	}
}

func actions(value ...domain.Action) *[]domain.Action {
	return &value
}
