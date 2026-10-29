package runtime

// Output from a byte_output provider is not always progress (#314).
//
// Run #317's Codex attempt spent its whole thirty-minute wall envelope in a
// reconnect loop. Every minute or so it printed `ERROR: Reconnecting...
// waiting for network`, and every four and a half minutes a models-manager
// `error sending request`. Each of those lines refreshed the inactivity
// window, so the bound built for exactly this case (#238) never fired.
//
// progressByteOutputExcludingTransportChatter keeps byte-output supervision
// and removes only KNOWN false progress: a complete output line matching one
// of the provider's own transport patterns does not refresh the window. Any
// other line refreshes it exactly as before.
//
// THE TRUST BOUNDARY IS ASYMMETRIC. Both streams carry candidate-controlled
// text - a test that prints `connection refused` in a retry loop looks the
// same here as Codex failing to connect. A match may therefore only WITHHOLD
// a refresh. It never classifies anything: this file returns no FailureClass,
// and terminal classification still reads only the stderr tail, through
// classifyAgentFailure, after the process has exited.
//
// An inactivity kill under this mode is ALWAYS provider_no_progress: the
// stderr tail is not consulted on that path (cli_agent.go), because on Codex a
// candidate's stderr shares it. So a real #317 loop ends as no-progress and is
// retried under the existing attempt authority, not parked on a wait.
//
// Known residuals:
//   - a legitimate tool that prints nothing but matching lines for a whole
//     window is terminated as provider_no_progress. The run is still finite
//     and the attempt is retried. If live evidence shows it matters, the answer
//     is a stronger provider oracle, not letting matched text grant a class;
//   - other Codex retry chatter is known and NOT covered, e.g.
//     `stream error: ... retrying N/5`. It still refreshes, as before;
//   - the patterns come from one recorded macOS transcript. A missed pattern
//     degrades to plain byte output, never to anything less bounded.

import (
	"bytes"
	"context"
	"regexp"
)

// maxJudgedLineBytes bounds the unterminated line held while waiting for its
// newline. Output that reaches it without one is judged as it stands, so a
// long run of text with no newline still refreshes the window instead of
// being buffered without bound.
//
// ponytail: two known edges, accepted until evidence says otherwise.
// (1) A line longer than this is judged in 4 KiB pieces, so a pattern split
// across a piece boundary is missed and that piece refreshes (safe direction).
// (2) Slow unterminated output - progress dots, a spinner without newline -
// refreshes only when its newline arrives or the bound fills, where byte
// output refreshed on every write. Codex exec writes whole lines to a pipe,
// so neither is observed; judge partial lines eagerly if one ever is.
const maxJudgedLineBytes = 4 << 10

// ansiSGR is a terminal colour escape. Codex colours its diagnostics when it
// thinks it can, and `\x1b[31mERROR\x1b[0m: Reconnecting...` must still match.
var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type transportChatterKey struct{}

// withTransportChatter carries the provider's non-progress line patterns to
// the process runner, as withClaudeStream does for Claude's parser. Patterns
// are lowercase substrings.
func withTransportChatter(ctx context.Context, patterns []string) context.Context {
	return context.WithValue(ctx, transportChatterKey{}, patterns)
}

func transportChatterFrom(ctx context.Context) []string {
	patterns, _ := ctx.Value(transportChatterKey{}).([]string)
	return patterns
}

// transportChatterPatterns is everything that withholds a refresh for spec:
// its non-classifying InactivityNonProgress list plus the Match text of its
// own Signals. Reading Signals here grants nothing. The class is dropped.
func transportChatterPatterns(spec cliAgentSpec) []string {
	patterns := append([]string(nil), spec.InactivityNonProgress...)
	for _, signal := range spec.Signals {
		patterns = append(patterns, signal.Match)
	}
	return patterns
}

// chatterFilter refreshes the inactivity window for every complete output
// line that matches no transport pattern. There is one per stream, and each
// is written by the one goroutine os/exec copies that stream on.
type chatterFilter struct {
	progress func(int)
	patterns []string
	pending  []byte
}

func (f *chatterFilter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			f.pending = append(f.pending, p...)
			if len(f.pending) >= maxJudgedLineBytes {
				f.judge()
			}
			break
		}
		f.pending = append(f.pending, p[:i+1]...)
		f.judge()
		p = p[i+1:]
	}
	return n, nil
}

func (f *chatterFilter) judge() {
	if !isTransportChatter(f.pending, f.patterns) {
		f.progress(len(f.pending))
	}
	f.pending = f.pending[:0]
}

// isTransportChatter reports whether line matches a pattern, ignoring case.
func isTransportChatter(line []byte, patterns []string) bool {
	lower := bytes.ToLower(ansiSGR.ReplaceAll(line, nil))
	for _, pattern := range patterns {
		if bytes.Contains(lower, []byte(pattern)) {
			return true
		}
	}
	return false
}
