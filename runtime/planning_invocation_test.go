package runtime

// The non-mutating planning invocation, at the adapter boundary.
//
// #64 requires that planner reasoning runs through a REGISTERED execution agent
// in a mode whose non-mutating boundary the provider actually enforces, and
// that a provider which cannot enter such a mode is ineligible rather than run
// permissively. These tests are where "ineligible rather than permissive" is
// checked: every one of them asserts on the argument vector the adapter would
// really pass, or on the refusal it produces instead.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func planningRequest(request ExecutionRequest) ExecutionRequest {
	request.Purpose = InvocationPlanning
	request.Mode = domain.InvocationModeNonMutatingPlanning
	return request
}

func TestPlanningInvocationUsesTheProvidersOwnReadOnlyMode(t *testing.T) {
	cases := []struct {
		kind     string
		mode     string
		sandbox  string
		expected []string
	}{
		{kind: AgentKindCodexCLI, mode: "read-only", sandbox: "read-only", expected: []string{"--sandbox", "read-only"}},
		{kind: AgentKindClaudeCode, mode: "plan", expected: []string{"--permission-mode", "plan"}},
		{kind: AgentKindQwenCLI, mode: "plan", expected: []string{"--approval-mode", "plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, tc.kind)
			result, err := provider.Execute(context.Background(), planningRequest(request))
			if err != nil {
				t.Fatalf("planning invocation: %v", err)
			}
			args := strings.Join(fake.execution(t).args, " ")
			for _, want := range tc.expected {
				if !strings.Contains(args, want) {
					t.Fatalf("planning argv %q does not carry %q", args, want)
				}
			}
			// The mutating modes must be ABSENT, not merely outranked.
			for _, forbidden := range []string{"workspace-write", "danger-full-access", "acceptEdits", "bypassPermissions", "auto-edit", "yolo"} {
				if strings.Contains(args, forbidden) {
					t.Fatalf("planning argv %q carries the mutating mode %q", args, forbidden)
				}
			}
			if result.Invocation == nil || result.Invocation.PermissionMode != tc.mode {
				t.Fatalf("provenance recorded permission mode %#v, want %q", result.Invocation, tc.mode)
			}
			// Provenance records the posture the process RAN under. Codex's
			// ordinary sandbox is workspace-write, so recording the spec's
			// sandbox here would durably claim write access this invocation
			// never had - while the argv beside it said read-only.
			if result.Invocation.SandboxMode != tc.sandbox {
				t.Fatalf("provenance recorded sandbox %q, want %q: the planning invocation ran under %q", result.Invocation.SandboxMode, tc.sandbox, strings.Join(tc.expected, " "))
			}
		})
	}
}

// A provider with no provable non-mutating mode is INELIGIBLE. It is never run
// in its ordinary editing mode as a fallback, because a planner that can write
// is the hidden autonomous plan replacement this boundary exists to prevent.
func TestPlanningIsRefusedByAProviderWithNoProvableReadOnlyMode(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindGeminiCLI)
	_, err := provider.Execute(context.Background(), planningRequest(request))
	var unsupported *InvocationModeUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected a typed invocation-mode refusal, got %v", err)
	}
	for _, call := range fake.calls {
		if len(call.args) > 0 && call.args[len(call.args)-1] != "--help" && call.args[len(call.args)-1] != "--version" {
			t.Fatalf("a refused planning invocation still started the worker: %#v", call)
		}
	}
}

// The read-only mode is PROBED against the installed binary, exactly like every
// other flag this runtime depends on. An upstream rename makes the agent
// ineligible for planning; it never downgrades the invocation.
func TestPlanningIsRefusedWhenTheInstalledCLINoLongerAdvertisesTheMode(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	fake.help = strings.ReplaceAll(capableHelp, "read-only, ", "")
	_, err := provider.Execute(context.Background(), planningRequest(request))
	var unsupported *InvocationModeUnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "no longer advertises") {
		t.Fatalf("expected a probe-driven refusal, got %v", err)
	}
}

// Two statements that contradict each other are refused rather than resolved.
func TestPlanningAndPermissionBypassCannotBeRequestedTogether(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	provider.Agent.AllowPermissionBypass = true
	provider.PermissionBypass = true
	_, err := provider.Execute(context.Background(), planningRequest(request))
	if err == nil || !strings.Contains(err.Error(), "opposite of a non-mutating mode") {
		t.Fatalf("expected the contradiction to be refused, got %v", err)
	}
}

// A caller cannot ask for planning and get an editing invocation by leaving the
// mode unset, and cannot run producer execution in a non-mutating mode.
func TestPurposeAndModeAreBoundToEachOther(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)

	unbound := request
	unbound.Purpose = InvocationPlanning
	if _, err := provider.Execute(context.Background(), unbound); err == nil || !strings.Contains(err.Error(), "requires the") {
		t.Fatalf("planning without the non-mutating mode was accepted: %v", err)
	}

	producing := request
	producing.Mode = domain.InvocationModeNonMutatingPlanning
	if _, err := provider.Execute(context.Background(), producing); err == nil || !strings.Contains(err.Error(), "producer execution") {
		t.Fatalf("producer execution in a non-mutating mode was accepted: %v", err)
	}
}

// An AgentProfile may ask its own worker for a different model. That is a
// specialization: the provider, the credential and the trust mode are unchanged
// and only the model argument moves.
func TestProfileModelPreferenceReachesTheInvocationAndItsProvenance(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	request.ModelPreference = "opus"
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(fake.execution(t).args, " ")
	if !strings.Contains(args, "--model opus") || strings.Contains(args, "test-model") {
		t.Fatalf("argv %q did not carry the profile's model preference", args)
	}
	if result.Model != "opus" || result.Invocation.Model != "opus" {
		t.Fatalf("provenance recorded model %q/%q, want the model actually asked for", result.Model, result.Invocation.Model)
	}
}

// Operator instruction text is TRUSTED and is labelled as operator-owned, so a
// transcript reader can tell which sentence came from where. It can only arrive
// from the operator's own planning directory: nothing reads instruction content
// out of a candidate.
func TestOperatorInstructionsReachTheWorkerAsTrustedText(t *testing.T) {
	_, request, _ := agentFixture(t, AgentKindClaudeCode)
	request.Instructions = []string{"Review the diff; do not implement."}
	prompt := agentPrompt(request)
	if !strings.Contains(prompt, "Operator instructions (operator-owned configuration") {
		t.Fatalf("prompt did not label operator instructions: %s", prompt)
	}
	if !strings.Contains(prompt, "Review the diff; do not implement.") {
		t.Fatalf("prompt did not carry the operator instruction: %s", prompt)
	}
	// The runtime's own instructions still come first, and the untrusted
	// framing they establish is unchanged.
	if strings.Index(prompt, "Trusted instructions (runtime-owned") > strings.Index(prompt, "Operator instructions") {
		t.Fatal("operator instructions displaced the runtime-owned instructions")
	}
}

// The planning envelope says the opposite of the mutating one, because the
// restriction it describes is the opposite.
func TestPlanningEnvelopeTellsTheWorkerItMayChangeNothing(t *testing.T) {
	_, request, _ := agentFixture(t, AgentKindCodexCLI)
	prompt := agentPrompt(planningRequest(request))
	if !strings.Contains(prompt, "NON-MUTATING") || !strings.Contains(prompt, "make no edit") {
		t.Fatalf("planning prompt does not state the restriction: %s", prompt)
	}
	if strings.Contains(prompt, "Modify only") {
		t.Fatalf("planning prompt still tells the worker what it may modify: %s", prompt)
	}
}
