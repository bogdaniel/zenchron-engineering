package planning_test

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

func gatePlan(contract domain.EngineeringWorkContract, action domain.Action, claim string) domain.EngineeringPlan {
	return domain.EngineeringPlan{
		ID: "plan", Revision: 1, Objective: "publish candidate", Subject: contract.Subject,
		Provenance: domain.PlanProvenance{Contract: domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}},
		Stages:     []domain.PlanStage{{ID: "publish-approval", Kind: domain.StageHumanDecisionGate, Action: &action, RequiredClaims: []string{claim}}},
	}
}

// The planner is offered a gate only if Validate accepts it, and every other
// publication action and claim pairing is still refused (#695).
func TestEveryAdvertisedPublicationGateIsAcceptedAndNothingElseIs(t *testing.T) {
	create := domain.Action{Type: "git.pull_request.create", Target: "main"}
	update := domain.Action{Type: "git.pull_request.update", Target: "main"}
	contracts := map[string]domain.EngineeringWorkContract{
		"one claim bound to one action": {
			RequiredClaims:      map[string]domain.RequiredClaim{"validation": {EvidenceClass: "automated_test"}, "approval": {EvidenceClass: "human_approval"}},
			AuthorityConditions: []domain.AuthorityCondition{{Action: create, RequiredClaims: []string{"validation", "approval"}}},
		},
		"one claim shared by two actions": {
			RequiredClaims: map[string]domain.RequiredClaim{"approval": {EvidenceClass: "human_approval"}},
			AuthorityConditions: []domain.AuthorityCondition{
				{Action: create, RequiredClaims: []string{"approval"}},
				{Action: update, RequiredClaims: []string{"approval"}},
			},
		},
		"only an automated claim": {
			RequiredClaims:      map[string]domain.RequiredClaim{"validation": {EvidenceClass: "automated_test"}},
			AuthorityConditions: []domain.AuthorityCondition{{Action: create, RequiredClaims: []string{"validation"}}},
		},
	}
	for name, contract := range contracts {
		t.Run(name, func(t *testing.T) {
			contract.ID, contract.Revision = "contract", "1"
			contract.Subject = domain.Subject{Repository: "acme/repo", Revision: "abc"}
			advertised := map[domain.Action]map[string]bool{}
			for _, gate := range planning.PublicationGates(contract) {
				advertised[gate.Action] = map[string]bool{}
				for _, claim := range gate.Claims {
					advertised[gate.Action][claim] = true
				}
			}
			for _, action := range []domain.Action{create, update} {
				for claim := range contract.RequiredClaims {
					err := planning.Validate(gatePlan(contract, action, claim), planning.ValidationInput{Contract: contract})
					if advertised[action][claim] && err != nil {
						t.Errorf("advertised gate %s:%s with %q was refused: %v", action.Type, action.Target, claim, err)
					}
					if !advertised[action][claim] && err == nil {
						t.Errorf("gate %s:%s with %q is accepted but not advertised", action.Type, action.Target, claim)
					}
				}
			}
		})
	}
}
