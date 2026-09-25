package runtime

// Claude Code's structured headless protocol, as the progress oracle for its
// inactivity bound (#322).
//
// In text-mode --print Claude writes nothing until its final answer, so a
// productive multi-turn session was indistinguishable from a dead provider and
// was killed at the #238 inactivity bound again and again (#303, #319). With
// --output-format stream-json it emits its agent loop as newline-delimited JSON
// events, and this file reads them WHILE the process runs.
//
// The bound itself is unchanged and stays finite. What changes for Claude is
// what refreshes it:
//
//   - an `assistant` message carrying a content block, or a `user` message
//     carrying a `tool_result`: progress.
//   - raw stdout or stderr bytes: NOT progress. A process printing warnings is
//     noisy, not working.
//   - `system/*` (including api_retry), `result`, unknown events: NOT
//     progress. "I am retrying transport" is not "the work advanced".
//
// A main-thread tool call that is still open suspends the inactivity kill -
// `go test` may legitimately run longer than the window - but never the
// absolute deadline, which still bounds a tool that hangs forever.
//
// Everything here is bounded, and nothing parsed from the stream is copied into
// durable state except counts and typed categories. Model prose, thinking and
// tool output are never read into a field.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// The progress oracles an adapter's inactivity bound can be supervised by.
// Recorded in InvocationProvenance so incident review can tell which one
// measured a stalled attempt.
const (
	progressByteOutput             = "byte_output"
	progressStructuredClaudeEvents = "structured_claude_events"
)

// maxClaudeEventBytes bounds one NDJSON line. A longer line - a huge tool
// result, say - is a protocol anomaly: it does not refresh the bound, it does
// not kill the invocation, and it is never buffered past this size. It is a var
// only so a test can drive the real boundary with a small line.
var maxClaudeEventBytes = 4 << 20

// maxClaudeOpenTools bounds the main-thread open-tool set.
// ponytail: opens past the cap are not tracked, so an absurdly parallel turn
// could lose its suspension; raise the cap if live evidence ever shows one.
const maxClaudeOpenTools = 256

// claudeStream is the incremental parser for one Claude invocation's stdout,
// and the state the inactivity watch consults.
//
// It is written to on the os/exec copy goroutine, so Write never blocks on
// anything slower than its own mutex; the durable progress write happens after
// that mutex is released and is throttled by the watch.
type claudeStream struct {
	attempt int

	mu    sync.Mutex
	watch *inactivityWatch
	// line holds the current partial line. discarding means the line in
	// progress already exceeded maxClaudeEventBytes and is being skipped to its
	// newline.
	line       []byte
	discarding bool

	// seq numbers every valid event. The last accepted progress and the latest
	// typed retry condition are kept as separate positions, so a retry that
	// later progress resolved cannot outrank an unrelated stall.
	seq, progressSeq, retrySeq int64
	retry                      FailureClass

	accepted  int64
	anomalies int
	// open is the MAIN-THREAD tool_use ids awaiting a result, and turn the
	// main-thread assistant message.id they were opened in.
	open map[string]struct{}
	turn string

	sawResult bool
	isError   bool
	subtype   string
	denials   int
}

func newClaudeStream(attempt int) *claudeStream {
	return &claudeStream{attempt: attempt, open: map[string]struct{}{}}
}

type claudeStreamKey struct{}

// withClaudeStream carries the parser to the process runner beside the
// inactivity policy, without widening the shared CommandExecutor interface.
func withClaudeStream(ctx context.Context, stream *claudeStream) context.Context {
	return context.WithValue(ctx, claudeStreamKey{}, stream)
}

func claudeStreamFrom(ctx context.Context) *claudeStream {
	stream, _ := ctx.Value(claudeStreamKey{}).(*claudeStream)
	return stream
}

// stdoutObservingExecutor is the capability an executor declares when its Run
// tees stdout into the context-carried claudeStream. Structured Claude refuses
// an executor without it BEFORE running anything: one that ignored the stream
// would run Claude, observe zero events, and misreport a working provider as
// provider_no_progress.
type stdoutObservingExecutor interface{ observesStdout() }

// attach binds the watch this stream refreshes. Either may be nil.
func (s *claudeStream) attach(watch *inactivityWatch) {
	if s == nil || watch == nil {
		return
	}
	s.mu.Lock()
	s.watch = watch
	s.mu.Unlock()
	watch.stream = s
}

// Write consumes an arbitrary chunk of stdout. It always reports success: the
// parser is an observer and must never give the child an I/O error.
func (s *claudeStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	before := s.accepted
	for rest := p; len(rest) > 0; {
		newline := bytes.IndexByte(rest, '\n')
		chunk := rest
		if newline >= 0 {
			chunk, rest = rest[:newline], rest[newline+1:]
		} else {
			rest = nil
		}
		if !s.discarding {
			if len(s.line)+len(chunk) > maxClaudeEventBytes {
				s.line, s.discarding = s.line[:0], true
				s.anomalies++
			} else {
				s.line = append(s.line, chunk...)
			}
		}
		if newline >= 0 {
			if !s.discarding {
				s.handle(s.line)
			}
			s.line, s.discarding = s.line[:0], false
		}
	}
	accepted, watch := s.accepted, s.watch
	s.mu.Unlock()
	// The DURABLE half, outside the lock and throttled by the watch. The key is
	// qualified by the physical attempt: RecordProviderProgress ignores a key
	// equal to the stored one, and a bare event count restarts at 1 after an
	// abandoned attempt is re-adopted.
	if accepted != before {
		watch.record(fmt.Sprintf("%d:", s.attempt), accepted)
	}
	return len(p), nil
}

// claudeEvent is the typed subset of one stream-json line this parser reads.
// Fields whose shape varies between event kinds stay raw until the kind is
// known, so a variant shape elsewhere cannot make a valid line unparseable.
type claudeEvent struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// system/api_retry
	Error       json.RawMessage `json:"error"`
	ErrorStatus json.RawMessage `json:"error_status"`
	NoResponse  json.RawMessage `json:"no_response"`
	// result
	IsError           bool              `json:"is_error"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
}

type claudeContentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
}

// handle applies one complete line. Called with s.mu held.
func (s *claudeStream) handle(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var event claudeEvent
	if err := json.Unmarshal(line, &event); err != nil {
		s.anomalies++
		return
	}
	s.seq++
	var blocks []claudeContentBlock
	_ = json.Unmarshal(event.Message.Content, &blocks)
	mainThread := event.ParentToolUseID == nil
	switch event.Type {
	case "assistant":
		if len(blocks) == 0 {
			return
		}
		if mainThread {
			// A NEW main-thread assistant turn means the previous turn's tool
			// results were all delivered, even if one was an oversized or
			// malformed line this parser could not read. Lines sharing a
			// message.id are one turn - parallel tool calls - and must not
			// close each other.
			if id := event.Message.ID; id != "" && id != s.turn {
				if s.turn != "" {
					clear(s.open)
				}
				s.turn = id
			}
			for _, block := range blocks {
				if block.Type == "tool_use" && block.ID != "" && len(s.open) < maxClaudeOpenTools {
					s.open[block.ID] = struct{}{}
				}
			}
		}
		s.progress()
	case "user":
		results := 0
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			results++
			// Nested/subagent results never touch the suspension set; an
			// unknown id is simply absent from it.
			if mainThread {
				delete(s.open, block.ToolUseID)
			}
		}
		if results > 0 {
			s.progress()
		}
	case "system":
		if event.Subtype == "api_retry" {
			s.retry, s.retrySeq = claudeRetryClass(event), s.seq
		}
	case "result":
		s.sawResult, s.isError, s.subtype = true, event.IsError, event.Subtype
		s.denials = len(event.PermissionDenials)
	}
}

// progress records one accepted event and refreshes the in-memory watch. It
// runs under s.mu, so a tool close and the refresh it causes are one step as
// far as the watcher's suspension check is concerned.
func (s *claudeStream) progress() {
	s.accepted++
	s.progressSeq = s.seq
	s.watch.touch()
}

// holdsOpenTool reports whether a main-thread tool call is awaiting its result.
func (s *claudeStream) holdsOpenTool() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open) > 0
}

// claudeRetryClass maps a typed system/api_retry event onto an existing failure
// class. Only typed fields are read - the category string and the HTTP status -
// never any message text, and an unrecognized category stays FailureUnknown.
func claudeRetryClass(event claudeEvent) FailureClass {
	var category string
	_ = json.Unmarshal(event.Error, &category)
	var status *int
	_ = json.Unmarshal(event.ErrorStatus, &status)
	switch category {
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold", "billing_error", "cloud_credential_error":
		return FailureProviderAccountUnavailable
	case "rate_limit":
		return FailureProviderRateLimited
	case "overloaded":
		return FailureProviderUnavailable
	case "server_error":
		// A 500 is the provider failing at a request, not proof it is
		// unreachable - the same line sharedAgentSignals draws. Only the
		// gateway statuses say the endpoint itself is unavailable.
		if status != nil && (*status == 502 || *status == 503 || *status == 504) {
			return FailureProviderUnavailable
		}
	}
	// No HTTP response at all, stated as such: the dead-network shape #238
	// exists to bound.
	if status == nil && len(event.NoResponse) > 0 && string(event.NoResponse) != "null" {
		return FailureProviderUnavailable
	}
	return FailureUnknown
}

// claudeStreamOutcome is what the stream established by the time the process
// returned.
type claudeStreamOutcome struct {
	// Condition is the typed provider condition that may outrank a generic
	// classification: a retry condition NEWER than the last accepted progress,
	// or FailureUnknown.
	Condition FailureClass
	// Failed is true when a process that exited 0 still failed by the protocol:
	// no valid final result, or a result with is_error.
	Failed                                  bool
	Accepted                                int64
	OpenTools, PermissionDenials, Anomalies int
}

// outcome reads the final state. A result is REQUIRED only when the process
// exited 0; a non-zero exit follows the ordinary failure path, but whatever the
// stream had typed before it still feeds the condition.
func (s *claudeStream) outcome(exitedZero bool) claudeStreamOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	condition := FailureUnknown
	if s.retrySeq > s.progressSeq {
		condition = s.retry
	}
	return claudeStreamOutcome{
		Condition: condition,
		Failed:    exitedZero && (!s.sawResult || s.isError),
		Accepted:  s.accepted, OpenTools: len(s.open),
		PermissionDenials: s.denials, Anomalies: s.anomalies,
	}
}
