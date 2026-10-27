package policy_test

import (
	"slices"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/authority"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

// browserClaim is the claim the runtime recognizes
// (runtime.BrowserAcceptanceClaimID), repeated here because the policy package
// must not depend on the runtime; the runtime's own test pins the same spelling.
const browserClaim = "claim-browser-acceptance"

// withBrowserAcceptanceRule applies exactly the addition docs/browser-acceptance.md
// tells the operator to make: one rule, matching only when the project model's
// user_interface boundary was modified. It adds a material obligation and no
// authority condition of its own: a second rule's condition on the same action
// would be refused as conflicting, and a material obligation's claims already
// gate every protected action (authority.requiredClaimsFor).
func withBrowserAcceptanceRule(in domain.EngineeringPolicy, revision string) domain.EngineeringPolicy {
	out := in
	out.Revision = revision
	out.Rules = map[string]domain.PolicyRule{}
	for id, rule := range in.Rules {
		out.Rules[id] = rule
	}
	claims := map[string]domain.RequiredClaim{
		browserClaim: {EvidenceClass: "automated_test", IndependentFromChangeProducer: true},
	}
	obligations := map[string]domain.PolicyRequirement{
		"browser-acceptance": {
			Statement:      "The dashboard must pass its real-browser acceptance tests on the exact candidate.",
			RequiredClaims: &[]string{browserClaim},
			Material:       true,
		},
	}
	out.Rules["USER-INTERFACE-TOUCHED-001"] = domain.PolicyRule{
		When: domain.PolicyCondition{Fact: "user_interface.boundary_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{
			RequiredClaims: &claims, Obligations: &obligations,
		},
	}
	return out
}

func userInterfaceFact(modified bool) domain.EngineeringFact {
	value := domain.FactFalse
	if modified {
		value = domain.FactTrue
	}
	return domain.EngineeringFact{
		SchemaVersion: domain.SchemaVersion, ID: "fact-user-interface", Key: "user_interface.boundary_modified",
		Value: value, Stage: domain.StageObserved, Confidence: domain.ConfidenceHigh,
		Subject:    domain.Subject{Repository: "acme/payments", Revision: "rev-a"},
		Provenance: domain.FactProvenance{Type: "path_analysis", Producer: "boundary-detector-v1"},
	}
}

// The browser requirement exists only where a rule asks for it, and only when
// the user interface was touched: work that does not touch it compiles to the
// contract it always had.
func TestBrowserAcceptanceIsRequiredOnlyWhenTheUserInterfaceChanged(t *testing.T) {
	base := fixtureInput(t, "normal-behavior.engineering-fact.json")
	base.Policy = withBrowserAcceptanceRule(base.Policy, base.Policy.Revision+"-browser")

	touched := base
	touched.Facts = append(append([]domain.EngineeringFact{}, base.Facts...), userInterfaceFact(true))
	compiled := compileInput(t, touched)
	claim, present := compiled.RequiredClaims[browserClaim]
	if !present || claim.EvidenceClass != "automated_test" {
		t.Fatalf("a user interface change did not require browser acceptance as automated_test: %+v", compiled.RequiredClaims)
	}
	obligation, present := compiled.Obligations["browser-acceptance"]
	if !present || !obligation.Material || len(obligation.RequiredClaims) != 1 || obligation.RequiredClaims[0] != browserClaim {
		t.Fatalf("the browser obligation is not a material obligation on the claim: %+v", compiled.Obligations)
	}

	// Publication waits on the claim: with no browser evidence at all the
	// protected action is not authorized and the claim is named as missing.
	decision, err := authority.Evaluate(authority.Input{
		DecisionID: "decision-browser", DecisionRevision: "1", Contract: compiled,
		Action:         domain.Action{Type: "git.pull_request.create", Target: "main"},
		Capability:     domain.CapabilityAvailable,
		ChangeProducer: domain.EvidenceProducer{ID: "change-producer", Type: domain.ProducerExecutionProvider},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Status == domain.AuthorityAuthorized || !slices.Contains(decision.Missing, browserClaim) {
		t.Fatalf("publication was not held for browser evidence: %+v", decision)
	}

	untouched := base
	untouched.Facts = append(append([]domain.EngineeringFact{}, base.Facts...), userInterfaceFact(false))
	if compiled := compileInput(t, untouched); func() bool { _, present := compiled.RequiredClaims[browserClaim]; return present }() {
		t.Fatal("work that did not touch the user interface was required to pass browser acceptance")
	}
}
