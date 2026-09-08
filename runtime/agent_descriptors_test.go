package runtime

// The planner's view of the workforce. What matters here is that the projection
// states facts a planner can reason over WITHOUT learning a provider's name,
// and that it never overstates what an adapter can do.

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestVendorFamilyIsSeparateFromProviderKind(t *testing.T) {
	// Two adapter ids over one vendor are NOT vendor-family independent. This
	// is the fact that makes that statement expressible at all.
	if VendorFamilyFor(AgentKindCodexCLI) != VendorFamilyFor(AgentKindOpenAIResponses) {
		t.Fatal("two OpenAI-backed adapters report different vendor families")
	}
	if VendorFamilyFor(AgentKindClaudeCode) == VendorFamilyFor(AgentKindCodexCLI) {
		t.Fatal("Claude Code and Codex report one vendor family")
	}
	// An unknown kind is UNKNOWN rather than empty: two unknowns are not
	// evidence of independence, and the resolver must not read them as
	// automatically distinct.
	if VendorFamilyFor("some_future_adapter") != "unknown" {
		t.Fatalf("unknown kind reported vendor family %q", VendorFamilyFor("some_future_adapter"))
	}
}

func TestInvocationModesStateWhatAnAdapterCanProve(t *testing.T) {
	planning := map[string]bool{
		AgentKindCodexCLI: true, AgentKindClaudeCode: true, AgentKindQwenCLI: true,
		// Gemini exposes no mode whose non-mutating boundary this runtime can
		// prove, so it is ineligible for planner-role stages. Claiming it would
		// be claiming a restriction that does not exist.
		AgentKindGeminiCLI: false,
		// The brokered provider's non-mutating mode is a separate piece of work
		// (its tool surface, not a CLI flag), and until it exists the honest
		// answer is that it cannot plan.
		AgentKindOpenAIResponses: false,
	}
	for kind, wantPlanning := range planning {
		modes := invocationModesFor(kind)
		mutating, nonMutating := false, false
		for _, mode := range modes {
			switch mode {
			case domain.InvocationModeMutating:
				mutating = true
			case domain.InvocationModeNonMutatingPlanning:
				nonMutating = true
			}
		}
		if !mutating {
			t.Fatalf("kind %q cannot perform ordinary producer execution", kind)
		}
		if nonMutating != wantPlanning {
			t.Fatalf("kind %q non-mutating planning = %v, want %v", kind, nonMutating, wantPlanning)
		}
	}
}

func TestDescribeExecutionAgentsProjectsTheRegistry(t *testing.T) {
	registry, err := OperatorConfig{
		Agents: map[string]AgentConfig{
			"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted), Model: "sonnet"},
			"codex":  {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)},
		},
		DefaultAgent: "codex",
	}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	descriptors := DescribeExecutionAgents(context.Background(), registry, func(ResolvedAgent) AgentProber { return nil })
	if len(descriptors) != 2 || descriptors[0].ID != "claude" || descriptors[1].ID != "codex" {
		t.Fatalf("descriptors = %#v", descriptors)
	}
	if descriptors[0].VendorFamily != "anthropic" || descriptors[1].VendorFamily != "openai" {
		t.Fatalf("vendor families = %q, %q", descriptors[0].VendorFamily, descriptors[1].VendorFamily)
	}
	if descriptors[0].Model != "sonnet" {
		t.Fatalf("model = %q", descriptors[0].Model)
	}
	// Readiness is an OBSERVATION. With no prober nothing was observed, so
	// nothing is available - never "assumed available".
	for _, descriptor := range descriptors {
		if descriptor.Available {
			t.Fatalf("agent %q reported available with no readiness observation", descriptor.ID)
		}
		if len(descriptor.Capabilities) == 0 {
			t.Fatalf("agent %q advertises no capability at all", descriptor.ID)
		}
	}
}
