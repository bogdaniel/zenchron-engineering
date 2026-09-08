// Package planning is the Engineering Planner: it compiles engineering intent,
// ProjectModel facts, compiled EngineeringPolicy obligations and an optional
// reusable template into a proposed EngineeringPlan, validates that plan
// deterministically, and resolves its executable stages onto operator-defined
// AgentProfiles backed by registered #63 execution agents.
//
// What it deliberately does NOT contain:
//
//   - a scheduler. Dependency-ready stages become ordinary EngineeringRuns and
//     the existing #63 scheduler and leases decide when they execute.
//   - a policy system. Role, capability, independence and gate obligations are
//     compiled by the existing policy compiler and arrive here inside a work
//     contract.
//   - an authority system. Assurance and human gates reference the evidence and
//     authority state the kernel already owns.
//   - any provider name. Providers reach this package as descriptors the
//     runtime builds; nothing here knows what a Codex or a Claude is.
package planning

import "github.com/bogdaniel/zenchron-engineering/domain"

// roleCapabilities is the capability floor for each role: what performing that
// responsibility requires at all, whatever else a policy or template adds.
//
// It is a floor rather than a description. Today's coding CLIs advertise
// substantially overlapping abilities, so this cannot pretend to discriminate
// between workers; what it does is stop a plan from asking a profile that
// cannot change code to implement something, which is a real refusal.
var roleCapabilities = map[domain.EngineeringRole][]domain.EngineeringCapability{
	domain.RoleProductArchitect: {domain.CapabilityRequirementsAnalysis},
	domain.RoleSystemArchitect:  {domain.CapabilityArchitectureReasoning, domain.CapabilityRepositoryAnalysis},
	domain.RolePlanner:          {domain.CapabilityRequirementsAnalysis, domain.CapabilityRepositoryAnalysis},
	domain.RoleImplementer:      {domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
	domain.RoleTester:           {domain.CapabilityVerification, domain.CapabilityCodeChange},
	domain.RoleSecurityReviewer: {domain.CapabilitySecurityReview, domain.CapabilityRepositoryAnalysis},
	domain.RoleReviewer:         {domain.CapabilityRepositoryAnalysis},
	domain.RoleIntegrator:       {domain.CapabilityCodeChange, domain.CapabilityRepositoryAnalysis},
	domain.RoleReleaseReviewer:  {domain.CapabilityRepositoryAnalysis},
}

// RoleCapabilities is the capability floor for one role, in the ontology's
// canonical order.
func RoleCapabilities(role domain.EngineeringRole) []domain.EngineeringCapability {
	floor := roleCapabilities[role]
	result := make([]domain.EngineeringCapability, 0, len(floor))
	for _, capability := range domain.EngineeringCapabilities() {
		for _, required := range floor {
			if required == capability {
				result = append(result, capability)
				break
			}
		}
	}
	return result
}

// materialProducerRoles are the roles whose work CHANGES the candidate. The
// distinction is load-bearing for independence: the existing law is that a
// producer of a material change cannot be its sole source of acceptance
// evidence, so "independent of the material producer" needs a stated set of
// roles that produce one.
var materialProducerRoles = map[domain.EngineeringRole]bool{
	domain.RoleImplementer: true,
	domain.RoleTester:      true,
	domain.RoleIntegrator:  true,
}

// ProducesMaterialChange reports whether a role's work is a material change.
//
// A reviewing role that turned out to write code would be a defect in the plan
// rather than a reclassification here: the runtime observes what a candidate
// actually changed, and this states what the plan INTENDED, which is the fact
// independence is compiled against.
func ProducesMaterialChange(role domain.EngineeringRole) bool { return materialProducerRoles[role] }

// RequiredInvocationMode is the provider mode a role's stage requires.
//
// The planner role is the one that must not write: its job is to reason about
// decomposition and emit a PlanRevisionProposal, and a reasoning invocation
// with candidate-write authority is exactly the hidden autonomous plan
// replacement #64 refuses. Every other role performs ordinary bounded producer
// execution.
func RequiredInvocationMode(role domain.EngineeringRole) domain.InvocationMode {
	if role == domain.RolePlanner {
		return domain.InvocationModeNonMutatingPlanning
	}
	return domain.InvocationModeMutating
}
