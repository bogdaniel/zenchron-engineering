package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInterruptedReviewContinuesWithPersistedBudget(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, 30*time.Minute)
	initial := f.state(id)
	if initial.run.Budgets == nil || initial.run.Budgets.WallLimit != 30*time.Minute {
		t.Fatal("fixture did not persist production budgets")
	}
	number := initial.projection.PullRequest.Number
	head := initial.projection.CandidateRevision
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9510, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("finish the helper"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	calls := len(f.provider.requests)
	// Spend the original envelope inside the real remediation dispatch, after
	// changing the owned candidate. The inactivity failure still records delivery.
	f.provider.mutate = func(dir string) error {
		f.clock.advance(31 * time.Minute)
		return os.WriteFile(filepath.Join(dir, "review.go"), []byte("package candidate\n// partial review fix\n"), 0600)
	}
	f.provider.Result.Failure = &ProviderFailure{Classification: FailureProviderNoProgress}
	outcome := f.reconcile(id)
	if terminalDisposition(outcome.Disposition) {
		t.Fatalf("interrupted review became terminal: %+v", outcome)
	}
	checkpoint := f.state(id)
	if len(f.provider.requests) != calls+1 || countType(checkpoint.events, EventCandidateChanged) < 2 {
		t.Fatal("did not drive real remediation")
	}
	key := "pull_request_comment:9510"
	if !checkpoint.feedbackState().Consumed[key] || len(checkpoint.pendingFeedbackKeys()) != 0 {
		t.Fatal("delivery identity not retained")
	}
	if checkpoint.projection.CandidateRevision == head || checkpoint.projection.CandidateComplete {
		t.Fatal("partial work was not preserved as an incomplete checkpoint")
	}
	grant, ok := checkpoint.reviewContinuationGrant()
	if !ok || grant.Allowance != 30*time.Minute {
		t.Fatalf("missing bounded continuation: %+v", grant)
	}
	workspace, err := f.runtime.workspace(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Dir, "review.go")); err != nil {
		t.Fatal(err)
	}
	reopen(t, f)
	f.inject(func(call GitHubCall) error {
		if call.Method == "UpdatePullRequest" {
			pr := f.forge.PullRequests[number]
			pr.HeadSHA = f.forge.Refs[candidateBranch(id)]
			f.forge.PullRequests[number] = pr
		}
		return nil
	})
	f.provider.Result.Failure = nil
	f.provider.mutate = func(dir string) error {
		if dir != workspace.Dir {
			t.Fatal("continuation switched workspace")
		}
		if _, err := os.Stat(filepath.Join(dir, "review.go")); err != nil {
			t.Fatal("partial remediation lost", err)
		}
		return os.WriteFile(filepath.Join(dir, "review.go"), []byte("package candidate\n// completed review fix\n"), 0600)
	}
	outcome = f.reconcile(id)
	final := f.state(id)
	if outcome.Disposition != Waiting || outcome.Reason != ReasonGoalStateReached {
		t.Fatalf("delivered continuation did not return to review: %+v", outcome)
	}
	if len(f.provider.requests) != calls+2 {
		t.Fatalf("continuation did not execute: %+v", outcome)
	}
	request := f.provider.requests[len(f.provider.requests)-1]
	if len(request.Feedback) != 0 || request.Deadline == nil || request.Budgets.WallLimit <= 0 {
		t.Fatal("continuation lost bounded delivery semantics")
	}
	if final.projection.PullRequest.Number != number || final.projection.PullRequest.HeadRevision != final.projection.CandidateRevision {
		t.Fatalf("same PR not republished: %+v", outcome)
	}
	if final.projection.Assurance == nil || !final.projection.CandidateComplete || len(final.outstandingReviewKeys()) != 0 {
		t.Fatalf("review was not verified and discharged: %+v", outcome)
	}
	if !final.feedbackState().Consumed[key] || countType(final.events, EventFeedbackConsumed) != 1 || countType(final.events, EventReviewContinuationGranted) != 1 {
		t.Fatal("continuation reset identity or minted additional budget")
	}
	repeat, err := f.runtime.ObserveFeedback(context.Background(), id)
	if err != nil || repeat.New != 0 {
		t.Fatalf("review was redelivered: %+v %v", repeat, err)
	}
	// A new productive obligation cannot spend the discharged review grant.
	final.projection.CandidateComplete = false
	if d, reason := final.conditions(); d != Failed || reason != "run_wall_budget_exhausted" {
		t.Fatalf("discharged review authorized unrelated work: %s/%s", d, reason)
	}
}

func TestDischargedReviewDoesNotExemptUnrelatedWork(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	payload := func(p any) json.RawMessage {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	events := []EngineeringEvent{
		{Type: EventFeedbackObserved, Payload: payload(FeedbackObservedPayload{FeedbackDecision: FeedbackDecision{Key: "review:1", Admitted: true}})},
		{Type: EventFeedbackConsumed, Payload: payload(FeedbackConsumedPayload{Keys: []string{"review:1"}})},
		{Type: EventOperationAfter, Payload: payload(RunOperation{Kind: OpPullRequestUpdate, State: Succeeded})},
	}
	s := conditionsFixture(start, &steppingClock{at: start.Add(31 * time.Minute)}, RunBudgets{WallLimit: 30 * time.Minute}, events)
	s.run.Budgets = &RunBudgets{WallLimit: 30 * time.Minute}
	if d, r := s.conditions(); d != Failed || r != "run_wall_budget_exhausted" {
		t.Fatalf("historical review exempted unrelated work: %s/%s", d, r)
	}
}

func TestReviewContinuationCannotBeRenewedByCommentsOrRestart(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, 30*time.Minute)
	number := f.state(id).projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9520, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("fix this"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Close the idle review wait so the injected clock measures active work.
	s := f.state(id)
	if err := f.runtime.recordDisposition(s, Waiting, "operation_unavailable"); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(31 * time.Minute)
	s = f.state(id)
	if err := f.runtime.grantReviewContinuation(s); err != nil {
		t.Fatal(err)
	}
	grant, ok := f.state(id).reviewContinuationGrant()
	if !ok {
		t.Fatal("no grant")
	}
	f.clock.advance(31 * time.Minute)
	for i := 0; i < 2; i++ {
		f.forge.ConversationComments[number] = append(f.forge.ConversationComments[number], GitHubComment{ID: int64(9521 + i), Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("more feedback"), CreatedAt: f.clock.Now()})
		if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		requests := len(f.provider.requests)
		outcome := f.reconcile(id)
		if outcome.Disposition != Waiting || outcome.Reason != ReasonReviewBudgetExhausted {
			t.Fatalf("spent grant did not preserve obligation: %+v", outcome)
		}
		state := f.state(id)
		current, _ := state.reviewContinuationGrant()
		if current != grant || countType(state.events, EventReviewContinuationGranted) != 1 || len(f.provider.requests) != requests {
			t.Fatal("comment minted compute")
		}
		reopen(t, f)
	}
}

func TestReviewContinuationRejectsLateProviderSuccess(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, 30*time.Minute)
	number := f.state(id).projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9530, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("finish review work"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.recordDisposition(f.state(id), Waiting, "operation_unavailable"); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(31 * time.Minute)
	before := countType(f.state(id).events, EventExecutionCompleted)
	var candidateDir string
	f.provider.mutate = func(dir string) error {
		candidateDir = dir
		f.clock.advance(31 * time.Minute)
		return os.WriteFile(filepath.Join(dir, "late.go"), []byte("package candidate\n"), 0600)
	}
	outcome := f.reconcile(id)
	if outcome.Disposition != Waiting || outcome.Reason != ReasonReviewBudgetExhausted {
		t.Fatalf("late provider escaped continuation bound: %+v", outcome)
	}
	state := f.state(id)
	if countType(state.events, EventExecutionCompleted) != before {
		t.Fatal("late success was admitted as completed execution")
	}
	if !state.feedbackState().Consumed["pull_request_comment:9530"] || len(state.outstandingReviewKeys()) != 1 {
		t.Fatal("late invocation lost accepted obligation")
	}
	if _, err := os.Stat(filepath.Join(candidateDir, "late.go")); err != nil {
		t.Fatal("late partial work was orphaned", err)
	}
	request := f.provider.requests[len(f.provider.requests)-1]
	if request.Deadline == nil || request.Budgets.WallLimit <= 0 || request.Budgets.WallLimit > 30*time.Minute {
		t.Fatal("continuation request was not bounded")
	}
}

func TestCompletedUnchangedReviewDoesNotGrantContinuation(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, 30*time.Minute)
	number := f.state(id).projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9540, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("check the existing fix"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	outcome := f.reconcile(id)
	if outcome.Disposition != Waiting || outcome.Reason != ReasonGoalStateReached {
		t.Fatalf("review did not complete: %+v", outcome)
	}
	s := f.state(id)
	if !s.feedbackState().Consumed["pull_request_comment:9540"] || len(s.outstandingReviewKeys()) != 0 {
		t.Fatal("completed unchanged review remains outstanding")
	}
	if err := f.runtime.recordDisposition(s, Waiting, "operation_unavailable"); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(31 * time.Minute)
	outcome = f.reconcile(id)
	if outcome.Disposition != Failed || outcome.Reason != "run_wall_budget_exhausted" {
		t.Fatalf("old review kept unrelated work alive: %+v", outcome)
	}
	if countType(f.state(id).events, EventReviewContinuationGranted) != 0 {
		t.Fatal("discharged feedback minted a grant")
	}
}
