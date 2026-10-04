package runtime

// Platform-neutral Claude stream-json fixtures, shared by the Unix-only
// process tests and by untagged tests that only need the wire shapes.

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

// claudeBackgroundToolUse is a main-thread Bash call dispatched with the Bash
// tool's own typed run_in_background input (#384).
func claudeBackgroundToolUse(id string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"go test ./...","run_in_background":true}}`
}

// claudePollToolUse and claudeKillShellToolUse are main-thread calls to
// Claude's own background-shell follow-up tools (#384). Neither ever
// resolves outstanding background work (#385: see
// claudeStream.backgroundStarts for why) - a poll proves only that Claude
// looked, and a KillShell call proves only that Claude asked, never that the
// named identity corresponds to a shell this invocation actually started -
// so claudeKillShellToolUse still takes a shellID parameter only to exercise
// that it is deliberately ignored for resolution.
func claudePollToolUse(id string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"BashOutput","input":{"bash_id":"bash_1"}}`
}

func claudeKillShellToolUse(id, shellID string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"KillShell","input":{"shell_id":"` + shellID + `"}}`
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

// claudeResultWithAnswer is a result line carrying a caller-chosen semantic
// answer, JSON-escaped by the real encoder rather than assembled by hand - so
// the escaping under test (#366) is the genuine transport encoding, not a
// hand-built approximation of it.
func claudeResultWithAnswer(isError bool, answer string) string {
	encoded, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":%t,"result":%s}`, isError, encoded)
}

func claudeAutomaticResult(id, parent, metadata string) string {
	return strings.TrimSuffix(claudeToolResult(id, parent), "}") + `,"tool_use_result":` + metadata + `}`
}
