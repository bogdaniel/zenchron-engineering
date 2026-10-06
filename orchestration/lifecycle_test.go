package orchestration

import "testing"

func TestLiveLifecycleTakesPrecedenceOverHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts ChildFacts
		want  ItemState
	}{
		{"provider finished, runtime committing", ChildFacts{true, RunLive, ActivityWorking, HandoffReported, ""}, ItemRunning},
		{"assurance failed, remediation active", ChildFacts{true, RunLive, ActivityWorking, HandoffAdmitted, OutcomeCompleted}, ItemRunning},
		{"refused handoff, repair active", ChildFacts{true, RunLive, ActivityWorking, HandoffRefused, ""}, ItemRunning},
		{"policy wait after admission", ChildFacts{true, RunLive, ActivityWaiting, HandoffAdmitted, OutcomeCompleted}, ItemWaiting},
		{"queued after refusal", ChildFacts{true, RunLive, ActivityIdle, HandoffRefused, ""}, ItemQueued},
		{"terminal failure dominates stale handoff", ChildFacts{true, RunFailed, ActivityIdle, HandoffAdmitted, OutcomeCompleted}, ItemFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectItem(tc.facts)
			if err != nil || got != tc.want {
				t.Fatalf("state=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}
