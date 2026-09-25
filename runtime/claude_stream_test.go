//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #322: Claude Code supervised by its own structured stream-json events rather
// than by raw-output silence. The lettered sections follow the issue's test
// requirements.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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

// ---------------------------------------------------------------------------
// Fixtures: Claude's real stream-json shapes, top-level messages carrying
// content blocks.
// ---------------------------------------------------------------------------

const (
	claudeInit    = `{"type":"system","subtype":"init","session_id":"s","model":"m","tools":["Bash"]}`
	claudeUnknown = `{"type":"future_event","payload":{"x":1}}`
	claudeText    = `{"type":"text","text":"working on it"}`
)

func claudeToolUse(id string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"go test ./..."}}`
}

// claudeAssistant is one assistant line; parent "" is the main thread.
func claudeAssistant(messageID, parent string, blocks ...string) string {
	return `{"type":"assistant","message":{"id":"` + messageID + `","role":"assistant","content":[` +
		strings.Join(blocks, ",") + `]},"parent_tool_use_id":` + claudeParent(parent) + `,"session_id":"s"}`
}

func claudeToolResult(toolID, parent string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID +
		`","content":"ok"}]},"parent_tool_use_id":` + claudeParent(parent) + `,"session_id":"s"}`
}

func claudeParent(parent string) string {
	if parent == "" {
		return "null"
	}
	return `"` + parent + `"`
}

// claudeRetry is a system/api_retry line; status is JSON ("429" or "null").
func claudeRetry(category, status string, noResponse bool) string {
	line := `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":` + status +
		`,"error":"` + category + `","uuid":"u","session_id":"s"`
	if noResponse {
		line += `,"no_response":{"waited_ms":60000,"retry_wait_ms":60000}`
	}
	return line + "}"
}

func claudeResult(isError bool, subtype string, denials int) string {
	d := make([]string, denials)
	for i := range d {
		d[i] = `{"tool_name":"Bash","tool_use_id":"t","tool_input":{}}`
	}
	return fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":%t,"result":"rate_limit authentication_failed overloaded","permission_denials":[%s]}`,
		subtype, isError, strings.Join(d, ","))
}

// feed writes lines to a stream in small arbitrary chunks, which is how the
// os/exec copy goroutine delivers them.
func feed(stream *claudeStream, lines ...string) {
	data := []byte(strings.Join(lines, "\n") + "\n")
	for len(data) > 0 {
		n := min(7, len(data))
		_, _ = stream.Write(data[:n])
		data = data[n:]
	}
}

// emit is a shell fragment printing each line verbatim.
func emit(lines ...string) string {
	quoted := make([]string, len(lines))
	for i, line := range lines {
		quoted[i] = "'" + line + "'"
	}
	return "printf '%s\\n' " + strings.Join(quoted, " ") + "\n"
}

// claudeProcess is one Claude agent whose invocation is a REAL bounded
// process running script, through the delegating inactivityCLI double.
func claudeProcess(t *testing.T, script string) (CLIAgentProvider, ExecutionRequest) {
	t.Helper()
	requireBoundedProcess(t)
	provider, request, _ := agentFixture(t, AgentKindClaudeCode)
	provider.Executor = &inactivityCLI{script: script}
	provider.Grace = 150 * time.Millisecond
	request.Budgets = ProviderBudget{WallLimit: time.Hour, InactivityLimit: inactivityWindow}
	return provider, request
}

// ---------------------------------------------------------------------------
// B. Parser
// ---------------------------------------------------------------------------

func TestClaudeStreamAcceptsOnlyAgentLoopEventsAsProgress(t *testing.T) {
	previous := maxClaudeEventBytes
	maxClaudeEventBytes = 512
	defer func() { maxClaudeEventBytes = previous }()

	stream := newClaudeStream(1)
	feed(stream,
		claudeInit,
		claudeAssistant("m1", "", claudeText),
		claudeAssistant("m1", "", claudeToolUse("X")),
		claudeToolResult("X", ""),
		claudeRetry("rate_limit", "429", false),
		claudeUnknown,
		`{"type":"assistant","message":{`,                           // malformed
		`{"type":"user","padding":"`+strings.Repeat("x", 2000)+`"}`, // oversized
		claudeAssistant("m2", "", claudeText),
		claudeResult(false, "success", 0),
	)
	got := stream.outcome(true)
	// assistant text, assistant tool_use, user tool_result, and the assistant
	// AFTER the anomalies: the parser recovered.
	if got.Accepted != 4 {
		t.Fatalf("accepted %d events, want 4: init, retry, unknown and result must not count", got.Accepted)
	}
	if got.Anomalies != 2 {
		t.Fatalf("anomalies = %d, want the malformed and the oversized line", got.Anomalies)
	}
	if got.Failed || got.OpenTools != 0 {
		t.Fatalf("outcome = %+v, want a clean success with nothing open", got)
	}
	// The retry was followed by accepted progress, so it is resolved.
	if got.Condition != FailureUnknown {
		t.Fatalf("a resolved retry still states %q", got.Condition)
	}
	// Unknown and system events alone authorize nothing.
	quiet := newClaudeStream(1)
	feed(quiet, claudeInit, claudeUnknown, claudeRetry("overloaded", "529", false), claudeResult(false, "success", 0))
	if quiet.outcome(true).Accepted != 0 {
		t.Fatal("non-progress events were counted as progress")
	}
}

// The main-thread open-tool set converges under every shape #322 names.
func TestClaudeOpenToolBookkeeping(t *testing.T) {
	open := func(lines ...string) int {
		stream := newClaudeStream(1)
		feed(stream, lines...)
		return stream.outcome(false).OpenTools
	}
	cases := []struct {
		name  string
		lines []string
		want  int
	}{
		{"one open tool", []string{claudeAssistant("M1", "", claudeToolUse("X"))}, 1},
		{"parallel tools in one turn stay open", []string{
			claudeAssistant("M1", "", claudeToolUse("X")), claudeAssistant("M1", "", claudeToolUse("Y"))}, 2},
		{"a new turn closes the previous turn's unresolved ids", []string{
			claudeAssistant("M1", "", claudeToolUse("X")), claudeAssistant("M1", "", claudeToolUse("Y")),
			`{"type":"user","message":{"content":[{"type":"tool_result"`, // the unparseable result line
			claudeAssistant("M2", "", claudeText)}, 0},
		{"a new turn applies its own tool after closing", []string{
			claudeAssistant("M1", "", claudeToolUse("X")), claudeAssistant("M2", "", claudeToolUse("Z"))}, 1},
		{"duplicate opens are idempotent", []string{
			claudeAssistant("M1", "", claudeToolUse("X")), claudeAssistant("M1", "", claudeToolUse("X")), claudeToolResult("X", "")}, 0},
		{"an unknown result creates nothing", []string{claudeToolResult("nobody", "")}, 0},
		{"a duplicate or late result is idempotent", []string{
			claudeAssistant("M1", "", claudeToolUse("X")), claudeToolResult("X", ""), claudeToolResult("X", "")}, 0},
		{"nested messages never open", []string{claudeAssistant("S1", "AGENT", claudeToolUse("N"))}, 0},
		{"nested results never close the main thread", []string{
			claudeAssistant("M1", "", claudeToolUse("AGENT")), claudeToolResult("AGENT", "AGENT")}, 1},
		{"a foreground subagent is held by its parent Agent call", []string{
			claudeAssistant("M1", "", claudeToolUse("AGENT")), claudeAssistant("S1", "AGENT", claudeToolUse("N")),
			claudeToolResult("N", "AGENT")}, 1},
		{"background subagent messages after the parent result hold nothing", []string{
			claudeAssistant("M1", "", claudeToolUse("AGENT")), claudeToolResult("AGENT", ""),
			claudeAssistant("S1", "AGENT", claudeToolUse("N")), claudeAssistant("S1", "AGENT", claudeText)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := open(tc.lines...); got != tc.want {
				t.Fatalf("open tools = %d, want %d", got, tc.want)
			}
		})
	}
	// Nested messages still count as progress.
	nested := newClaudeStream(1)
	feed(nested, claudeAssistant("S1", "AGENT", claudeText), claudeToolResult("N", "AGENT"))
	if nested.outcome(false).Accepted != 2 {
		t.Fatal("nested subagent activity did not refresh progress")
	}
}

// An executor that does not feed the stream is refused before Claude runs.
func TestStructuredClaudeRefusesAnExecutorThatCannotObserveStdout(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	provider.Executor = struct{ CommandExecutor }{fake} // the marker is not promoted
	if _, err := provider.Execute(context.Background(), request); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("want a refusal before invocation, got %v", err)
	}
	for _, call := range fake.calls {
		if last := call.args[len(call.args)-1]; last != "--help" && last != "--version" {
			t.Fatalf("Claude ran under an executor that cannot observe it: %v", call.args)
		}
	}
	if provider.Probe(context.Background()).Available {
		t.Fatal("readiness reported the non-observing executor as available")
	}
}

// ---------------------------------------------------------------------------
// C. #238 preserved: a silent Claude is still bounded
// ---------------------------------------------------------------------------

func TestASilentClaudeIsStillTerminatedAtTheInactivityBound(t *testing.T) {
	dir := t.TempDir()
	leader, child := dir+"/provider.pid", dir+"/descendant.pid"
	provider, request := claudeProcess(t, silentProviderScript(leader, child))

	started := time.Now()
	result, err := provider.Execute(context.Background(), request)
	elapsed := time.Since(started)
	if err == nil || result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("zero structured lines ended as %#v (%v), want %q - not a protocol failure", result.Failure, err, FailureProviderNoProgress)
	}
	if elapsed < inactivityWindow || elapsed > 10*time.Second {
		t.Fatalf("elapsed %s: the inactivity bound, not the hour of wall budget, must end it", elapsed)
	}
	if result.Invocation.TerminationCause != "provider_inactivity_limit_reached" ||
		result.Invocation.ProgressMode != progressStructuredClaudeEvents || result.Invocation.StructuredEvents != 0 {
		t.Fatalf("provenance = %+v", result.Invocation)
	}
	assertProcessGroupIsGone(t, leader, child)
}

// ---------------------------------------------------------------------------
// D. Structured progress refreshes the window; noisy stderr does not
// ---------------------------------------------------------------------------

func TestStructuredProgressRefreshesTheWindowAndStderrDoesNot(t *testing.T) {
	t.Run("assistant events", func(t *testing.T) {
		provider, request := claudeProcess(t,
			"i=0\nwhile [ $i -lt 20 ]; do "+strings.TrimSuffix(emit(claudeAssistant("m1", "", claudeText)), "\n")+
				"; sleep 0.05; i=$((i+1)); done\n"+emit(claudeResult(false, "success", 0)))
		started := time.Now()
		result, err := provider.Execute(context.Background(), request)
		elapsed := time.Since(started)
		if err != nil || result.Outcome != Succeeded {
			t.Fatalf("a Claude emitting progress was failed: %v %#v", err, result.Failure)
		}
		if elapsed <= inactivityWindow {
			t.Fatalf("finished in %s, inside the window: the refresh was never exercised", elapsed)
		}
		if result.Invocation.StructuredEvents != 20 {
			t.Fatalf("structured events = %d, want 20", result.Invocation.StructuredEvents)
		}
		if result.Invocation.Deadline == nil || result.Invocation.Deadline.Before(started.Add(59*time.Minute)) {
			t.Fatalf("the absolute deadline moved: %v", result.Invocation.Deadline)
		}
	})
	t.Run("noisy stderr and raw stdout", func(t *testing.T) {
		provider, request := claudeProcess(t, "while :; do echo warning >&2; echo not-json; sleep 0.02; done\n")
		// A near deadline so a regression fails fast instead of hanging.
		request.Budgets.WallLimit = 5 * time.Second
		started := time.Now()
		result, _ := provider.Execute(context.Background(), request)
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("noise kept Claude alive for %s", elapsed)
		}
		if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
			t.Fatalf("failure = %#v, want %q: bytes are not Claude progress", result.Failure, FailureProviderNoProgress)
		}
		if result.Invocation.Deadline == nil || result.Invocation.Deadline.Before(started.Add(4*time.Second)) {
			t.Fatalf("the absolute deadline moved: %v", result.Invocation.Deadline)
		}
	})
}

// ---------------------------------------------------------------------------
// E. api_retry is not progress, and its precedence expires on progress
// ---------------------------------------------------------------------------

func TestApiRetryIsNotProgressAndStalePrecedenceExpires(t *testing.T) {
	t.Run("retry chatter only", func(t *testing.T) {
		provider, request := claudeProcess(t,
			"while :; do "+strings.TrimSuffix(emit(claudeRetry("rate_limit", "429", false)), "\n")+"; sleep 0.05; done\n")
		started := time.Now()
		result, _ := provider.Execute(context.Background(), request)
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("retry chatter held Claude for %s", elapsed)
		}
		if result.Invocation.TerminationCause != "provider_inactivity_limit_reached" {
			t.Fatalf("termination = %q, want the inactivity bound", result.Invocation.TerminationCause)
		}
		// Condition and termination cause are separate facts.
		if result.Failure == nil || result.Failure.Classification != FailureProviderRateLimited {
			t.Fatalf("failure = %#v, want the unresolved typed retry condition", result.Failure)
		}
	})
	t.Run("retry resolved by later progress", func(t *testing.T) {
		provider, request := claudeProcess(t,
			emit(claudeRetry("rate_limit", "429", false), claudeAssistant("m1", "", claudeText))+"while :; do sleep 30; done\n")
		result, _ := provider.Execute(context.Background(), request)
		if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
			t.Fatalf("failure = %#v, want %q: a resolved retry must not classify a later stall", result.Failure, FailureProviderNoProgress)
		}
		if result.Invocation.StructuredEvents != 1 {
			t.Fatalf("structured events = %d, want 1", result.Invocation.StructuredEvents)
		}
	})
}

// ---------------------------------------------------------------------------
// F/G. An open tool suspends the inactivity kill, not the deadline
// ---------------------------------------------------------------------------

func TestAnOpenToolSuspendsInactivityUntilItsResult(t *testing.T) {
	const toolRuns = 900 * time.Millisecond
	provider, request := claudeProcess(t,
		emit(claudeAssistant("M1", "", claudeToolUse("X")))+"sleep 0.9\n"+
			emit(claudeToolResult("X", ""))+"while :; do sleep 30; done\n")
	started := time.Now()
	result, _ := provider.Execute(context.Background(), request)
	elapsed := time.Since(started)
	// Not killed while X was open (0.9s > 2x the window), and killed by the
	// window once the result resumed normal timing.
	if elapsed < toolRuns+inactivityWindow || elapsed > 10*time.Second {
		t.Fatalf("elapsed %s: want at least the tool's %s plus one %s window", elapsed, toolRuns, inactivityWindow)
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure = %#v, want inactivity after the tool closed", result.Failure)
	}
	if result.Invocation.OpenToolsAtExit != 0 || result.Invocation.StructuredEvents != 2 {
		t.Fatalf("provenance = %+v", result.Invocation)
	}
}

// TestAHungToolIsBoundedByTheAbsoluteDeadline documents the PR1 trade-off.
func TestAHungToolIsBoundedByTheAbsoluteDeadline(t *testing.T) {
	provider, request := claudeProcess(t, emit(claudeAssistant("M1", "", claudeToolUse("X")))+"while :; do sleep 30; done\n")
	request.Budgets.WallLimit = 1500 * time.Millisecond
	started := time.Now()
	result, _ := provider.Execute(context.Background(), request)
	if elapsed := time.Since(started); elapsed < request.Budgets.WallLimit {
		t.Fatalf("killed after %s, before the %s deadline: the open tool did not suspend inactivity", elapsed, request.Budgets.WallLimit)
	}
	if result.Invocation.TerminationCause != "deadline_reached" || result.Invocation.OpenToolsAtExit != 1 {
		t.Fatalf("provenance = %+v, want the deadline with the tool still open", result.Invocation)
	}
	if result.Failure == nil || result.Failure.Classification != FailureExecutionIncomplete {
		t.Fatalf("failure = %#v, want the existing deadline semantics", result.Failure)
	}
}

// ---------------------------------------------------------------------------
// H. The final result after the capture ceiling
// ---------------------------------------------------------------------------

func TestTheFinalResultIsSeenPastTheCaptureCeiling(t *testing.T) {
	previous := maxCapturedProcessBytes
	maxCapturedProcessBytes = 2048
	defer func() { maxCapturedProcessBytes = previous }()
	provider, request := claudeProcess(t,
		"i=0\nwhile [ $i -lt 100 ]; do "+strings.TrimSuffix(emit(claudeAssistant("m", "", claudeText)), "\n")+
			"; i=$((i+1)); done\n"+emit(claudeResult(false, "success", 0)))
	result, err := provider.Execute(context.Background(), request)
	if err != nil || result.Outcome != Succeeded {
		t.Fatalf("the result past the ceiling was not seen: %v %#v", err, result.Failure)
	}
	transcript, readErr := os.ReadFile(result.Artifacts[0].Path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(transcript), "[truncated by Zenchron") || strings.Contains(string(transcript), `"type":"result"`) {
		t.Fatal("the transcript was not truncated before the result, so this proves nothing")
	}
	if result.Invocation.StructuredEvents != 100 {
		t.Fatalf("structured events = %d, want 100", result.Invocation.StructuredEvents)
	}
}

// ---------------------------------------------------------------------------
// I. Typed failure classification
// ---------------------------------------------------------------------------

func TestClaudeTypedFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		line string
		want FailureClass
	}{
		{claudeRetry("authentication_failed", "401", false), FailureProviderAccountUnavailable},
		{claudeRetry("billing_error", "402", false), FailureProviderAccountUnavailable},
		{claudeRetry("account_on_hold", "403", false), FailureProviderAccountUnavailable},
		{claudeRetry("rate_limit", "429", false), FailureProviderRateLimited},
		{claudeRetry("overloaded", "529", false), FailureProviderUnavailable},
		{claudeRetry("server_error", "503", false), FailureProviderUnavailable},
		{claudeRetry("server_error", "500", false), FailureUnknown},
		{claudeRetry("unknown", "null", true), FailureProviderUnavailable},
		{claudeRetry("unknown", "null", false), FailureUnknown},
		{claudeRetry("a_future_category", "418", false), FailureUnknown},
	} {
		stream := newClaudeStream(1)
		feed(stream, tc.line)
		if got := stream.outcome(false).Condition; got != tc.want {
			t.Errorf("%s classified %q, want %q", tc.line, got, tc.want)
		}
	}
	// PROSE CANNOT CREATE A CONDITION: model text and result text naming every
	// category, with no typed retry event.
	prose := newClaudeStream(1)
	feed(prose, claudeAssistant("m", "", `{"type":"text","text":"rate_limit 429 authentication_failed overloaded no_response"}`),
		claudeResult(true, "error_during_execution", 0))
	got := prose.outcome(true)
	if got.Condition != FailureUnknown || !got.Failed {
		t.Fatalf("prose outcome = %+v, want a fail-closed unknown failure", got)
	}
}

// Exit 0 is not success by itself; permission denials are a count only.
func TestAZeroExitStillFailsOnAnErrorResultOrNoResult(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout  string
		failed  bool
		denials int
	}{
		"is_error result":               {claudeResult(true, "error_during_execution", 0), true, 0},
		"no final result":               {claudeAssistant("m", "", claudeText), true, 0},
		"success with denials":          {claudeResult(false, "success", 3), false, 3},
		"error with an unresolved auth": {claudeRetry("authentication_failed", "401", false) + "\n" + claudeResult(true, "success", 0), true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := agentFixture(t, AgentKindClaudeCode)
			fake.outputs = []CommandOutput{{Stdout: []byte(tc.stdout + "\n")}}
			result, err := provider.Execute(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if tc.failed != (result.Outcome == OperationFailed) {
				t.Fatalf("outcome = %q, want failed=%v", result.Outcome, tc.failed)
			}
			if result.Invocation.PermissionDenials != tc.denials {
				t.Fatalf("permission denials = %d, want %d", result.Invocation.PermissionDenials, tc.denials)
			}
			if !tc.failed {
				if result.Failure != nil {
					t.Fatalf("denials created a failure class: %#v", result.Failure)
				}
				return
			}
			want := FailureUnknown
			if strings.Contains(name, "auth") {
				want = FailureProviderAccountUnavailable
			}
			if result.Failure == nil || result.Failure.Classification != want {
				t.Fatalf("failure = %#v, want %q", result.Failure, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A (continued). The invocation-only background-wait ceiling
// ---------------------------------------------------------------------------

func TestClaudeBackgroundWaitCeilingSitsInsideTheWindow(t *testing.T) {
	envOf := func(call recordedCommand) string {
		for _, entry := range call.env {
			if strings.HasPrefix(entry, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=") {
				return entry
			}
		}
		return ""
	}
	for window, want := range map[time.Duration]string{
		10 * time.Minute: "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=450000",
		time.Minute:      "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=45000", // tightening tightens it
		0:                "",                                           // no bound, Claude's own default
	} {
		provider, request, fake := agentFixture(t, AgentKindClaudeCode)
		request.Budgets.InactivityLimit = window
		if _, err := provider.Execute(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if got := envOf(fake.execution(t)); got != want {
			t.Fatalf("window %s: env %q, want %q", window, got, want)
		}
		for _, call := range fake.calls {
			if last := call.args[len(call.args)-1]; (last == "--help" || last == "--version") && envOf(call) != "" {
				t.Fatalf("a probe inherited the invocation-only env: %v", call.args)
			}
		}
	}
	// A window with no positive millisecond below it refuses, never emits 0.
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	request.Budgets.InactivityLimit = time.Millisecond
	if _, err := provider.Execute(context.Background(), request); err == nil {
		t.Fatal("an impossible window dispatched Claude")
	}
	for _, call := range fake.calls {
		if last := call.args[len(call.args)-1]; last != "--help" && last != "--version" {
			t.Fatalf("Claude ran with an underivable ceiling: %v", call.args)
		}
	}
	// Other providers get no invocation env at all.
	codex, codexRequest, codexFake := agentFixture(t, AgentKindCodexCLI)
	codexRequest.Budgets.InactivityLimit = 10 * time.Minute
	if _, err := codex.Execute(context.Background(), codexRequest); err != nil {
		t.Fatal(err)
	}
	if envOf(codexFake.execution(t)) != "" {
		t.Fatal("codex received Claude's invocation env")
	}
}

func TestTheInvocationEnvHookCannotCarryCredentialsOrReplaceTheAllowlist(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/op"}
	for _, entry := range []string{"ANTHROPIC_API_KEY=x", "CLAUDE_CODE_OAUTH_TOKEN=x", "GH_TOKEN=x", "PATH=/evil", "HOME=/elsewhere"} {
		if _, err := withInvocationEnv(base, []string{entry}); err == nil {
			t.Errorf("%s was accepted", entry)
		}
	}
	got, err := withInvocationEnv(base, []string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=1"})
	if err != nil || len(got) != 3 {
		t.Fatalf("a non-secret control was refused: %v %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// J. Restart / re-adoption
// ---------------------------------------------------------------------------

func TestAStructuredClaudeAttemptGetsItsOwnWindowWithoutReplenishingAuthority(t *testing.T) {
	const window = 10 * time.Minute
	claude := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindClaudeCode}}
	codex := CLIAgentProvider{Agent: ResolvedAgent{Kind: AgentKindCodexCLI}}
	scheduler, clock := deadlineScheduler(t)
	planned, _, err := scheduler.Plan(RunOperation{
		RunID: "run-claude", Kind: OpExecutionInvoke, IdempotencyKey: "invoke-claude",
		MaxAttempts: 8, WallBudget: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(planned.RunID); err != nil {
		t.Fatal(err)
	}
	op, err := scheduler.Start(planned.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Structured activity advances durable progress, qualified by attempt.
	recorded, err := scheduler.RecordProviderProgress(op.ID, fmt.Sprintf("%d:%d", op.AttemptIdentity, 1))
	if err != nil || recorded.LastProgressAt == nil || !recorded.LastProgressAt.Equal(clock.Now()) {
		t.Fatalf("structured progress did not advance durably: %+v %v", recorded, err)
	}
	consumed, remaining, identity := op.ConsumedExecution, OperationRemaining(op, clock.Now()), op.AttemptIdentity
	for cycle := 1; cycle <= 4; cycle++ {
		// Eleven silent minutes: longer than the whole window.
		clock.advance(11 * time.Minute)
		abandonExecution(t, scheduler, op.ID)
		if op, err = scheduler.Start(op.ID); err != nil {
			t.Fatal(err)
		}
		now := clock.Now()
		// A FRESH physical Claude attempt is not refused over its dead
		// predecessor's stale progress; codex keeps today's semantics.
		if got := dispatchInactivityWindow(window, op, now, claude); got != window {
			t.Fatalf("cycle %d: Claude's fresh attempt got %s, want its full %s window", cycle, got, window)
		}
		if got := dispatchInactivityWindow(window, op, now, codex); got != 0 {
			t.Fatalf("cycle %d: codex's abandoned silence was forgiven: %s", cycle, got)
		}
		// Monotonic facts across succession.
		if op.ConsumedExecution < consumed || OperationRemaining(op, now) > remaining || op.AttemptIdentity <= identity {
			t.Fatalf("cycle %d: consumed %s (was %s), remaining %s (was %s), identity %d (was %d)",
				cycle, op.ConsumedExecution, consumed, OperationRemaining(op, now), remaining, op.AttemptIdentity, identity)
		}
		consumed, remaining, identity = op.ConsumedExecution, OperationRemaining(op, now), op.AttemptIdentity
	}
	// Succession converges: 44 of 60 minutes are charged, not refunded.
	if consumed < 44*time.Minute || remaining > 16*time.Minute {
		t.Fatalf("after four abandoned attempts consumed %s, remaining %s", consumed, remaining)
	}
}

// ---------------------------------------------------------------------------
// Configuration: the named minimum, at load and in doctor
// ---------------------------------------------------------------------------

func TestTheProviderInactivityMinimumIsRefusedBeforeAnyRun(t *testing.T) {
	operator := OperatorConfig{
		StateDir: "/state", ProjectModelPath: "/m.json", PolicyPath: "/p.json",
		Assurance: AssuranceConfig{Image: "sha256:" + strings.Repeat("a", 64)},
		Agents:    map[string]AgentConfig{"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)}},
		GitHub:    GitHubConfig{CredentialMode: GitHubCredentialNone},
		Budgets: BudgetConfig{
			WallLimitSeconds: 1800, MaxExecutionAttempts: 2, MaxRemediationAttempts: 2, MaxAssuranceAttempts: 2,
			ProviderInactivitySeconds: MinProviderInactivitySeconds - 1,
		},
	}
	if err := operator.validate("/config.json"); err == nil || !strings.Contains(err.Error(), "minimum 10") {
		t.Fatalf("a window below the minimum loaded: %v", err)
	}
	operator.Budgets.ProviderInactivitySeconds = MinProviderInactivitySeconds
	if err := operator.validate("/config.json"); err != nil {
		t.Fatalf("the minimum itself was refused: %v", err)
	}
	tiny := MinProviderInactivitySeconds - 1
	if _, err := operator.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{ProviderInactivitySeconds: &tiny}}); err == nil {
		t.Fatal("a repository tightened below the minimum")
	}
	// Every accepted window derives a positive ceiling strictly inside it.
	if env, err := claudeBackgroundWaitEnv(MinProviderInactivitySeconds * time.Second); err != nil || len(env) != 1 {
		t.Fatalf("the minimum window did not derive a ceiling: %v %v", env, err)
	}

	f := newDoctorFixture(t)
	f.writeOperatorConfig(func(config map[string]any) {
		config["budgets"].(map[string]any)["provider_inactivity_seconds"] = 5
	})
	requireCheck(t, f.run(), "config.global", DoctorFail, "is 5, below the minimum 10")
}
