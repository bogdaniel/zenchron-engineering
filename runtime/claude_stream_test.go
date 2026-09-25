//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #322: Claude Code supervised by its own structured stream-json events rather
// than by raw-output silence. The lettered sections follow the issue's test
// requirements.

import (
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
