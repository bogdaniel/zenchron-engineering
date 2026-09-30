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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// The progress oracles an adapter's inactivity bound can be supervised by.
// Recorded in InvocationProvenance so incident review can tell which one
// measured a stalled attempt.
const (
	progressByteOutput                          = "byte_output"
	progressByteOutputExcludingTransportChatter = "byte_output_excluding_transport_chatter"
	progressStructuredClaudeEvents              = "structured_claude_events"
)

// ProgressStructuredClaudeEvents is the recorded progress mode under which an
// attempt's permission denials are observable at all, exported so a reader of
// recorded provenance decides from the record rather than from live config.
const ProgressStructuredClaudeEvents = progressStructuredClaudeEvents

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
// anything slower than its own mutex. The in-memory refresh happens inline; the
// durable state is handed to the watch's coalescing recorder, which never
// blocks, so a slow or contended state write can never backpressure Claude's
// stdout pipe and manufacture the silence it would then diagnose.
type claudeStream struct {
	attempt int

	// LOCK ORDER: inactivityWatch.expire holds w.mu and then takes this mu
	// (holdsOpenTool). Never take a watch lock while holding this one.
	mu    sync.Mutex
	watch *inactivityWatch
	// progressAt is when the last event was accepted, and openedAt when the
	// main-thread open-tool set last became non-empty (zero while it is
	// empty). They are what the durable row is told (#352).
	progressAt, openedAt time.Time
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

	sawResult   bool
	isError     bool
	denials     int
	deniedTools []string
	// answer is Claude's own semantic final answer text, decoded exactly once
	// out of a successful final result's "result" field. hasAnswer
	// distinguishes "no valid final result, or it was an error" from "the
	// result held the empty string" - both leave answer at its zero value, but
	// only the latter is a real (if useless) answer.
	answer    string
	hasAnswer bool
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

// attach binds the watch this stream refreshes and records through. Either may
// be nil.
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
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	before, openedBefore := s.accepted, s.openedAt
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
				// Per EVENT, not per chunk (#355): tool A's result and tool
				// B's call can share one pipe read, and B's suspension starts
				// at B, not at A.
				if len(s.open) == 0 {
					s.openedAt = time.Time{}
				} else if s.openedAt.IsZero() {
					s.openedAt = now
				}
			}
			s.line, s.discarding = s.line[:0], false
		}
	}
	// The DURABLE half is handed to the recorder. The key is qualified by the
	// physical attempt, because RecordProviderProgress ignores a key equal to
	// the stored one and a bare event count restarts at 1 after an abandoned
	// attempt is re-adopted. An open-tool transition is recorded too, even
	// with no new progress - the final result closes tools without being
	// progress - so the durable suspension follows the one the watchdog reads
	// (#352). tool_progress and every other unaccepted event change neither.
	//
	// It is handed over UNDER this lock, so observations reach the recorder in
	// stream order and an older suspension state can never be queued behind a
	// newer one (#355). That keeps the LOCK ORDER: observe takes only the
	// recorder's own leaf lock and never blocks, and no watch lock is taken.
	if s.accepted != before {
		s.progressAt = now
	}
	if s.accepted != before || !s.openedAt.Equal(openedBefore) {
		s.watch.record(fmt.Sprintf("%d:%d", s.attempt, s.accepted), s.progressAt, s.openedAt)
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
	// result. IsError stays raw and is decoded strictly where it is read: it
	// decides success, so a missing or type-drifted value must be refused, and
	// the decoder would otherwise leave false behind (even through a pointer,
	// which it allocates before it discovers the mismatch).
	IsError           json.RawMessage   `json:"is_error"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	// Answer is the JSON field literally named "result" on a result event: this
	// invocation's SEMANTIC final answer, exactly as Claude wrote it before the
	// stream-json transport wrapped it in this event's own JSON encoding. It
	// stays raw here for the same reason IsError does - decoded strictly, only
	// once a valid final result is otherwise established - so a missing or
	// type-drifted value never promotes to an answer.
	Answer json.RawMessage `json:"result"`
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
	// Only a SYNTAX error makes a line an anomaly here. A field whose type
	// drifted in a later CLI - permission_denials as an object, say - is a type
	// error the decoder reports after filling every other field, and discarding
	// the whole event for optional metadata would turn every successful run's
	// final result into a protocol failure. The REQUIRED result fields are
	// validated where they are read, below: the decoder skips a drifted field,
	// so checking them by value also covers a drift it did not report first.
	var typeDrift *json.UnmarshalTypeError
	if err := json.Unmarshal(line, &event); err != nil && !errors.As(err, &typeDrift) {
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
					s.openedAt = time.Time{} // the previous turn's tools ended
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
		// A result without a typed boolean is_error AND a typed string subtype
		// - the two terminal inputs #322 names - is not a VALID final result,
		// so it can never make an exit 0 succeed: it is an anomaly, and the run
		// fails closed for want of a valid one. Missing, drifted and null all
		// qualify: null decodes into a *bool as nil, and the decoder leaves a
		// missing, null or drifted string subtype empty. The subtype selects no
		// failure class - error_during_execution and error_max_turns stay
		// FailureUnknown unless a typed retry condition narrows them.
		var isError *bool
		if json.Unmarshal(event.IsError, &isError) != nil || isError == nil || event.Subtype == "" {
			s.anomalies++
			return
		}
		s.sawResult, s.isError = true, *isError
		s.denials = len(event.PermissionDenials)
		s.deniedTools = deniedToolNames(event.PermissionDenials)
		// THE SEMANTIC ANSWER, read only off a result the two typed fields above
		// already established as a valid, non-error final result. A malformed or
		// error result must never promote to an answer, so this is reached only
		// past that gate - and a wrong-typed or missing "result" field simply
		// leaves hasAnswer false, exactly like every other optional field here.
		if !s.isError {
			var answer *string
			if json.Unmarshal(event.Answer, &answer) == nil && answer != nil {
				s.answer, s.hasAnswer = *answer, true
			}
		}
		// The final result ends every turn. An oversized last tool_result line
		// must not leave a stale open tool in the provenance of a clean run.
		clear(s.open)
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
	// FinalResult reports that a valid final result was read. The denial
	// count and DeniedTools come from it, so without one they are unknown.
	FinalResult bool
	// DeniedTools is the bounded set of typed tool identifiers the final
	// result's permission_denials named.
	DeniedTools []string
	// Answer is Claude's own semantic final answer text - the transport's
	// stream-json encoding decoded exactly once, never the raw transcript bytes
	// re-scanned by a consumer that has to guess where the model's prose ends
	// and its transport framing begins. AnswerObserved is set only alongside a
	// successful FinalResult; a failed or missing one never reaches here at
	// all, so this can never promote a malformed answer into a valid one.
	Answer         string
	AnswerObserved bool
}

// outcome reads the final state. A result is REQUIRED only when the process
// exited 0; a non-zero exit follows the ordinary failure path, but whatever the
// stream had typed before it still feeds the condition.
func (s *claudeStream) outcome(exitedZero bool) claudeStreamOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A stream that ended without a trailing newline still ended its last
	// line: nothing documents that the CLI writes one after the result.
	if !s.discarding && len(s.line) > 0 {
		s.handle(s.line)
		s.line = s.line[:0]
	}
	condition := FailureUnknown
	if s.retrySeq > s.progressSeq {
		condition = s.retry
	}
	return claudeStreamOutcome{
		Condition: condition,
		Failed:    exitedZero && (!s.sawResult || s.isError),
		Accepted:  s.accepted, OpenTools: len(s.open),
		PermissionDenials: s.denials, Anomalies: s.anomalies,
		FinalResult: s.sawResult, DeniedTools: s.deniedTools,
		Answer: s.answer, AnswerObserved: s.hasAnswer,
	}
}

// deniedToolNames reads ONLY the typed tool_name of each permission denial:
// never its input, its arguments or any denial text. The result is a sorted,
// de-duplicated set of at most domain.MaxPermissionDeniedTools identifiers, and
// a name that is not a short identifier is skipped rather than recorded, so a
// provider cannot write prose - or anything that needs escaping - into a
// durable row through it.
func deniedToolNames(denials []json.RawMessage) []string {
	var names []string
	for _, raw := range denials {
		var denial struct {
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(raw, &denial) != nil || !domain.IsInvocationIdentifier(denial.ToolName) || slices.Contains(names, denial.ToolName) {
			continue
		}
		names = append(names, denial.ToolName)
	}
	slices.Sort(names)
	if len(names) > domain.MaxPermissionDeniedTools {
		names = names[:domain.MaxPermissionDeniedTools]
	}
	return names
}

// MinProviderInactivitySeconds is the smallest provider inactivity window the
// configuration accepts.
//
// Claude's own background-wait ceiling is derived from the window as three
// quarters of it (claudeBackgroundWaitEnv), and ten seconds is where the
// quarter left for Claude to give up, emit its result and exit is still whole
// seconds rather than a race with the runtime's own bound.
const MinProviderInactivitySeconds = 10

// claudeBackgroundWaitEnv sets CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS strictly
// below the configured per-attempt inactivity window.
//
// Claude's default ceiling for idle waiting on background subagents is
// 600000ms - exactly the runtime's default window - so the two timers raced.
// Three quarters of the window keeps the provider's own wait inside the
// runtime's bound and tightens with it. It is a provider-internal cap, not
// progress and not authority, and support for it is not help-probeable, so it
// is best effort and live acceptance is its evidence.
//
// Zero would mean "wait without limit" to Claude, so a window too small to
// yield a positive millisecond value is refused rather than emitted. Config
// validation keeps that from ever being reached in practice.
func claudeBackgroundWaitEnv(window time.Duration) ([]string, error) {
	if window <= 0 {
		return nil, nil
	}
	ceiling := (window * 3 / 4).Milliseconds()
	if ceiling < 1 {
		return nil, fmt.Errorf("provider inactivity window %s is too small to derive a positive Claude background-wait ceiling below it", window)
	}
	return []string{fmt.Sprintf("CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=%d", ceiling)}, nil
}

// withInvocationEnv appends a spec's invocation-only variables to the
// allowlisted environment. A variable that would replace one the allowlist
// already set (PATH, HOME, the Git guard) or that is shaped like a credential
// is refused: the hook is for non-secret provider controls and nothing else.
func withInvocationEnv(env, extra []string) ([]string, error) {
	present := map[string]bool{}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		present[key] = true
	}
	for _, entry := range extra {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		for _, secret := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"} {
			if strings.Contains(upper, secret) {
				return nil, fmt.Errorf("refused invocation environment variable %s: provider specs may add non-secret controls only", key)
			}
		}
		if key == "" || present[key] {
			return nil, fmt.Errorf("refused invocation environment variable %q: it would replace the runtime's own environment", key)
		}
		present[key] = true
	}
	return append(append([]string(nil), env...), extra...), nil
}
