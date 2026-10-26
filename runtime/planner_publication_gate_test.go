package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func publicationContractPrompt(t *testing.T, claims map[string]domain.RequiredClaim, conditions []domain.AuthorityCondition) string {
	t.Helper()
	input, _ := plannerFixture(t, goodAnswer)
	input.Contract.RequiredClaims = claims
	input.Contract.AuthorityConditions = conditions
	return plannerOutputContract(input)
}

var publishMain = domain.Action{Type: "git.pull_request.create", Target: "main"}

// #693: the contract bound only an automated_test claim to publication, the
// planner proposed a publication gate anyway, and every proposal was refused.
func TestAContractWithoutAHumanApprovalClaimDoesNotInviteAPublicationGate(t *testing.T) {
	contract := publicationContractPrompt(t,
		map[string]domain.RequiredClaim{"claim-validation": {EvidenceClass: "automated_test"}},
		[]domain.AuthorityCondition{{Action: publishMain, RequiredClaims: []string{"claim-validation"}}})
	if !strings.Contains(contract, "do not propose a human_decision_gate for a publication action") {
		t.Fatalf("the planner is not told to leave publication to the runtime:\n%s", contract)
	}
	if strings.Contains(contract, "publication human_decision_gate is possible only as") {
		t.Fatalf("a publication gate is offered with no human_approval claim to satisfy it:\n%s", contract)
	}
}

func TestAHumanApprovalClaimBoundToThePublicationActionKeepsTheGateOption(t *testing.T) {
	contract := publicationContractPrompt(t,
		map[string]domain.RequiredClaim{
			"claim-validation":           {EvidenceClass: "automated_test"},
			"claim-publication-approval": {EvidenceClass: "human_approval"},
		},
		[]domain.AuthorityCondition{{Action: publishMain, RequiredClaims: []string{"claim-validation", "claim-publication-approval"}}})
	want := "action git.pull_request.create:main with required_claims chosen from claim-publication-approval."
	if !strings.Contains(contract, want) {
		t.Fatalf("the bound gate option is not stated exactly (%q):\n%s", want, contract)
	}
	if strings.Contains(contract, "chosen from claim-validation") || strings.Contains(contract, "claim-publication-approval, claim-validation") {
		t.Fatalf("an automated_test claim is offered to a publication gate:\n%s", contract)
	}
}

func TestAHumanApprovalClaimUnrelatedToPublicationDoesNotJustifyAGate(t *testing.T) {
	claims := map[string]domain.RequiredClaim{
		"claim-validation":       {EvidenceClass: "automated_test"},
		"claim-security-signoff": {EvidenceClass: "human_approval"},
	}
	for name, conditions := range map[string][]domain.AuthorityCondition{
		"bound to another action":         {{Action: domain.Action{Type: "deploy.release", Target: "prod"}, RequiredClaims: []string{"claim-security-signoff"}}},
		"not required by the publication": {{Action: publishMain, RequiredClaims: []string{"claim-validation"}}},
	} {
		t.Run(name, func(t *testing.T) {
			contract := publicationContractPrompt(t, claims, conditions)
			if strings.Contains(contract, "publication human_decision_gate is possible only as") {
				t.Fatalf("an unrelated human_approval claim justified a publication gate:\n%s", contract)
			}
		})
	}
}
