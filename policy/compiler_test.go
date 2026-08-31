package policy_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

func TestCompileCanonicalFixtureCasesDifferAndReplayDeterministically(t *testing.T) {
	cases := []struct {
		name        string
		fact        string
		obligations []string
		claims      []string
		permission  []domain.Action
		condition   domain.AuthorityCondition
	}{
		{
			name: "trivial", fact: "trivial.engineering-fact.json",
			obligations: []string{acceptanceObligation, "trivial-change-validation"},
			claims:      []string{"claim-semantic-acceptance", "claim-trivial-validation"},
			permission:  []domain.Action{{Type: "git.pull_request.create", Target: "main"}},
			condition:   domain.AuthorityCondition{Action: domain.Action{Type: "git.pull_request.create", Target: "main"}, RequiredClaims: []string{"claim-trivial-validation"}},
		},
		{
			name: "normal", fact: "normal-behavior.engineering-fact.json",
			obligations: []string{acceptanceObligation, "api-behavior-tests"},
			claims:      []string{"claim-api-behavior-tests", "claim-api-contract-tests", "claim-semantic-acceptance"},
			permission:  []domain.Action{{Type: "git.pull_request.create", Target: "main"}},
			condition:   domain.AuthorityCondition{Action: domain.Action{Type: "git.pull_request.create", Target: "main"}, RequiredClaims: []string{"claim-api-behavior-tests", "claim-api-contract-tests"}},
		},
		{
			name: "security-sensitive", fact: "security-sensitive.engineering-fact.json",
			obligations: []string{acceptanceObligation, "auth-regression-tests", "independent-security-review", "security-owner-approval"},
			claims:      []string{"claim-auth-regression-tests", "claim-security-owner-approval", "claim-security-review", "claim-semantic-acceptance"},
			permission:  []domain.Action{},
			condition:   domain.AuthorityCondition{Action: domain.Action{Type: "git.merge", Target: "main"}, RequiredClaims: []string{"claim-auth-regression-tests", "claim-security-owner-approval", "claim-security-review"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := compileInput(t, fixtureInput(t, tc.fact))
			if got := requirementIDs(first.Obligations); !reflect.DeepEqual(got, tc.obligations) {
				t.Fatalf("obligations = %v, want %v", got, tc.obligations)
			}
			if got := claimIDs(first.RequiredClaims); !reflect.DeepEqual(got, tc.claims) {
				t.Fatalf("required claims = %v, want %v", got, tc.claims)
			}
			if !reflect.DeepEqual(first.Permissions, tc.permission) {
				t.Fatalf("permissions = %#v, want %#v", first.Permissions, tc.permission)
			}
			if len(first.AuthorityConditions) != 1 || !reflect.DeepEqual(first.AuthorityConditions[0], tc.condition) {
				t.Fatalf("authority conditions = %#v, want %#v", first.AuthorityConditions, tc.condition)
			}

			firstJSON, err := domain.Encode(first)
			if err != nil {
				t.Fatal(err)
			}
			secondJSON, err := domain.Encode(compileInput(t, fixtureInput(t, tc.fact)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(firstJSON, secondJSON) {
				t.Fatalf("same canonical inputs did not replay deterministically:\n%s\n%s", firstJSON, secondJSON)
			}
		})
	}
}

func TestCompileUnknownCreatesResolutionObligation(t *testing.T) {
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	unknown := input.Facts[0]
	unknown.ID = "fact-api-unknown"
	unknown.Value = domain.FactUnknown
	contract := compile(t, unknown)
	if _, ok := contract.Obligations["resolve-uncertain-"+unknown.ID]; !ok {
		t.Fatalf("unknown fact disappeared: %#v", contract.Obligations)
	}
	if len(contract.Facts) != 1 || contract.Facts[0] != unknown.ID {
		t.Fatalf("contract did not retain the unknown fact: %#v", contract.Facts)
	}
}

func TestCompileRejectsConflicts(t *testing.T) {
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	input.Policy.Rules["deny"] = domain.PolicyRule{
		When: domain.PolicyCondition{Fact: "api.behavior_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{Prohibitions: actions(domain.Action{
			Type: "git.pull_request.create", Target: "main",
		})},
	}
	if _, err := policy.Compile(input); err == nil {
		t.Fatal("expected permission/prohibition conflict")
	}
}

func TestCompileRejectsPermissionExpansionDuringRecompilation(t *testing.T) {
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	input.ContractRevision = "2"
	input.Subject.Revision = "rev-b"
	input.Facts[0].Subject = input.Subject
	input.PreviousContract = &domain.EngineeringWorkContract{
		ID:       input.ContractID,
		Revision: "1",
		Subject: domain.Subject{
			Repository: input.Subject.Repository,
			Revision:   "rev-a",
		},
	}
	if _, err := policy.Compile(input); err == nil {
		t.Fatal("expected cross-revision recompilation permission expansion to fail")
	}
}

func TestCompileProjectModelSubjectBoundary(t *testing.T) {
	t.Run("predicted revision mismatch is rejected", func(t *testing.T) {
		input := fixtureInput(t, "normal-behavior.engineering-fact.json")
		input.Scope.Stage = domain.StagePredicted
		input.Subject.Revision = "rev-b"
		input.Facts[0].Subject = input.Subject
		if _, err := policy.Compile(input); err == nil {
			t.Fatal("expected predicted ProjectModel revision mismatch to fail")
		}
	})

	t.Run("observed later revision in same repository is accepted", func(t *testing.T) {
		input := fixtureInput(t, "normal-behavior.engineering-fact.json")
		input.Subject.Revision = "rev-b"
		input.Facts[0].Subject = input.Subject
		contract := compileInput(t, input)
		if contract.Subject != input.Subject {
			t.Fatalf("compiled subject = %#v, want %#v", contract.Subject, input.Subject)
		}
	})
}

func TestCompilePreviousContractBoundary(t *testing.T) {
	t.Run("same repository later revision preserves permissions", func(t *testing.T) {
		previousInput := fixtureInput(t, "normal-behavior.engineering-fact.json")
		previousInput.ContractRevision = "1"
		previous := compileInput(t, previousInput)

		input := fixtureInput(t, "normal-behavior.engineering-fact.json")
		input.ContractRevision = "2"
		input.Subject.Revision = "rev-b"
		input.Facts[0].Subject = input.Subject
		input.PreviousContract = &previous

		contract := compileInput(t, input)
		if contract.Provenance.PreviousContractRevision == nil || *contract.Provenance.PreviousContractRevision != previous.Revision {
			t.Fatalf("previous contract revision = %#v, want %q", contract.Provenance.PreviousContractRevision, previous.Revision)
		}
	})

	t.Run("different repository is rejected", func(t *testing.T) {
		input := fixtureInput(t, "normal-behavior.engineering-fact.json")
		input.PreviousContract = &domain.EngineeringWorkContract{
			ID:       input.ContractID,
			Revision: "1",
			Subject:  domain.Subject{Repository: "other/repository", Revision: "rev-a"},
		}
		if _, err := policy.Compile(input); err == nil {
			t.Fatal("expected previous contract repository mismatch to fail")
		}
	})
}

func TestCompileRejectsRequirementsWithDifferentRequiredClaims(t *testing.T) {
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	claims := map[string]domain.RequiredClaim{
		"claim-alternate-api-tests": {EvidenceClass: "test_result", IndependentFromChangeProducer: true},
	}
	requirements := map[string]domain.PolicyRequirement{
		"api-behavior-tests": {Statement: "API behavior and compatibility tests must pass.", RequiredClaims: stringList("claim-alternate-api-tests")},
	}
	input.Policy.Rules["conflicting-api-requirement"] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "api.behavior_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &requirements},
	}
	if _, err := policy.Compile(input); err == nil || !strings.Contains(err.Error(), `conflicting requirement "api-behavior-tests"`) {
		t.Fatalf("expected required-claim conflict, got %v", err)
	}
}

func TestCompileAcceptsRequirementClaimReferencesInDifferentOrder(t *testing.T) {
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	requirements := map[string]domain.PolicyRequirement{
		"api-behavior-tests": {Statement: "API behavior and compatibility tests must pass.", RequiredClaims: stringList("claim-api-contract-tests", "claim-api-behavior-tests")},
	}
	input.Policy.Rules["duplicate-api-requirement"] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "api.behavior_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{Obligations: &requirements},
	}
	if _, err := policy.Compile(input); err != nil {
		t.Fatalf("requirement claim ordering changed its definition: %v", err)
	}
}

// acceptanceObligation is the stable, content-derived id of the fixture's own
// acceptance criterion. It is computed rather than written out so the test
// proves the derivation is stable, not that someone copied a hash.
var acceptanceObligation = domain.AcceptanceObligationID("Works.")

func compile(t *testing.T, facts ...domain.EngineeringFact) domain.EngineeringWorkContract {
	t.Helper()
	input := fixtureInput(t, "normal-behavior.engineering-fact.json")
	input.Facts = facts
	input.Subject = facts[0].Subject
	input.ProjectModel.Subject = facts[0].Subject
	contract := compileInput(t, input)
	return contract
}

func compileInput(t *testing.T, input policy.CompileInput) domain.EngineeringWorkContract {
	t.Helper()
	contract, err := policy.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func fixtureInput(t *testing.T, factName string) policy.CompileInput {
	t.Helper()
	fact := decodeFixture[domain.EngineeringFact](t, factName)
	project := decodeFixture[domain.ProjectModel](t, "security-sensitive.project-model.json")
	policyFixture := decodeFixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	return policy.CompileInput{
		ContractID:       "contract-" + fact.ID,
		ContractRevision: "1",
		Objective:        "Change behavior.",
		AcceptanceIntent: []string{"Works."},
		Subject:          fact.Subject,
		Scope: domain.ContractScope{
			Stage: domain.StageObserved, AllowedPaths: []string{"internal/payments/retry.go"},
		},
		ProjectModel: project,
		Policy:       policyFixture,
		Facts:        []domain.EngineeringFact{fact},
	}
}

func decodeFixture[T domain.Contract](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[T](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func actions(value ...domain.Action) *[]domain.Action {
	return &value
}

func stringList(values ...string) *[]string {
	return &values
}

func requirementIDs(requirements map[string]domain.Requirement) []string {
	ids := make([]string, 0, len(requirements))
	for id := range requirements {
		ids = append(ids, id)
	}
	return sorted(ids)
}

func claimIDs(claims map[string]domain.RequiredClaim) []string {
	ids := make([]string, 0, len(claims))
	for id := range claims {
		ids = append(ids, id)
	}
	return sorted(ids)
}

func sorted(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
