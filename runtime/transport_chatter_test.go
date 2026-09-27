package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexReconnectLoop317 is lines 113-168 of the recorded #317 Codex attempt:
// the reconnect burst, then the steady loop that held the invocation for the
// rest of its wall envelope. They contain no credential or local path.
func codexReconnectLoop317(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_reconnect_loop_317.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// replayOnClock feeds lines through a chatterFilter on an injected clock, one
// line per cadence, and reports when silence first reached limit.
func replayOnClock(lines []string, patterns []string, cadence, limit time.Duration) (time.Duration, bool) {
	var now, last time.Duration
	filter := &chatterFilter{progress: func(int) { last = now }, patterns: patterns}
	for _, line := range lines {
		now += cadence
		if now-last >= limit {
			return now, true
		}
		filter.Write([]byte(line + "\n"))
	}
	return now, false
}

// The #317 shape, replayed at 50s per line - denser than the recorded steady
// loop (one line about every 55s) - against the shipped 600s window.
func TestCodexReconnectLoopCannotHoldTheInactivityWindowOpen(t *testing.T) {
	lines := codexReconnectLoop317(t)
	const cadence, limit = 50 * time.Second, DefaultProviderInactivitySeconds * time.Second
	for _, line := range lines {
		if !isTransportChatter([]byte(line), transportChatterPatterns(codexSpec)) {
			t.Fatalf("a recorded reconnect-loop line is not recognized as chatter: %q", line)
		}
	}
	at, fired := replayOnClock(lines, transportChatterPatterns(codexSpec), cadence, limit)
	if !fired || at != limit {
		t.Fatalf("inactivity fired=%v at %s, want it to fire at %s", fired, at, limit)
	}
	// RESTORED DEFECT: under plain byte output the same loop refreshes on
	// every line and the window never closes, which is what #317 did live.
	if at, fired := replayOnClock(lines, nil, cadence, limit); fired {
		t.Fatalf("byte-output semantics fired at %s; the fixture no longer reproduces #317", at)
	}
}

func TestOutputOtherThanTransportChatterStillRefreshes(t *testing.T) {
	patterns := transportChatterPatterns(codexSpec)
	const cadence, limit = 50 * time.Second, 600 * time.Second
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, "ERROR: Reconnecting... waiting for network")
		if i%5 == 4 {
			lines = append(lines, "exec bash -lc 'go test ./runtime' in /work succeeded in 41.2s:")
		}
	}
	if at, fired := replayOnClock(lines, patterns, cadence, limit); fired {
		t.Fatalf("ordinary Codex output interleaved with chatter was killed at %s", at)
	}

	var refreshed int
	filter := &chatterFilter{progress: func(n int) { refreshed += n }, patterns: patterns}
	// A chatter line split across writes is judged whole, so it is withheld.
	filter.Write([]byte("ERROR: Reconn"))
	filter.Write([]byte("ecting... 3/5\n"))
	if refreshed != 0 {
		t.Fatalf("a split chatter line refreshed %d bytes", refreshed)
	}
	// Output with no newline refreshes once the judged-line bound is reached,
	// rather than being buffered without bound.
	filter.Write([]byte(strings.Repeat(".", maxJudgedLineBytes)))
	if refreshed != maxJudgedLineBytes {
		t.Fatalf("unterminated output refreshed %d bytes, want %d", refreshed, maxJudgedLineBytes)
	}
}

// KNOWN RESIDUAL FALSE NEGATIVE (#314): a legitimate tool whose only output
// for a whole window matches a transport pattern - an integration test
// printing its retry loop - withholds every refresh and is terminated. The
// run stays bounded and the attempt is retried. This test pins the accepted
// trade-off so that changing it is a decision.
func TestResidualFalseNegativeMatchingToolOutputIsNotProgress(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "retry: dial tcp 127.0.0.1:5432: connect: connection refused")
	}
	if _, fired := replayOnClock(lines, transportChatterPatterns(codexSpec), 50*time.Second, 600*time.Second); !fired {
		t.Fatal("matching tool output refreshed the window; the documented residual no longer holds")
	}
}

// InactivityNonProgress withholds refreshes and nothing else: the terminal
// classifier never reads it.
func TestNonProgressPatternsNeverClassify(t *testing.T) {
	for _, pattern := range codexSpec.InactivityNonProgress {
		if got := classifyAgentFailure(codexSpec, pattern); got != FailureUnknown {
			t.Fatalf("non-progress pattern %q classified as %q", pattern, got)
		}
	}
}

// End to end: Codex replaying the recorded loop on stderr is ended by the
// inactivity policy, and the stated condition still comes only from the
// existing terminal stderr tail.
func TestCodexReplayingTheReconnectLoopIsTerminatedForInactivity(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "codex_reconnect_loop_317.log"))
	if err != nil {
		t.Fatal(err)
	}
	provider, request, _ := inactivityFixture(t,
		"trap '' TERM\nwhile :; do while IFS= read -r l; do printf '%s\\n' \"$l\" >&2; sleep 0.02; done < "+
			shellQuoted(fixture)+"; done\n")
	request.Budgets.InactivityLimit = inactivityWindow

	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("the reconnect loop was not terminated")
	}
	if result.Invocation == nil || result.Invocation.TerminationCause != "provider_inactivity_limit_reached" {
		t.Fatalf("the inactivity policy did not end the loop: %#v", result.Invocation)
	}
	if result.Invocation.ProgressMode != progressByteOutputExcludingTransportChatter {
		t.Fatalf("progress mode = %q", result.Invocation.ProgressMode)
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderUnavailable {
		t.Fatalf("failure = %#v, want the stderr tail's %q", result.Failure, FailureProviderUnavailable)
	}
}

// Candidate-controlled stdout matching a transport Signal withholds the
// refresh and grants no transport failure: the kill is provider_no_progress.
func TestMatchedCandidateOutputGrantsNoFailureClass(t *testing.T) {
	provider, request, _ := inactivityFixture(t,
		"trap '' TERM\nwhile :; do echo 'dial tcp 127.0.0.1:5432: connect: connection refused'; sleep 0.02; done\n")
	request.Budgets.InactivityLimit = inactivityWindow

	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("the matching loop was not terminated")
	}
	if result.Invocation == nil || result.Invocation.TerminationCause != "provider_inactivity_limit_reached" {
		t.Fatalf("the inactivity policy did not end the loop: %#v", result.Invocation)
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("candidate output granted %#v, want %q", result.Failure, FailureProviderNoProgress)
	}
}
