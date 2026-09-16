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
			if outcome.Disposition != Failed || outcome.Reason != "run_wall_budget_exhausted" {
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

func TestBudgetDeferredFeedbackCanBeAdmittedAfterBudgetRaise(t *testing.T) {
	fixture, runID := feedbackFixture(t)
	number := fixture.state(runID).projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{{ID: 902, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("please improve the helper"), CreatedAt: fixture.clock.Now()}}
	fixture.runtime.deps.Budgets.WallLimit = time.Nanosecond
	observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
	if err != nil || observation.Unavailable == "" || observation.New != 0 {
		t.Fatalf("deferred observation = %+v, %v", observation, err)
	}
	fixture.runtime.deps.Budgets.WallLimit = time.Hour
	observation, err = fixture.runtime.ObserveFeedback(context.Background(), runID)
	if err != nil || observation.Admitted != 1 {
		t.Fatalf("recovered observation = %+v, %v", observation, err)
	}
	before := len(fixture.provider.requests)
	fixture.reconcile(runID)
	if len(fixture.provider.requests) <= before {
		t.Fatal("recovered feedback never reached a provider")
	}
}
