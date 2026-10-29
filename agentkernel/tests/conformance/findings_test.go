package conformance

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// Review findings. Each test encodes the violated invariant and fails against
// the current implementation; it is skipped so the suite stays green until
// the owning lane repairs it. Remove the t.Skip line with the fix.

// TestFindingNegativeReportedUsageRenewsBudget: reported usage is untrusted
// wire data. A negative count settled against a reservation lowers the
// ledger's spend and so renews the budget; no adapter refuses negative counts
// (providers/openai, providers/anthropic, providers/local decode *int64 as is).
func TestFindingNegativeReportedUsageRenewsBudget(t *testing.T) {
	negative := toolUse(call("c1", "read_file", map[string]any{"path": "notes.txt"}))
	negative.Response.Usage = api.TokenUsage{Input: api.Count(-1_000_000), Output: api.Count(-1_000_000)}
	steps := []scripted.Step{negative}
	for i := range 4 {
		s := toolUse(call("c"+string(rune('2'+i)), "read_file", map[string]any{"path": "notes.txt"}))
		s.Response.Usage = usage(100, 100)
		steps = append(steps, s)
	}
	p := scripted.New(steps...)
	k := newKernel(t, config{providers: providers(p)})
	req := request("f1")
	req.Budget.MaxOutputTokens, req.Providers[0].MaxOutputTokens = 100, 100
	req.Budget.MaxIterations, req.Budget.MaxToolCalls = 10, 10
	k.run(t, context.Background(), req)
	// Turn 1 reserved the whole 100-token output budget and its usage is not
	// a usable count, so no second turn may be granted output tokens.
	if n := len(p.Requests()); n > 1 {
		t.Fatalf("%d provider calls granted after a negative usage report on a 100-token output budget", n)
	}
}

type fakeRunner struct{ ran int }

func (r *fakeRunner) Run(context.Context, api.CommandRequest) (api.CommandResult, error) {
	r.ran++
	return api.CommandResult{ExitCode: 0, Stdout: []byte("migrated 3 rows\n")}, nil
}

// TestFindingCommandRanButResultHidesIt: the artifact budget is checked only
// after the host command has run. When the output cannot be recorded the
// execution settles exhausted/artifact_bytes, the tool.executed observation
// is never emitted, and nothing in the result says a possibly mutating
// command ran: the host cannot tell an uncertain mutation from no mutation.
func TestFindingCommandRanButResultHidesIt(t *testing.T) {
	runner := &fakeRunner{}
	cmd, err := tools.NewCommand(api.ServeCommands(t.Context(), runner), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := scripted.New(toolUse(call("c1", "run_command", map[string]any{"command": "migrate"})), end("done"))
	k := newKernel(t, config{providers: providers(p), extra: []tools.Tool{cmd}})
	req := request("f2")
	req.Grants = append(req.Grants, api.Capability{Handle: "cmds", Kind: api.CapabilityCommand,
		Commands: []api.CommandGrant{{Name: "migrate", Argv: []string{"migrate"}, TimeoutSeconds: 5}}})
	req.Budget.MaxArtifactBytes = 1
	res := k.run(t, context.Background(), req)
	if runner.ran != 1 {
		t.Fatalf("runner ran %d times", runner.ran)
	}
	executed := false
	for _, o := range res.Observations {
		if o.Kind == api.EventToolExecuted && o.ToolCall == "c1" {
			executed = true
		}
	}
	if !executed && res.Termination.Cause == api.CauseBudgetExhausted {
		t.Fatalf("command ran but the result records only %+v and no tool.executed observation", res.Termination)
	}
}

// TestFindingUnknownStopSettlesCompleted: a provider stop the kernel does not
// understand (StopOther: Anthropic pause_turn/stop_sequence, OpenAI
// incomplete for an unlisted reason) or a tool_use stop with no tool calls
// settles completed/loop_completed, collapsing an unknown provider state into
// the success-shaped outcome.
func TestFindingUnknownStopSettlesCompleted(t *testing.T) {
	for name, stop := range map[string]api.StopReason{"other": api.StopOther, "tool_use without calls": api.StopToolUse} {
		t.Run(name, func(t *testing.T) {
			p := scripted.New(scripted.Step{Response: api.ProviderResponse{Text: "partial", Stop: stop, Usage: usage(10, 10)}})
			k := newKernel(t, config{providers: providers(p)})
			res := k.run(t, context.Background(), request("f3"))
			if res.Termination.Outcome == api.OutcomeCompleted {
				t.Fatalf("stop %q settled %+v", stop, res.Termination)
			}
		})
	}
}
