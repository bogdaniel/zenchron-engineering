package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// The delivered continuation states an explicit checkpoint-completion
	// claim (#379), bound to the exact checkpoint it inherited - mutating the
	// workspace further is real work, but it does not by itself prove the
	// inherited checkpoint is finished, so a clean return alone must not
	// promote it.
	f.provider.resolveFeedback = true
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

// TestProviderReturnAloneDoesNotDischargeFeedback is the restored #376
// defect, stated directly against outstandingReviewKeys(): an invocation
// that reached a provider, left the workspace unmodified, and simply
// returned success is NOT evidence the admitted feedback was addressed. The
// production no-change-success rule used to discharge exactly this shape
// (ProviderExecuted && !Mutated), which is indistinguishable from a provider
// that deferred unfinished background work and exited. It must stay
// outstanding until an explicit, bound resolution admits it.
func TestProviderReturnAloneDoesNotDischargeFeedback(t *testing.T) {
	payload := func(p any) json.RawMessage {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	const head = "head-1"
	baseEvents := []EngineeringEvent{
		{Type: EventFeedbackObserved, Payload: payload(FeedbackObservedPayload{FeedbackDecision: FeedbackDecision{Key: "review:1", Admitted: true}})},
		{Type: EventGitHubPRObserved, Payload: payload(GitHubPRObservedPayload{HeadRevision: head})},
		{Type: EventFeedbackConsumed, Payload: payload(FeedbackConsumedPayload{Keys: []string{"review:1"}, OperationID: "op-1"})},
		{OperationID: "op-1", Type: EventExecutionCompleted, Payload: payload(ExecutionCompletedPayload{SubjectCommit: head})},
	}
	t.Run("return alone leaves feedback outstanding", func(t *testing.T) {
		events := append(append([]EngineeringEvent(nil), baseEvents...), EngineeringEvent{
			Type: EventOperationAfter, Payload: payload(RunOperation{
				ID: "op-1", Kind: OpExecutionInvoke, State: Succeeded,
				Result: payload(mutationResult{ProviderExecuted: true, Mutated: false}),
			}),
		})
		s := &runState{events: events}
		if keys := s.outstandingReviewKeys(); len(keys) != 1 || keys[0] != "review:1" {
			t.Fatalf("a provider that merely returned discharged feedback without stating a resolution: %v", keys)
		}
	})
	t.Run("an explicit bound resolution discharges it", func(t *testing.T) {
		events := append(append([]EngineeringEvent(nil), baseEvents...), EngineeringEvent{
			Type: EventOperationAfter, Payload: payload(RunOperation{
				ID: "op-1", Kind: OpExecutionInvoke, State: Succeeded,
				Result: payload(mutationResult{ProviderExecuted: true, Mutated: false, ResolvedFeedback: []string{"review:1"}}),
			}),
		})
		s := &runState{events: events}
		if keys := s.outstandingReviewKeys(); len(keys) != 0 {
			t.Fatalf("an admitted, bound no-change resolution did not discharge feedback: %v", keys)
		}
	})
}

// TestUnresolvedFeedbackReturnFailsTheOperationAndStaysActionable is the full
// runtime-lifecycle restoration of the #376 defect, driven through the real
// EngineeringRuntime rather than only against outstandingReviewKeys() in
// isolation. REQUEST_CHANGES is delivered; the provider returns on every
// attempt having changed nothing and stated no resolution - the
// isolatedProvider default, since its "mutate" rewrites candidate.go with the
// exact bytes already committed, producing a clean workspace, and
// resolveFeedback is left unset.
//
// bindExecutionInvoke's feedback branch is built from pendingFeedbackKeys():
// admitted, undelivered feedback. The first attempt's EventFeedbackConsumed
// empties that set immediately, before the attempt even settles - which used
// to mean the operation, once marked Succeeded by the old no-change-success
// rule, satisfied its binding forever while discharging nothing. This proves
// the operation itself is never a successful completion for that shape, that
// the feedback stays actionable rather than stranded behind a settled
// operation, and that the run reaches a bounded successor retry and then a
// truthful attempt-budget failure - never goal_state_reached.
func TestUnresolvedFeedbackReturnFailsTheOperationAndStaysActionable(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, time.Hour)
	number := f.state(id).projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9550, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("tighten this"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	key := "pull_request_comment:9550"
	calls := len(f.provider.requests)

	outcome := f.reconcile(id)

	// Both attempts of the SAME binding ran: the feedback_unresolved failure
	// did not strand the obligation behind a settled operation that nothing
	// proposes again, and it did not mint a second, different binding either.
	if got := len(f.provider.requests) - calls; got != 2 {
		t.Fatalf("expected exactly the 2 configured attempts of the feedback binding to run, got %d", got)
	}
	state := f.state(id)
	var invoke RunOperation
	found := 0
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke && strings.HasPrefix(bindingOf(op), "feedback|") {
			invoke, found = op, found+1
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one feedback execution.invoke operation, found %d", found)
	}
	if invoke.State != OperationFailed {
		t.Fatalf("an invocation that neither changed the candidate nor stated an admitted resolution was recorded as %s, not a failed operation", invoke.State)
	}
	var result mutationResult
	if err := decodeJSON(invoke.Result, &result); err != nil || result.FailureClass != FailureFeedbackUnresolved {
		t.Fatalf("unresolved feedback did not record failure_class=feedback_unresolved: %+v (decode err %v)", result, err)
	}
	if keys := state.outstandingReviewKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("feedback that was neither addressed nor explicitly resolved is not outstanding: %v", keys)
	}
	if outcome.Disposition == Waiting && outcome.Reason == ReasonGoalStateReached {
		t.Fatalf("an unaddressed REQUEST_CHANGES reached goal_state_reached: %+v", outcome)
	}
	if outcome.Disposition != Failed || !BudgetBoundary(outcome.Disposition, outcome.Reason) {
		t.Fatalf("exhausted attempts did not stop the run truthfully under its existing finite authority: %+v", outcome)
	}
}

// TestMalformedFeedbackResolutionFailsAndRoutesCorrectively proves the other
// half of the reviewer's #376 follow-up: a resolution that does not BIND
// (here, the wrong subject - exactly the AdmitFeedbackResolution refusal
// tested in isolation in feedback_resolution_test.go) must not be silently
// dropped as an otherwise-successful execution. It has to fail the operation
// with an OBSERVABLE EXACT REASON, the same as a resolution that was never
// written at all, and route to the same bounded corrective retry.
func TestMalformedFeedbackResolutionFailsAndRoutesCorrectively(t *testing.T) {
	f, id := feedbackFixtureWithWallLimit(t, time.Hour)
	number := f.state(id).projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 9560, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("tighten this too"), CreatedAt: f.clock.Now()}}
	if _, err := f.runtime.ObserveFeedback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	key := "pull_request_comment:9560"
	// Every attempt claims a subject the invocation was never shown - a bound
	// refusal, through the real write/read/admit path, on every retry.
	f.provider.malformedResolutionSubject = strings.Repeat("f", 40)

	outcome := f.reconcile(id)

	state := f.state(id)
	var invoke RunOperation
	found := 0
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke && strings.HasPrefix(bindingOf(op), "feedback|") {
			invoke, found = op, found+1
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one feedback execution.invoke operation, found %d", found)
	}
	if invoke.State != OperationFailed {
		t.Fatalf("a resolution that did not bind was recorded as %s, not a failed operation", invoke.State)
	}
	var result executionRecord
	if err := decodeJSON(invoke.Result, &result); err != nil || result.FailureClass != FailureFeedbackUnresolved {
		t.Fatalf("an unbound resolution did not record failure_class=feedback_unresolved: %+v (decode err %v)", result, err)
	}
	if result.Diagnostic == nil || !strings.Contains(result.Diagnostic.Message, "claims subject") {
		t.Fatalf("the exact refusal reason was not observable on the diagnostic: %+v", result.Diagnostic)
	}
	if keys := state.outstandingReviewKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("feedback behind an unbound resolution is not outstanding: %v", keys)
	}
	if outcome.Disposition != Failed || !BudgetBoundary(outcome.Disposition, outcome.Reason) {
		t.Fatalf("a resolution that never binds did not stop the run truthfully under its existing finite authority: %+v", outcome)
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
	// Late partial work is REFUSED, so it must not stay in the candidate
	// workspace for the next attempt to inherit (#390) - and it must not be
	// orphaned either: it is preserved, byte for byte, in the journalled
	// quarantine.
	if _, err := os.Stat(filepath.Join(candidateDir, "late.go")); !os.IsNotExist(err) {
		t.Fatalf("refused late work was left in the workspace for the next attempt (err=%v)", err)
	}
	q, ok := state.latestQuarantine()
	if !ok {
		t.Fatal("late partial work was orphaned: no quarantine record")
	}
	if kept, err := os.ReadFile(filepath.Join(f.stateDir, q.Location, "files", "late.go")); err != nil || string(kept) != "package candidate\n" {
		t.Fatalf("late partial work was orphaned: quarantine %s holds %q, %v", q.Location, kept, err)
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
	// The provider's return alone must never discharge the review (#376): it
	// has to STATE that no change is required, through the typed resolution
	// channel, bound to this attempt's exact delivered key and subject.
	f.provider.resolveFeedback = true
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
