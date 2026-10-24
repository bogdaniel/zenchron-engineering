package policy_test

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// fileSizeClaim is the claim the runtime recognizes (runtime.FileSizePolicyClaimID).
// It is repeated here, not imported, because the policy package must not depend
// on the runtime; the runtime's own test pins the same spelling.
const fileSizeClaim = "claim-file-size-policy"

// withFileSizeObligation applies, to every rule, exactly the additions
// docs/file-size-policy.md tells the operator to make.
func withFileSizeObligation(in domain.EngineeringPolicy, revision string) domain.EngineeringPolicy {
	out := in
	out.Revision = revision
	out.Rules = map[string]domain.PolicyRule{}
	for id, rule := range in.Rules {
		effect := rule.Effect
		claims := map[string]domain.RequiredClaim{}
		if effect.RequiredClaims != nil {
			for k, v := range *effect.RequiredClaims {
				claims[k] = v
			}
		}
		claims[fileSizeClaim] = domain.RequiredClaim{EvidenceClass: "automated_test", IndependentFromChangeProducer: true}
		effect.RequiredClaims = &claims
		obligations := map[string]domain.PolicyRequirement{}
		if effect.Obligations != nil {
			for k, v := range *effect.Obligations {
				obligations[k] = v
			}
		}
		obligations["file-size-policy"] = domain.PolicyRequirement{
			Statement:      "The repository's file-size policy must hold on the exact candidate tree.",
			RequiredClaims: &[]string{fileSizeClaim},
			Material:       true,
		}
		effect.Obligations = &obligations
		if effect.AuthorityConditions != nil {
			conditions := make([]domain.AuthorityCondition, 0, len(*effect.AuthorityConditions))
			for _, condition := range *effect.AuthorityConditions {
				condition.RequiredClaims = append(append([]string{}, condition.RequiredClaims...), fileSizeClaim)
				conditions = append(conditions, condition)
			}
			effect.AuthorityConditions = &conditions
		}
		discharge := []string{fileSizeClaim}
		if effect.AcceptanceDischargeClaims != nil {
			discharge = append(append([]string{}, *effect.AcceptanceDischargeClaims...), fileSizeClaim)
		}
		effect.AcceptanceDischargeClaims = &discharge
		out.Rules[id] = domain.PolicyRule{When: rule.When, Effect: effect}
	}
	return out
}

// The file-size requirement reaches a contract only through policy compilation,
// and a contract compiled under the previous policy revision keeps that
// revision's identity and has no such claim: nothing is retroactive.
func TestFileSizeObligationAppliesThroughPolicyCompilationOnly(t *testing.T) {
	before := fixtureInput(t, "normal-behavior.engineering-fact.json")
	frozen := compileInput(t, before)
	if _, present := frozen.RequiredClaims[fileSizeClaim]; present {
		t.Fatal("the unmodified policy already required the file-size claim")
	}

	after := before
	after.Policy = withFileSizeObligation(before.Policy, before.Policy.Revision+"-file-size")
	compiled := compileInput(t, after)

	claim, present := compiled.RequiredClaims[fileSizeClaim]
	if !present || claim.EvidenceClass != "automated_test" {
		t.Fatalf("the compiled contract does not require the claim as automated_test: %+v", compiled.RequiredClaims)
	}
	if _, ok := compiled.Obligations["file-size-policy"]; !ok {
		t.Fatalf("the obligation did not compile: %v", compiled.Obligations)
	}
	var gated bool
	for _, condition := range compiled.AuthorityConditions {
		for _, id := range condition.RequiredClaims {
			gated = gated || id == fileSizeClaim
		}
	}
	if !gated {
		t.Fatalf("no protected action waits on the claim: %+v", compiled.AuthorityConditions)
	}
	if frozen.Provenance.Policy == compiled.Provenance.Policy {
		t.Fatalf("the two contracts share a policy identity: %+v", frozen.Provenance.Policy)
	}
	if _, present := frozen.RequiredClaims[fileSizeClaim]; present {
		t.Fatal("recompiling under the new policy rewrote the earlier contract")
	}
}
