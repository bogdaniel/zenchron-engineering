package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReviewBudgetStop(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_admitted_%t", admitted), func(t *testing.T) {
			fixture, runID := feedbackFixture(t)
			number := fixture.state(runID).projection.PullRequest.Number
			fixture.forge.ConversationComments[number] = []GitHubComment{{ID: 901, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("private review text"), CreatedAt: fixture.clock.Now()}}
			if admitted {
				observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
				if err != nil || observation.Admitted != 1 {
					t.Fatalf("admission = %+v, %v", observation, err)
				}
			}
			fixture.runtime.deps.Budgets.WallLimit = time.Nanosecond
			before := len(fixture.provider.requests)
			observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
			if err != nil || observation.New != 0 || !strings.Contains(observation.Unavailable, "run_wall_budget_exhausted") {
				t.Fatalf("exhausted admission = %+v, %v", observation, err)
			}
			// A failed status write must surface and remain retryable.
			fixture.forge.Fail = func(call GitHubCall) error {
				if call.Method == "UpdatePullRequest" {
					return fmt.Errorf("forge unavailable")
				}
				return nil
			}
			if _, err := fixture.runtime.Reconcile(context.Background(), runID); err == nil {
				t.Fatal("status write failure was hidden")
			}
			if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
				t.Fatal("failed notice made run terminal")
			}
			fixture.forge.Fail = nil
			outcome := fixture.reconcile(runID)
			if outcome.Disposition != Waiting || outcome.Reason != ReasonReviewBudgetExhausted {
				t.Fatalf("outcome = %+v", outcome)
			}
			if len(fixture.provider.requests) != before {
				t.Fatal("exhausted run invoked provider")
			}
			pending := fixture.state(runID).feedbackState().Pending(fixture.state(runID).projection.Head())
			if admitted && len(pending) != 1 || !admitted && len(pending) != 0 {
				t.Fatalf("pending = %+v", pending)
			}
			notices := 0
			for _, call := range fixture.forge.Calls {
				if call.Method == "UpdatePullRequest" && strings.Contains(call.Body, "Runtime stopped: wall budget exhausted") {
					notices++
					if strings.Contains(call.Body, "private review text") {
						t.Fatal("notice leaked feedback")
					}
				}
			}
			if notices != 2 {
				t.Fatalf("notice attempts = %d", notices)
			}
			fixture.reconcile(runID)
			after := 0
			for _, call := range fixture.forge.Calls {
				if call.Method == "UpdatePullRequest" && strings.Contains(call.Body, "Runtime stopped: wall budget exhausted") {
					after++
				}
			}
			if after != notices {
				t.Fatal("terminal reconcile repeated notice")
			}
		})
	}
}

func TestReviewBudgetNoticePreservesPublicationBoundary(t *testing.T) {
	for _, changed := range []string{"controller", "external_head", "authority", "remote_head"} {
		t.Run(changed, func(t *testing.T) {
			fixture, runID := feedbackFixture(t)
			state := fixture.state(runID)
			switch changed {
			case "controller":
				state.controllerChanged = true
			case "external_head":
				state.projection.ObservedExternalHead = "unexpected"
			case "authority":
				state.snapshot.Operations = nil
			case "remote_head":
				state.projection.PullRequest.HeadRevision = "unexpected"
			}
			before := len(fixture.forge.Calls)
			if err := fixture.runtime.reportReviewBudgetStop(context.Background(), state); err == nil {
				t.Fatal("notice bypassed publication boundary")
			}
			for _, call := range fixture.forge.Calls[before:] {
				if call.Method == "UpdatePullRequest" || call.Method == "CommentOnPullRequest" {
					t.Fatal("unauthorized remote mutation")
				}
			}
		})
	}
}

// Recovery crosses durable settlement and runtime reconstruction, with the
// original persisted ceiling exhausted and the same admitted item still owned
// by the same generation.
func TestPersistedReviewBudgetRecovery(t *testing.T) {
	fixture, runID := feedbackFixture(t)
	state := fixture.state(runID)
	originalLimit := state.run.Budgets.WallLimit
	number := state.projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{{ID: 902, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("please improve the helper"), CreatedAt: fixture.clock.Now()}}
	observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
	if err != nil || observation.Admitted != 1 {
		t.Fatalf("admission = %+v, %v", observation, err)
	}
	pending := fixture.state(runID).feedbackState().Pending(fixture.state(runID).projection.Head())
	if err := fixture.runtime.recordDisposition(fixture.state(runID), Waiting, "execution_pending"); err != nil {
		t.Fatal(err)
	}
	fixture.clock.advance(originalLimit + time.Minute)
	before := len(fixture.provider.requests)
	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Waiting || outcome.Reason != ReasonReviewBudgetExhausted {
		t.Fatalf("settlement = %+v", outcome)
	}
	consumed := fixture.state(runID).activeElapsed(fixture.clock.Now())
	fixture.deps.Budgets.WallLimit = originalLimit * 4
	fixture.runtime = fixture.newRuntime(fixture.deps)
	outcome = fixture.reconcile(runID)
	if outcome.Reason != ReasonReviewBudgetExhausted || len(fixture.provider.requests) != before {
		t.Fatal("config raise bypassed persisted authority")
	}
	operator := RecordedOperator{ID: "maintainer", Provenance: ProvenanceLocalUnverified}
	total := originalLimit * 3
	if err := fixture.runtime.ExtendWallBudget(context.Background(), runID, total, operator); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runtime.ExtendWallBudget(context.Background(), runID, total, operator); err != nil {
		t.Fatal(err)
	}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	state = fixture.state(runID)
	if state.run.Budgets.WallLimit != originalLimit || state.activeElapsed(fixture.clock.Now()) < consumed {
		t.Fatal("original budget or consumption reset")
	}
	if countType(state.events, EventRunWallBudgetExtended) != 1 {
		t.Fatal("extension was not idempotent")
	}
	remaining := state.feedbackState().Pending(state.projection.Head())
	if len(remaining) != 1 || len(pending) != 1 || remaining[0].Key != pending[0].Key {
		t.Fatal("pending obligation moved or disappeared")
	}
	fixture.reconcile(runID)
	if len(fixture.provider.requests) <= before {
		t.Fatal("authorized recovery never invoked provider")
	}
	invocation := fixture.provider.requests[len(fixture.provider.requests)-1]
	if len(invocation.Feedback) != 1 || invocation.Feedback[0].Body != "please improve the helper" {
		t.Fatalf("original obligation not delivered: %+v", invocation.Feedback)
	}
	state = fixture.state(runID)
	if countType(state.events, EventFeedbackConsumed) != 1 {
		t.Fatal("delivery was not recorded exactly once")
	}
	if len(state.feedbackState().Pending(state.projection.Head())) != 0 {
		t.Fatal("feedback was not delivered")
	}
}

func TestBudgetExtensionCannotReviveTerminalRun(t *testing.T) {
	fixture, runID := feedbackFixture(t)
	if _, err := fixture.runtime.settle(fixture.state(runID), Failed, "run_wall_budget_exhausted"); err != nil {
		t.Fatal(err)
	}
	operator := RecordedOperator{ID: "maintainer", Provenance: ProvenanceLocalUnverified}
	if err := fixture.runtime.ExtendWallBudget(context.Background(), runID, 24*time.Hour, operator); err == nil {
		t.Fatal("terminal run accepted new authority")
	}
	before := len(fixture.provider.requests)
	fixture.runtime.deps.Budgets.WallLimit = 24 * time.Hour
	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Failed || len(fixture.provider.requests) != before {
		t.Fatal("terminal run resumed")
	}
}

func TestBudgetExtensionRejectsInsufficientOrUnattributedAuthority(t *testing.T) {
	fixture, runID := feedbackFixture(t)
	operator := RecordedOperator{ID: "maintainer", Provenance: ProvenanceLocalUnverified}
	if err := fixture.runtime.ExtendWallBudget(context.Background(), runID, 4*time.Hour, operator); err == nil {
		t.Fatal("extension accepted outside budget wait")
	}
	fixture.runtime.deps.Budgets.WallLimit = time.Nanosecond
	fixture.reconcile(runID)
	for _, tc := range []struct {
		total    time.Duration
		operator RecordedOperator
	}{
		{4 * time.Hour, RecordedOperator{}},
		{4 * time.Hour, RecordedOperator{ID: "maintainer", Provenance: "forged"}},
		{0, operator},
		{time.Nanosecond, operator},
	} {
		if err := fixture.runtime.ExtendWallBudget(context.Background(), runID, tc.total, tc.operator); err == nil {
			t.Fatalf("accepted invalid extension: %+v", tc)
		}
	}
	if countType(fixture.state(runID).events, EventRunWallBudgetExtended) != 0 {
		t.Fatal("refused extension wrote authority")
	}
}
