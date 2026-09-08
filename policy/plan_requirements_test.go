package policy_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

// The plan-shaped obligations - roles, capabilities, independence, gates -
// compile through THIS compiler. #64 acceptance scenario 20 is that there is no
// second policy engine behind the planner, and these tests are where that is
// checked at the compiler level: everything a planner is later allowed to
// require has to arrive in a contract through the path below.

func TestPolicyCompilesRoleCapabilityAndGateObligations(t *testing.T) {
	contract := compileInput(t, withEngineeringRequirements(t, domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role:         domain.RoleSecurityReviewer,
			Capabilities: []domain.EngineeringCapability{domain.CapabilitySecurityReview},
			Independence: &domain.IndependenceRequirement{Dimension: domain.IndependenceVendorFamily},
			Statement:    "A security reviewer independent of the material producer is required.",
		}},
		Capabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis},
		Gates: []domain.GateRequirement{{
			Kind:           domain.StageAssuranceGate,
			RequiredClaims: []string{"claim-security-review"},
			Statement:      "Assurance must record the independent review.",
		}},
	}))

	requirements := contract.PlanRequirements
	if requirements == nil {
		t.Fatal("policy stated plan obligations and the contract carries none")
	}
	if len(requirements.Roles) != 1 || requirements.Roles[0].Role != domain.RoleSecurityReviewer {
		t.Fatalf("roles = %#v", requirements.Roles)
	}
	if requirements.Roles[0].Independence == nil || requirements.Roles[0].Independence.Dimension != domain.IndependenceVendorFamily {
		t.Fatalf("independence = %#v", requirements.Roles[0].Independence)
	}
	if !reflect.DeepEqual(requirements.Capabilities, []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis}) {
		t.Fatalf("capabilities = %#v", requirements.Capabilities)
	}
	if len(requirements.Gates) != 1 || requirements.Gates[0].Kind != domain.StageAssuranceGate {
		t.Fatalf("gates = %#v", requirements.Gates)
	}
}

// Obligations are conjunctive, so two rules asking for the same role must
// produce ONE role that satisfies both - never the last one written.
func TestOverlappingRoleObligationsMergeToTheStrongerRequirement(t *testing.T) {
	input := withEngineeringRequirements(t, domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role:         domain.RoleSecurityReviewer,
			Capabilities: []domain.EngineeringCapability{domain.CapabilitySecurityReview},
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceAgentProfile, HumanSubstitutionPermitted: true,
			},
			Statement: "A security reviewer is required.",
		}},
	})
	// A second rule matching the same fact, asking for MORE.
	addRule(&input, "RULE-STRONGER", domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role:         domain.RoleSecurityReviewer,
			Capabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis},
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceVendorFamily, HumanSubstitutionPermitted: true,
			},
			TrustRequirement: domain.TrustRequirementProtected,
			Statement:        "The reviewer must be from a different vendor family.",
		}},
	})

	contract := compileInput(t, input)
	role := contract.PlanRequirements.Roles[0]
	want := []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis, domain.CapabilitySecurityReview}
	if !reflect.DeepEqual(role.Capabilities, want) {
		t.Fatalf("capabilities = %#v, want the union %#v", role.Capabilities, want)
	}
	if role.Independence.Dimension != domain.IndependenceVendorFamily {
		t.Fatalf("independence dimension = %q, want the stronger %q", role.Independence.Dimension, domain.IndependenceVendorFamily)
	}
	if role.TrustRequirement != domain.TrustRequirementProtected {
		t.Fatalf("trust = %q, want the stronger %q", role.TrustRequirement, domain.TrustRequirementProtected)
	}
	if !role.Independence.HumanSubstitutionPermitted {
		t.Fatal("both rules permitted human substitution and the merge withdrew it")
	}
}

// The one member that is a PERMISSION rather than an obligation is intersected.
// This is the mutation check for the merge above: with everything else
// unchanged, one rule refusing the substitution must make the merged
// requirement refuse it too.
func TestHumanSubstitutionIsPermittedOnlyWhenEveryRulePermitsIt(t *testing.T) {
	input := withEngineeringRequirements(t, domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role: domain.RoleSecurityReviewer,
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceVendorFamily, HumanSubstitutionPermitted: true,
			},
			Statement: "A security reviewer is required.",
		}},
	})
	addRule(&input, "RULE-NO-SUBSTITUTION", domain.PlanRequirements{
		Roles: []domain.RoleRequirement{{
			Role: domain.RoleSecurityReviewer,
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceVendorFamily, HumanSubstitutionPermitted: false,
			},
			Statement: "No human substitution for this boundary.",
		}},
	})

	contract := compileInput(t, input)
	if contract.PlanRequirements.Roles[0].Independence.HumanSubstitutionPermitted {
		t.Fatal("a rule that refused human substitution was overridden by one that permitted it")
	}
}

func TestGateObligationsMergeClaimsAndRefuseUndefinedClaims(t *testing.T) {
	input := withEngineeringRequirements(t, domain.PlanRequirements{
		Gates: []domain.GateRequirement{{
			Kind:           domain.StageAssuranceGate,
			RequiredClaims: []string{"claim-security-review"},
			Statement:      "Assurance must record the review.",
		}},
	})
	addRule(&input, "RULE-SECOND-GATE", domain.PlanRequirements{
		Gates: []domain.GateRequirement{{
			Kind:           domain.StageAssuranceGate,
			RequiredClaims: []string{"claim-auth-regression-tests"},
			Statement:      "Assurance must record the regression tests.",
		}},
	})
	contract := compileInput(t, input)
	if len(contract.PlanRequirements.Gates) != 1 {
		t.Fatalf("two obligations over one gate produced %d gates", len(contract.PlanRequirements.Gates))
	}
	want := []string{"claim-auth-regression-tests", "claim-security-review"}
	if !reflect.DeepEqual(contract.PlanRequirements.Gates[0].RequiredClaims, want) {
		t.Fatalf("claims = %#v, want the union %#v", contract.PlanRequirements.Gates[0].RequiredClaims, want)
	}

	undefined := withEngineeringRequirements(t, domain.PlanRequirements{
		Gates: []domain.GateRequirement{{
			Kind:           domain.StageAssuranceGate,
			RequiredClaims: []string{"claim-nobody-defines"},
			Statement:      "A gate over a claim nothing produces.",
		}},
	})
	if _, err := policy.Compile(undefined); err == nil || !strings.Contains(err.Error(), "claim-nobody-defines") {
		t.Fatalf("expected a gate over an undefined claim to be refused, got %v", err)
	}
}

// Unknown vocabulary is refused before it can reach a plan. The schema catches
// it first - the compiler validates its policy input - and the compiler keeps
// its own guard behind that, so widening a schema later cannot silently admit a
// role or gate kind nothing can resolve. The assertion is on the refusal and on
// the offending token, which is what holds whichever layer answers.
func TestEngineeringRequirementsRefuseUnknownVocabulary(t *testing.T) {
	cases := []struct {
		name         string
		requirements domain.PlanRequirements
		detail       string
	}{
		{
			name:         "unknown role",
			requirements: domain.PlanRequirements{Roles: []domain.RoleRequirement{{Role: "release_manager", Statement: "x"}}},
			detail:       "roles/0/role",
		},
		{
			name: "unknown capability",
			requirements: domain.PlanRequirements{
				Roles: []domain.RoleRequirement{{Role: domain.RoleReviewer, Capabilities: []domain.EngineeringCapability{"deploy_production"}, Statement: "x"}},
			},
			detail: "roles/0/capabilities/0",
		},
		{
			name: "unknown independence dimension",
			requirements: domain.PlanRequirements{
				Roles: []domain.RoleRequirement{{
					Role:         domain.RoleReviewer,
					Independence: &domain.IndependenceRequirement{Dimension: "different_person_probably"},
					Statement:    "x",
				}},
			},
			detail: "roles/0/independence/dimension",
		},
		{
			// A gate is not worker execution. Allowing an `agent` gate would be
			// the fake-worker-run that scenario 21 exists to refuse.
			name: "agent stage as a gate",
			requirements: domain.PlanRequirements{
				Gates: []domain.GateRequirement{{Kind: domain.StageAgent, Statement: "x"}},
			},
			detail: "gates/0/kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := policy.Compile(withEngineeringRequirements(t, tc.requirements)); err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("expected refusal containing %q, got %v", tc.detail, err)
			}
		})
	}
}

// A policy that states no plan obligations must compile to a contract with NO
// plan_requirements member at all. Contract revisions and run identities are
// derived from the canonical document, so an added empty object would
// re-identify every historical contract.
func TestContractsWithoutPlanObligationsStayByteIdentical(t *testing.T) {
	contract := compileInput(t, fixtureInput(t, "security-sensitive.engineering-fact.json"))
	if contract.PlanRequirements != nil {
		t.Fatalf("a policy stating no plan obligations produced %#v", contract.PlanRequirements)
	}
	encoded, err := domain.Encode(contract)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "plan_requirements") {
		t.Fatalf("canonical contract gained a plan_requirements member: %s", encoded)
	}
}

// withEngineeringRequirements builds a compile input whose security-sensitive
// rule also states the given plan obligations.
func withEngineeringRequirements(t *testing.T, requirements domain.PlanRequirements) policy.CompileInput {
	t.Helper()
	input := fixtureInput(t, "security-sensitive.engineering-fact.json")
	addRule(&input, "RULE-PLAN-OBLIGATIONS", requirements)
	return input
}

// addRule appends one more rule matching the same security-sensitive fact, so
// two rules resolve together and the merge behaviour is what is under test.
func addRule(input *policy.CompileInput, id string, requirements domain.PlanRequirements) {
	rules := make(map[string]domain.PolicyRule, len(input.Policy.Rules)+1)
	for existing, rule := range input.Policy.Rules {
		rules[existing] = rule
	}
	rules[id] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{EngineeringRequirements: &requirements},
	}
	input.Policy.Rules = rules
}
