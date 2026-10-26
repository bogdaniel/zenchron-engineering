package runtime

import (
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// publicationGateGuidance tells the planner when a publication
// human_decision_gate exists to propose. The validator refuses a gate for a
// publication action unless the contract binds a human_approval claim to that
// exact action, so offering one otherwise only produces refusals (#695); the
// runtime opens the pull request through its own governed publication either
// way, and a stage a selected template requires is stated separately and stays
// required. This is guidance only: nothing here widens what the validator accepts.
func publicationGateGuidance(contract domain.EngineeringWorkContract) string {
	gates := planning.PublicationGates(contract)
	if len(gates) == 0 {
		return "\n- do not propose a human_decision_gate for a publication action " +
			"(git.pull_request.create, git.pull_request.update, git.push) on your own: this work contract " +
			"binds no human_approval claim to one, so it could not be satisfied. The runtime opens the " +
			"pull request through its own governed publication; mention of authority, approval or " +
			"publication in the objective does not by itself call for a gate. A gate a required " +
			"template stage names above is the exception."
	}
	offers := make([]string, 0, len(gates))
	for _, gate := range gates {
		offers = append(offers, fmt.Sprintf("action %s:%s with required_claims chosen from %s",
			gate.Action.Type, gate.Action.Target, strings.Join(gate.Claims, ", ")))
	}
	return "\n- a publication human_decision_gate is possible only as: " + strings.Join(offers, "; ") +
		". Propose one only if the objective needs a human decision there; otherwise leave publication " +
		"to the runtime's governed publication. Any other publication gate is refused."
}
