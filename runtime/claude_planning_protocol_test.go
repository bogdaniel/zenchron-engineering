package runtime

import (
	"context"
	"strings"
	"testing"
)

// TestClaudeRunsTheStructuredProtocolInBothModes: --print with stream-json and
// --verbose on the editing AND the plan invocation, no partial-token streaming,
// no --bare, and every existing constraint still passed.
func TestClaudeRunsTheStructuredProtocolInBothModes(t *testing.T) {
	for name, plan := range map[string]bool{"mutating": false, "plan": true} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := agentFixture(t, AgentKindClaudeCode)
			request.RequiredTools = []string{"go"}
			request.ScratchDir = t.TempDir()
			if plan {
				request = planningRequest(request)
			}
			if _, err := provider.Execute(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			args := " " + strings.Join(fake.execution(t).args, " ") + " "
			want := []string{" --print ", " --output-format stream-json ", " --verbose ", " --safe-mode ", " --model test-model "}
			if plan {
				want = append(want, " --permission-mode plan ", " --disallowedTools EnterPlanMode ExitPlanMode ", "Headless planning: return the requested fenced JSON object inline")
			} else {
				want = append(want, " --permission-mode acceptEdits ", " --add-dir ", " --allowedTools Bash(go *) ")
			}
			for _, flag := range want {
				if !strings.Contains(args, flag) {
					t.Errorf("argv lacks %q: %s", flag, args)
				}
			}
			for _, forbidden := range []string{"--include-partial-messages", "--bare"} {
				if strings.Contains(args, forbidden) {
					t.Errorf("argv carries %q: %s", forbidden, args)
				}
			}
		})
	}
}
