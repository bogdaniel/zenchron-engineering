//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #322: Claude Code supervised by its own structured stream-json events rather
// than by raw-output silence. The lettered sections follow the issue's test
// requirements.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// claudeHelpExcerpt is the real `claude --help` shape (2.1.282) around the
// output-format row, verbatim. `--output-format=stream-json` appears inside
// three OTHER options' descriptions before the --output-format row itself.
const claudeHelpExcerpt = `Options:
  --add-dir <directories...>            Additional directories to allow tool
                                        access to
  --allowedTools, --allowed-tools <tools...>
      Comma or space-separated list of tool names to allow (e.g. "Bash(git *)
      Edit")
  -c, --continue                        Continue the most recent conversation in
                                        the current directory
  --forward-subagent-text               Forward subagent text and thinking
                                        blocks as assistant/user messages with
                                        parent_tool_use_id set (only works with
                                        --print and --output-format=stream-json)
  --include-hook-events                 Include all hook lifecycle events in the
                                        output stream (only works with
                                        --output-format=stream-json)
  --include-partial-messages            Include partial message chunks as they
                                        arrive (only works with --print and
                                        --output-format=stream-json)
  --input-format <format>               Input format (only works with --print):
                                        "text" (default), or "stream-json"
                                        (realtime streaming input) (choices:
                                        "text", "stream-json")
  --model <model>                       Model for the current session. Provide
                                        an alias for the latest model (e.g.
                                        'fable', 'opus', or 'sonnet') or a
                                        model's full name (e.g.
                                        'claude-fable-5').
  --output-format <format>              Output format (only works with --print):
                                        "text" (default), "json" (single
                                        result), or "stream-json" (realtime
                                        streaming) (choices: "text", "json",
                                        "stream-json")
  --permission-mode <mode>              Permission mode to use for the session
                                        (choices: "acceptEdits", "auto",
                                        "bypassPermissions", "manual",
                                        "dontAsk", "plan")
  -p, --print                           Print response and exit (useful for
                                        pipes).
  --safe-mode                           Start with all customizations disabled
  --verbose                             Override verbose mode setting from
                                        config
`

// ---------------------------------------------------------------------------
// A. CLI/probe
// ---------------------------------------------------------------------------

// TestAChoiceMustBelongToTheFlagsOwnHelpRow is the probe-association
// regression: stream-json must be a choice OF --output-format, not a token
// found after `--output-format=` in some other option's prose.
func TestAChoiceMustBelongToTheFlagsOwnHelpRow(t *testing.T) {
	if !advertisesChoice(claudeHelpExcerpt, "--output-format", "stream-json") {
		t.Fatal("the real Claude help does not advertise stream-json on --output-format")
	}
	if !advertisesChoice(claudeHelpExcerpt, "--permission-mode", "plan") {
		t.Fatal("the real Claude help does not advertise plan on --permission-mode")
	}
	// THE FALSE POSITIVE. The row itself no longer offers stream-json; only
	// other options' descriptions mention it.
	withoutRow := strings.Replace(claudeHelpExcerpt,
		`or "stream-json" (realtime
                                        streaming) (choices: "text", "json",
                                        "stream-json")`, `(choices: "text", "json")`, 1)
	if withoutRow == claudeHelpExcerpt {
		t.Fatal("fixture edit did not apply, so this proves nothing")
	}
	if advertisesChoice(withoutRow, "--output-format", "stream-json") {
		t.Fatal("stream-json was accepted from another option's description")
	}
	// A short alias before the long flag still opens the row, commander and
	// clap alike.
	for _, help := range []string{
		"  -o, --output-format <format>  (choices: \"text\", \"stream-json\")\n",
		"  -s, --sandbox <SANDBOX_MODE>  [possible values: read-only, workspace-write]\n",
	} {
		flag, value := "--output-format", "stream-json"
		if strings.Contains(help, "--sandbox") {
			flag, value = "--sandbox", "read-only"
		}
		if !advertisesChoice(help, flag, value) {
			t.Fatalf("a short-alias row was refused: %q", help)
		}
	}
	// A longer flag sharing the prefix is not the flag.
	if advertisesChoice("  --output-format-extra <f>  (choices: \"stream-json\")\n", "--output-format", "stream-json") {
		t.Fatal("a longer flag was mistaken for --output-format")
	}
}

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
				want = append(want, " --permission-mode plan ")
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

// TestClaudeWithoutTheStructuredProtocolIsUnavailable: a binary that does not
// advertise stream-json on --output-format, or --verbose, is refused; it is
// never run in text mode supervised by byte silence.
func TestClaudeWithoutTheStructuredProtocolIsUnavailable(t *testing.T) {
	for name, degrade := range map[string]func(string) string{
		"no stream-json choice": func(help string) string { return strings.Replace(help, `, "stream-json")`, ")", 1) },
		"no --verbose":          func(help string) string { return strings.Replace(help, "--verbose", "--loud", 1) },
		"no --output-format":    func(help string) string { return strings.Replace(help, "--output-format", "--format", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := agentFixture(t, AgentKindClaudeCode)
			if degraded := degrade(fake.help); degraded == fake.help {
				t.Fatal("the degradation did not apply, so this proves nothing")
			} else {
				fake.help = degraded
			}
			if _, err := provider.Execute(context.Background(), request); !errors.Is(err, ErrSandboxUnavailable) {
				t.Fatalf("want a fail-closed capability refusal, got %v", err)
			}
			for _, call := range fake.calls {
				if last := call.args[len(call.args)-1]; last != "--help" && last != "--version" {
					t.Fatalf("a refused Claude still started: %v", call.args)
				}
			}
		})
	}
}
