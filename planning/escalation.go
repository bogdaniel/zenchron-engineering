package planning

// The frozen customization law:
//
//	Customization may specialize or narrow. It may not escalate.
//
// This file is where that stops being prose. A profile is checked against the
// worker it claims to specialize, and every way a profile could try to gain
// something its worker does not have is refused with the reason.
//
// Two of the escalations #64 names are unrepresentable rather than checked
// here, which is stronger: ProfileConstraints has no member that RAISES a
// ceiling or grants a permission bypass, and ArtifactSource has no repository
// class. A test asserts both, because "you cannot write it down" is only a
// guarantee while the type stays that way.

import (
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// EscalationError is the typed refusal for a profile that would gain authority,
// trust or capability its underlying worker does not have.
type EscalationError struct {
	Profile string
	Agent   string
	Detail  string
}

func (e *EscalationError) Error() string {
	return fmt.Sprintf("profile %q may not escalate agent %q: %s", e.Profile, e.Agent, e.Detail)
}

// Bind checks every installed profile against the registered workforce.
//
// It is called where the workforce is known - the composition root and the
// resolver - rather than at load time, because a registry is valid on its own
// and only its RELATIONSHIP to the configured agents can be escalating.
func (r Registry) Bind(agents []domain.ExecutionAgentDescriptor) error {
	byID := make(map[string]domain.ExecutionAgentDescriptor, len(agents))
	for _, agent := range agents {
		byID[agent.ID] = agent
	}
	for _, profile := range r.Profiles() {
		agent, ok := byID[profile.ExecutionAgent]
		if !ok {
			return &EscalationError{
				Profile: profile.ID, Agent: profile.ExecutionAgent,
				Detail: "no such agent is configured; a profile specializes a worker the operator already registered and can never introduce one",
			}
		}
		if err := RefuseEscalation(profile, agent); err != nil {
			return err
		}
	}
	return nil
}

// RefuseEscalation is the check for one profile against one worker.
func RefuseEscalation(profile domain.AgentProfile, agent domain.ExecutionAgentDescriptor) error {
	refuse := func(detail string) error {
		return &EscalationError{Profile: profile.ID, Agent: agent.ID, Detail: detail}
	}
	// TRUST. A profile states the trust its stages require. Requiring MORE than
	// the worker holds is not a promotion of the worker: it is a profile that
	// can never be eligible, and saying so at configuration time is better than
	// discovering it when work is already waiting.
	if trustStrength(profile.TrustRequirement) > trustStrength(agent.TrustMode) {
		return refuse(fmt.Sprintf("it requires %q execution trust while the agent's adapter is %q; a trust mode is a property of the adapter and no configuration can raise it",
			profile.TrustRequirement, agent.TrustMode))
	}
	// CAPABILITY. A profile narrows what a worker is asked to do. Advertising
	// an ability the worker does not have would make the resolver select it for
	// work it cannot perform.
	held := make(map[domain.EngineeringCapability]bool, len(agent.Capabilities))
	for _, capability := range agent.Capabilities {
		held[capability] = true
	}
	var missing []string
	for _, capability := range profile.Capabilities {
		if !held[capability] {
			missing = append(missing, string(capability))
		}
	}
	if len(missing) > 0 {
		return refuse(fmt.Sprintf("it advertises capabilities the agent does not hold (%s); a profile may narrow the agent's capability set and never extend it",
			strings.Join(missing, ", ")))
	}
	if len(profile.Capabilities) == 0 {
		return refuse("it advertises no capability at all, so no stage could ever be eligible for it")
	}
	// INSTRUCTION PROVENANCE. Instruction content is operator authority. The
	// schema already refuses a repository source, and this refuses anything
	// that is not one of the operator classes, so widening the schema later
	// cannot quietly admit candidate-authored instruction.
	if !operatorOwned(profile.Source) {
		return refuse(fmt.Sprintf("its definition claims source %q, which is not operator-owned", profile.Source.Type))
	}
	return nil
}

// RefusePackEscalation checks the provenance of instruction content itself.
// It is separate from the profile check because a pack can be installed and
// referenced by several profiles, and the trust question is about the PACK.
func RefusePackEscalation(pack domain.InstructionPack) error {
	if !operatorOwned(pack.Source) {
		return fmt.Errorf("instruction pack %q claims source %q: model-visible instruction is operator-owned authority configuration, and candidate or repository content may never become one",
			pack.ID, pack.Source.Type)
	}
	return nil
}

func operatorOwned(source domain.ArtifactSource) bool {
	return source.Type == domain.SourceOperatorConfig || source.Type == domain.SourceOperatorFile
}

// trustStrength orders execution trust. It matches the compiler's ordering
// exactly; a second ordering would be a second definition of "stricter".
func trustStrength(trust domain.TrustRequirement) int {
	switch trust {
	case domain.TrustRequirementProtected:
		return 2
	case domain.TrustRequirementOperatorTrusted:
		return 1
	default:
		return 0
	}
}
