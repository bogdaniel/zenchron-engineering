package runtime

import "testing"

func TestCheckReviewIndependenceRefusesTheSameAgent(t *testing.T) {
	agent := ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI}
	err := CheckReviewIndependence(agent, agent)
	if err == nil {
		t.Fatal("expected the same agent reviewing its own work to be refused")
	}
	refused, ok := err.(*ReviewIndependenceError)
	if !ok || refused.Dimension != "execution_agent" {
		t.Fatalf("expected an execution_agent refusal, got %#v", err)
	}
}

// Mutation check: two DIFFERENT agent ids backed by the same vendor family
// (the exact case #233 itself warns about) must still be refused. Removing
// the vendor-family comparison below must make this test fail.
func TestCheckReviewIndependenceRefusesTheSameVendorFamilyUnderADifferentAgentID(t *testing.T) {
	producer := ResolvedAgent{ID: "codex-cli", Kind: AgentKindCodexCLI}
	reviewer := ResolvedAgent{ID: "openai-responses", Kind: AgentKindOpenAIResponses}
	if VendorFamilyFor(producer.Kind) != VendorFamilyFor(reviewer.Kind) {
		t.Fatalf("test fixture assumption broken: %q and %q are expected to share a vendor family", producer.Kind, reviewer.Kind)
	}
	err := CheckReviewIndependence(producer, reviewer)
	if err == nil {
		t.Fatal("expected a different agent id of the same vendor family to be refused")
	}
	refused, ok := err.(*ReviewIndependenceError)
	if !ok || refused.Dimension != "vendor_family" {
		t.Fatalf("expected a vendor_family refusal, got %#v", err)
	}
}

func TestCheckReviewIndependenceAllowsADifferentVendor(t *testing.T) {
	producer := ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode}
	reviewer := ResolvedAgent{ID: "gemini", Kind: AgentKindGeminiCLI}
	if err := CheckReviewIndependence(producer, reviewer); err != nil {
		t.Fatalf("expected independent vendors to be allowed, got %v", err)
	}
}
