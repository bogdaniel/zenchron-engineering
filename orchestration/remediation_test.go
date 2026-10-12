package orchestration

import "testing"

func TestDeriveEpochOutcome(t *testing.T) {
	cases := []struct {
		name              string
		termination       RunTermination
		hasAdmittedOutput bool
		want              EpochOutcome
	}{
		{"live run is running", RunLive, false, EpochRunning},
		{"live run with an output still reports running", RunLive, true, EpochRunning},
		{"cancelled run is cancelled regardless of output", RunCancelled, true, EpochCancelled},
		{"failed run is failed regardless of output", RunFailed, false, EpochFailed},
		{"completed run with an admitted output is completed_admitted", RunCompleted, true, EpochCompletedAdmitted},
		{"completed run with no admitted output is completed_no_output", RunCompleted, false, EpochCompletedNoOutput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveEpochOutcome(c.termination, c.hasAdmittedOutput); got != c.want {
				t.Fatalf("DeriveEpochOutcome(%s, %v) = %s, want %s", c.termination, c.hasAdmittedOutput, got, c.want)
			}
		})
	}
}

func TestDeriveRemediationState(t *testing.T) {
	cases := []struct {
		name                       string
		latestAttemptOutcome       EpochOutcome
		latestAttemptSucceeded     bool
		hasLivePendingDecision     bool
		triggeringFindingDismissed bool
		effectiveCeilingReached    bool
		want                       RemediationState
	}{
		{
			name:                 "a dismissed finding resolves regardless of anything else",
			latestAttemptOutcome: EpochFailed, triggeringFindingDismissed: true,
			effectiveCeilingReached: true, want: RemediationResolved,
		},
		{
			name:                 "a successful latest attempt resolves",
			latestAttemptOutcome: EpochCompletedAdmitted, latestAttemptSucceeded: true,
			want: RemediationResolved,
		},
		{
			name: "an admitted outcome that is not the unit's current epoch does not resolve on its own",
			// e.g. epoch 2 succeeded but a later epoch 3 is now running: this
			// case is exercised by the caller passing epoch 3's own running
			// outcome, never epoch 2's - included here to document the
			// latestAttemptSucceeded flag's purpose explicitly.
			latestAttemptOutcome: EpochRunning, latestAttemptSucceeded: false,
			want: RemediationRunning,
		},
		{
			name:                 "a live pending decision suspends",
			latestAttemptOutcome: EpochCompletedNoOutput, hasLivePendingDecision: true,
			want: RemediationPendingDecision,
		},
		{
			name:                 "exhaustion with the ceiling reached and no dismissal stays unresolved",
			latestAttemptOutcome: EpochFailed, effectiveCeilingReached: true,
			want: RemediationExhaustedUnresolved,
		},
		{
			name:                 "a failed attempt with room left under the ceiling is eligible for a fresh decision",
			latestAttemptOutcome: EpochFailed, effectiveCeilingReached: false,
			want: RemediationPendingDecision,
		},
		{
			name:                 "cancellation behaves exactly like any other non-succeeding terminal outcome",
			latestAttemptOutcome: EpochCancelled, effectiveCeilingReached: true,
			want: RemediationExhaustedUnresolved,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveRemediationState(c.latestAttemptOutcome, c.latestAttemptSucceeded,
				c.hasLivePendingDecision, c.triggeringFindingDismissed, c.effectiveCeilingReached)
			if got != c.want {
				t.Fatalf("DeriveRemediationState(...) = %s, want %s", got, c.want)
			}
		})
	}
}

// TestDownstreamDependencySatisfactionNeverResurfacesAStaleHandoff is the
// direct regression for ADR-0008's second review pass (P1b): a failed or
// exhausted remediation attempt must never make the original, still-disputed
// handoff look current to a dependent again.
func TestDownstreamDependencySatisfactionNeverResurfacesAStaleHandoff(t *testing.T) {
	output := &UnitOutput{HandoffID: "h1", RunID: "r1", CandidateRevision: "c1"}
	suspendingStates := []RemediationState{
		RemediationPendingDecision, RemediationRunning, RemediationExhaustedUnresolved,
	}
	for _, state := range suspendingStates {
		t.Run(string(state), func(t *testing.T) {
			if got := DownstreamDependencySatisfaction(state, output); got != nil {
				t.Fatalf("DownstreamDependencySatisfaction(%s, ...) = %+v, want nil (not satisfied)", state, got)
			}
		})
	}
}

func TestDownstreamDependencySatisfactionReturnsTheHistoricalOutputWhenNoneOrResolved(t *testing.T) {
	output := &UnitOutput{HandoffID: "h1", RunID: "r1", CandidateRevision: "c1"}
	for _, state := range []RemediationState{RemediationNone, RemediationResolved} {
		t.Run(string(state), func(t *testing.T) {
			if got := DownstreamDependencySatisfaction(state, output); got != output {
				t.Fatalf("DownstreamDependencySatisfaction(%s, ...) = %+v, want %+v", state, got, output)
			}
		})
	}
}

func TestDownstreamDependencySatisfactionWithNoHistoricalOutputIsNeverSatisfied(t *testing.T) {
	// A unit that has never admitted anything stays unsatisfied in every
	// state, including None/Resolved - unchanged from today's existing
	// behavior for a unit with no Output at all.
	for _, state := range []RemediationState{RemediationNone, RemediationResolved, RemediationRunning} {
		if got := DownstreamDependencySatisfaction(state, nil); got != nil {
			t.Fatalf("DownstreamDependencySatisfaction(%s, nil) = %+v, want nil", state, got)
		}
	}
}
