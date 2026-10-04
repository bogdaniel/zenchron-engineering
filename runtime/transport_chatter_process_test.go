//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// The transport-chatter cases that need a real bounded process (Unix-only,
// like the inactivity fixtures they drive); the pure classification cases
// stay in transport_chatter_test.go.

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// End to end: Codex replaying the recorded loop on stderr is ended by the
// inactivity policy as provider_no_progress. The stderr tail names a transport
// failure, and is deliberately not believed on this path: a candidate can
// write the same tail.
func TestCodexReplayingTheReconnectLoopIsTerminatedForInactivity(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "codex_reconnect_loop_317.log"))
	if err != nil {
		t.Fatal(err)
	}
	provider, request, _ := inactivityFixture(t,
		"trap '' TERM\nwhile :; do while IFS= read -r l; do printf '%s\\n' \"$l\" >&2; sleep 0.02; done < "+
			shellQuoted(fixture)+"; done\n")
	request.Budgets.InactivityLimit = inactivityWindow
	// Short, so a restored defect fails as an assertion, not a hang.
	request.Budgets.WallLimit = 5 * time.Second

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
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure = %#v, want %q", result.Failure, FailureProviderNoProgress)
	}
	if RouteFailure(result.Failure.Classification) == RouteWait {
		t.Fatal("the reconnect loop was parked on an external wait")
	}
}

// Candidate-controlled output matching a transport Signal withholds the
// refresh and grants no transport failure, on EITHER stream: the kill is
// provider_no_progress, never provider_unavailable and never a wait.
func TestMatchedCandidateOutputGrantsNoFailureClass(t *testing.T) {
	for name, redirect := range map[string]string{"stdout": "", "stderr": " >&2"} {
		t.Run(name, func(t *testing.T) {
			provider, request, _ := inactivityFixture(t,
				"trap '' TERM\nwhile :; do echo 'dial tcp 127.0.0.1:5432: connect: connection refused'"+redirect+"; sleep 0.02; done\n")
			request.Budgets.InactivityLimit = inactivityWindow
			request.Budgets.WallLimit = 5 * time.Second

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
			if RouteFailure(result.Failure.Classification) == RouteWait {
				t.Fatal("candidate output routed the run to an external wait")
			}
		})
	}
}
