package runtime

// The planner's view of the workforce.
//
// #64 reasons about roles, capabilities, trust and independence. #63 owns
// agents, adapters and provider kinds. This file is the ONE translation between
// them: it projects the configured agent registry into provider-independent
// descriptors, and it is the only place outside agent_specs.go that has to know
// which vendor a provider kind belongs to.
//
// Nothing in the planner, the resolver or the kernel branches on a provider
// name. They branch on the descriptor's vendor family, trust mode, capability
// set and invocation modes - which is what makes "Claude through two adapters is
// not vendor-family independent" expressible at all.

import (
	"context"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// agentVendorFamily maps a provider kind onto the vendor whose model family it
// actually runs.
//
// It is a SEPARATE fact from the provider kind, and that separation is
// load-bearing for independence: two adapter ids backed by the same vendor are
// not vendor-family independent, so a plan requiring vendor independence must
// not be satisfiable by adding another adapter for the same models.
var agentVendorFamily = map[string]string{
	AgentKindCodexCLI:        "openai",
	AgentKindClaudeCode:      "anthropic",
	AgentKindGeminiCLI:       "google",
	AgentKindQwenCLI:         "alibaba",
	AgentKindOpenAIResponses: "openai",
}

// VendorFamilyFor is the vendor family of one provider kind, or "unknown" for a
// kind this build does not know.
//
// Unknown is deliberately its own value rather than an empty string: two
// unknown vendors are not evidence of independence, and the resolver treats an
// unknown family as unable to prove vendor separation rather than as
// automatically distinct.
func VendorFamilyFor(kind string) string {
	if family, ok := agentVendorFamily[kind]; ok {
		return family
	}
	return "unknown"
}

// generalCodingCapabilities is what today's coding agents can be ASKED to do.
//
// Every supported provider advertises the same broad set, and that is stated
// honestly rather than differentiated for appearance. #64 is explicit that at
// M2 the resolver's discrimination comes from trust eligibility, invocation
// mode, availability, independence and operator preference - not from a
// capability taxonomy pretending four general coding CLIs have different
// skills. A capability id earns a difference here when one actually exists.
func generalCodingCapabilities() []domain.EngineeringCapability {
	return domain.EngineeringCapabilities()
}

// DescribeExecutionAgents projects the configured registry into planner-facing
// descriptors. The prober is injected exactly as DescribeAgents injects it, so
// this spends nothing and a test answers it without an executable.
func DescribeExecutionAgents(ctx context.Context, registry AgentRegistry, prober func(ResolvedAgent) AgentProber) []domain.ExecutionAgentDescriptor {
	statuses := DescribeAgents(ctx, registry, prober)
	descriptors := make([]domain.ExecutionAgentDescriptor, 0, len(statuses))
	for _, status := range statuses {
		descriptors = append(descriptors, domain.ExecutionAgentDescriptor{
			ID:              status.ID,
			ProviderKind:    status.Kind,
			VendorFamily:    VendorFamilyFor(status.Kind),
			TrustMode:       domain.TrustRequirement(status.TrustMode),
			Model:           status.Model,
			Capabilities:    generalCodingCapabilities(),
			InvocationModes: invocationModesFor(status.Kind),
			Available:       status.Eligible,
			Detail:          status.Detail,
			Unattended:      status.Unattended,
		})
	}
	sort.SliceStable(descriptors, func(i, j int) bool { return descriptors[i].ID < descriptors[j].ID })
	return descriptors
}

// invocationModesFor states which modes an adapter can actually enter.
//
// Mutating is universal: every adapter drives a worker that may change the
// candidate it was given. Non-mutating planning is NOT universal, and the
// difference is the point - a provider without an enforceable read-only mode is
// ineligible for a planner-role stage rather than being run in a permissive
// mode and trusted to behave.
func invocationModesFor(kind string) []domain.InvocationMode {
	modes := []domain.InvocationMode{domain.InvocationModeMutating}
	if spec, err := specForKind(kind); err == nil && spec.ReadOnly != nil {
		modes = append(modes, domain.InvocationModeNonMutatingPlanning)
	}
	return modes
}
