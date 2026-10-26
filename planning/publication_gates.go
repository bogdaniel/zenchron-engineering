package planning

import "github.com/bogdaniel/zenchron-engineering/domain"

// PublicationGate is a publication action together with the human_approval
// claims the work contract itself binds to it. It is the only shape of
// publication human_decision_gate that Validate can accept for that action.
type PublicationGate struct {
	Action domain.Action
	Claims []string
}

// PublicationGates lists exactly the publication gates Validate accepts for a
// contract, in authority-condition order. A contract that binds no usable
// human_approval claim to a publication action offers none.
func PublicationGates(contract domain.EngineeringWorkContract) []PublicationGate {
	var gates []PublicationGate
	for _, condition := range contract.AuthorityConditions {
		if !publicationAction(condition.Action) {
			continue
		}
		var claims []string
		for _, id := range condition.RequiredClaims {
			definition, ok := contract.RequiredClaims[id]
			if !ok || definition.EvidenceClass != "human_approval" {
				continue
			}
			// Validate binds a claim to the FIRST publication action that
			// requires it, so a claim shared by two actions is acceptable on
			// that one only; advertising it for the other would offer a gate
			// Validate refuses.
			if bound, _ := publicationConditionForClaim(contract, id); bound == condition.Action {
				claims = append(claims, id)
			}
		}
		if len(claims) > 0 {
			gates = append(gates, PublicationGate{Action: condition.Action, Claims: claims})
		}
	}
	return gates
}
