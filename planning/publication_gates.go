package planning

import "github.com/bogdaniel/zenchron-engineering/domain"

// PublicationGate is a publication action together with the human_approval
// claims the work contract itself binds to it. It is the only shape of
// publication human_decision_gate that Validate can accept for that action.
type PublicationGate struct {
	Action domain.Action
	Claims []string
}

// PublicationGates lists the publication gates a contract makes satisfiable, in
// authority-condition order. A contract that binds no human_approval claim to a
// publication action offers none, and a gate for it would be refused.
func PublicationGates(contract domain.EngineeringWorkContract) []PublicationGate {
	var gates []PublicationGate
	for _, condition := range contract.AuthorityConditions {
		if !publicationAction(condition.Action) {
			continue
		}
		var claims []string
		for _, id := range condition.RequiredClaims {
			if definition, ok := contract.RequiredClaims[id]; ok && definition.EvidenceClass == "human_approval" {
				claims = append(claims, id)
			}
		}
		if len(claims) > 0 {
			gates = append(gates, PublicationGate{Action: condition.Action, Claims: claims})
		}
	}
	return gates
}
