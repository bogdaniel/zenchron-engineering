package runtime

// A failed reviewer INVOCATION earns exactly one automatic retry of the same
// assignment against the same candidate (#669), and nothing else does.
//
// These run through the real plan reconciler and the real run lifecycle with the
// existing in-process reviewer fake, because what is being proved is what the
// lifecycle does at the stage boundary: not that a predicate returns true, but
// that a stranded reviewer stage is performed again, once, without touching the
// implementer, the candidate, the frozen reviewer or the plan's budgets.

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	malformedVerdict  = `{"schema_version":"0.1","verdict":"maybe"}`
	malformedVerdict2 = `{"verdict":"blocked","summary":"needs changes"}`
)

func acceptingVerdict() ReviewerResult {
	return ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted, Reason: "ok"}
}

// recoveryScenario drives a plan to its reviewer stage with a reviewer whose
// first len(raw) invocations write a malformed document, then answer with the
// verdicts. It returns the fixture and the reviewer's first run.
func recoveryScenario(t *testing.T, fixture *planRunFixture, raw map[int]string, verdicts ...ReviewerResult) (*FakeReviewerProvider, string) {
	t.Helper()
	reviewer := NewFakeReviewerProvider(verdicts...)
	reviewer.Raw = raw
	deps := fixture.deps
	deps.Provider = reviewer
	deps.Agent = ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}
	// More attempts than the protocol correction allows, so what ends a failing
	// reviewer run is the one-correction bound rather than a budget that ran out.
	deps.Budgets.MaxExecutionAttempts = 5
	fixture.engines["claude"] = fixture.newRuntime(deps)
	fixture.approve(t)
	fixture.reconcile(t)
	producer := planStageState(t, fixture, "implementation").RunID
	produceCandidate(t, fixture, producer, "package b\n", true)
	fixture.reconcile(t)
	fixture.reconcile(t)
	run := planStageState(t, fixture, "review").RunID
	if run == "" {
		t.Fatal("the reviewer stage created no run")
	}
	return reviewer, run
}

// driveUntilSettled runs a reviewer's run until it fails or reaches its goal.
// It stops there because the supervisor never reconciles a terminal run; doing
// so in a test only exercises a state production never has.
func driveUntilSettled(t *testing.T, fixture *planRunFixture, runID string) {
	t.Helper()
	engine := fixture.engines["claude"]
	for i := 0; i < 14; i++ {
		outcome, err := engine.Reconcile(context.Background(), runID)
		if err != nil {
			t.Fatalf("drive reviewer run %s: %v", runID, err)
		}
		if outcome.Disposition == Failed || (outcome.Disposition == Waiting && outcome.Reason == ReasonGoalStateReached) {
			return
		}
	}
}

func stageRuns(t *testing.T, fixture *planRunFixture, stageID string) []EngineeringRun {
	t.Helper()
	all, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	var runs []EngineeringRun
	for _, run := range all {
		if run.Plan != nil && run.Plan.PlanID == fixture.plan.ID && run.Plan.StageID == stageID {
			runs = append(runs, run)
		}
	}
	return runs
}

func settlements(t *testing.T, fixture *planRunFixture, stageID, outcome string) int {
	t.Helper()
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type != EventPlanStageSettled {
			continue
		}
		var payload PlanStageSettledPayload
		if err := decodeJSON(event.Payload, &payload); err == nil && payload.StageID == stageID && payload.Outcome == outcome {
			count++
		}
	}
	return count
}

func TestAReviewerWhoseInvocationFailedIsRetriedOnceAgainstTheSameCandidate(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	// The first run's two invocations (the original and its one protocol
	// correction) both fail; the third invocation, in the retry, answers.
	reviewer, first := recoveryScenario(t, fixture,
		map[int]string{0: malformedVerdict, 1: malformedVerdict2},
		acceptingVerdict(), acceptingVerdict(), acceptingVerdict())
	producerBefore, _, err := fixture.store.Run(planStageState(t, fixture, "implementation").RunID)
	if err != nil {
		t.Fatal(err)
	}
	frozenBefore, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("the first performance froze no assignment: found=%v err=%v", found, err)
	}

	driveUntilSettled(t, fixture, first)
	if run, _, _ := fixture.store.Run(first); run.Disposition != Failed {
		t.Fatalf("the first reviewer run is %s/%s, want failed", run.Disposition, run.Reason)
	}
	fixture.reconcile(t) // settles the failure as a retry
	fixture.reconcile(t) // starts the retry
	retried := planStageState(t, fixture, "review")
	if retried.Generation != 1 || retried.RunID == "" || retried.RunID == first {
		t.Fatalf("the stage was not performed again under a new generation and run: %#v", retried)
	}
	if retried.AgentID != frozenBefore.Agent.ID {
		t.Fatalf("the retry's reviewer is %q, the frozen one was %q", retried.AgentID, frozenBefore.Agent.ID)
	}

	// The SAME frozen assignment, subject and independence - not a re-resolution.
	frozenAfter, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 1, "review")
	if err != nil || !found {
		t.Fatalf("the retry froze no assignment: found=%v err=%v", found, err)
	}
	before, _ := domain.CanonicalJSON(renewalScope(frozenBefore))
	after, _ := domain.CanonicalJSON(renewalScope(frozenAfter))
	if string(before) != string(after) {
		t.Fatal("the retry's assignment differs from the one the failed performance was frozen under")
	}
	if len(frozenAfter.Context.UpstreamOutputs) == 0 || len(frozenBefore.Context.UpstreamOutputs) != len(frozenAfter.Context.UpstreamOutputs) {
		t.Fatalf("the upstream subject changed: %#v -> %#v", frozenBefore.Context.UpstreamOutputs, frozenAfter.Context.UpstreamOutputs)
	}
	for i, upstream := range frozenBefore.Context.UpstreamOutputs {
		if got := frozenAfter.Context.UpstreamOutputs[i]; got.Candidate != upstream.Candidate || got.Tree != upstream.Tree {
			t.Fatalf("the retry reviews %s/%s, the failed performance reviewed %s/%s", got.Candidate, got.Tree, upstream.Candidate, upstream.Tree)
		}
	}

	driveUntilSettled(t, fixture, retried.RunID)
	fixture.reconcile(t)
	done := planStageState(t, fixture, "review")
	if done.State != PlanStageCompleted || done.Review == nil || done.Review.Verdict != StageReviewAccepted {
		t.Fatalf("the retried reviewer's verdict did not settle the stage: %#v", done)
	}
	if len(reviewer.Reviewed) != 3 {
		t.Fatalf("the reviewer was invoked %d times, want 3 (two failed, one answered)", len(reviewer.Reviewed))
	}

	// THE IMPLEMENTER AND ITS CANDIDATE ARE UNTOUCHED.
	if runs := stageRuns(t, fixture, "implementation"); len(runs) != 1 {
		t.Fatalf("the implementer has %d runs, want the one it started with", len(runs))
	}
	producerAfter, _, err := fixture.store.Run(producerBefore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if producerAfter.Candidate != producerBefore.Candidate {
		t.Fatalf("the implementer's candidate moved: %+v -> %+v",
			producerBefore.Candidate, producerAfter.Candidate)
	}

	// RESTART AND REPLAY DO NOT DUPLICATE IT, and the cost is durable.
	for range 3 {
		fixture.reconcile(t)
		reborn := fixture.reconciler
		if _, err := reborn.Reconcile(context.Background(), fixture.plan.ID); err != nil {
			t.Fatal(err)
		}
	}
	if runs := stageRuns(t, fixture, "review"); len(runs) != 2 {
		t.Fatalf("the reviewer stage has %d runs after replay, want exactly 2", len(runs))
	}
	if n := settlements(t, fixture, "review", planStageInvalidated); n != 1 {
		t.Fatalf("the retry was recorded %d times, want once", n)
	}
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns != 3 {
		t.Fatalf("the plan charged %d child runs, want 3 (implementer, reviewer, its retry)", snapshot.Consumed.ChildRuns)
	}
}

func TestASecondFailedReviewerPerformanceLeavesTheStageFailed(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	reviewer, first := recoveryScenario(t, fixture,
		map[int]string{0: malformedVerdict, 1: malformedVerdict2, 2: malformedVerdict, 3: malformedVerdict2},
		acceptingVerdict())
	driveUntilSettled(t, fixture, first)
	fixture.reconcile(t)
	fixture.reconcile(t)
	second := planStageState(t, fixture, "review")
	if second.Generation != 1 || second.RunID == first {
		t.Fatalf("the retry did not start: %#v", second)
	}
	driveUntilSettled(t, fixture, second.RunID)
	for range 4 {
		fixture.reconcile(t)
	}

	final := planStageState(t, fixture, "review")
	if final.State != PlanStageFailed || final.Review != nil {
		t.Fatalf("the stage is %s with review %#v, want failed and no verdict invented", final.State, final.Review)
	}
	if runs := stageRuns(t, fixture, "review"); len(runs) != 2 {
		t.Fatalf("%d reviewer runs, want the original and exactly one retry", len(runs))
	}
	if len(reviewer.Reviewed) != 4 {
		t.Fatalf("the reviewer was invoked %d times, want 4 (two per run, never a third run)", len(reviewer.Reviewed))
	}
	if n := settlements(t, fixture, "review", planStageInvalidated); n != 1 {
		t.Fatalf("%d retries recorded, want exactly one", n)
	}
	if planStatus(t, fixture) == PlanStateCompleted {
		t.Fatal("a plan whose reviewer never answered reported completed")
	}
}

func TestARetryTheEnvelopeCannotAffordLeavesTheStageFailedWithItsRealFailure(t *testing.T) {
	// Two child runs: the implementer and the reviewer. There is no room for a
	// third, and a spent budget is never renewed.
	base := newPhase8Fixture(t)
	fixture := newPlanRunFixtureWith(t, base, closedLoopStages(), base.deps.ConfigDigest,
		domain.PlanBudgetEnvelope{MaxChildRuns: 2, MaxConcurrency: 3, MaxProviderInvocations: 12})
	reviewer, first := recoveryScenario(t, fixture,
		map[int]string{0: malformedVerdict, 1: malformedVerdict2}, acceptingVerdict(), acceptingVerdict(), acceptingVerdict())
	driveUntilSettled(t, fixture, first)
	for range 3 {
		fixture.reconcile(t)
	}
	final := planStageState(t, fixture, "review")
	if final.State != PlanStageFailed || final.Generation != 0 {
		t.Fatalf("the stage is %s at generation %d, want failed at 0 with no retry it cannot pay for", final.State, final.Generation)
	}
	if runs := stageRuns(t, fixture, "review"); len(runs) != 1 {
		t.Fatalf("%d reviewer runs, want 1", len(runs))
	}
	if len(reviewer.Reviewed) != 2 {
		t.Fatalf("the reviewer was invoked %d times, want only the original's 2", len(reviewer.Reviewed))
	}
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns > 2 {
		t.Fatalf("the plan exceeded its 2-child-run envelope: %d", snapshot.Consumed.ChildRuns)
	}
}

func TestABlockingVerdictIsAReviewNotAFailedInvocationAndIsNeverRetried(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	blocking := ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "review:needs-changes", Detail: "the change is incomplete"}}}
	_, first := recoveryScenario(t, fixture, nil, blocking)
	driveUntilSettled(t, fixture, first)
	for range 3 {
		fixture.reconcile(t)
	}
	if runs := stageRuns(t, fixture, "review"); len(runs) != 1 {
		t.Fatalf("a blocking verdict produced %d reviewer runs, want 1: it is a judgement, not a failed invocation", len(runs))
	}
	if n := settlements(t, fixture, "review", planStageInvalidated); n != 0 {
		t.Fatalf("a blocking verdict was recorded as a retry %d times", n)
	}
}

// The ONE eligibility rule, over every class the runtime can name.
func TestReviewerFailureRecoverableIsDecidedFromTheTypedClassAndNeverRenewsABudget(t *testing.T) {
	failed := func(reason string) EngineeringRun { return EngineeringRun{Disposition: Failed, Reason: reason} }
	retryable := []FailureClass{
		FailureReviewerProtocolIncomplete, FailureTransientProvider, FailureTransientInfrastructure,
		FailureProviderNoProgress, FailureConnectivity, FailureExecutionIncomplete,
	}
	for _, class := range retryable {
		for _, reason := range []string{"execution.invoke_attempts_exhausted", "execution.invoke_reviewer_protocol_correction_exhausted"} {
			if !reviewerFailureRecoverable(failed(reason), &ExecutionDiagnostic{FailureClass: class, Route: RouteStop}) {
				t.Errorf("%s ended by %s must earn the retry: it is a failed invocation, and the class (not the recorded route) decides", class, reason)
			}
		}
	}
	// Not an invocation failure, or a stop a person must clear, or a verdict.
	for _, class := range []FailureClass{
		FailureVerification, FailureCompileTest, FailureRunCancelled, FailureExecutionDeadlineExceeded,
		FailureProviderQuota, FailureProviderRateLimited, FailureAuthorityWait, FailureWorkspaceIntegrity, FailureUnknown,
	} {
		if reviewerFailureRecoverable(failed("execution.invoke_attempts_exhausted"), &ExecutionDiagnostic{FailureClass: class}) {
			t.Errorf("%s must not be retried: the runtime routes it to %s", class, RouteFailure(class))
		}
	}
	// A run ended by a run-level budget is at its approved limit whatever its
	// last class was.
	for reason := range budgetReasons {
		if reviewerFailureRecoverable(failed(reason), &ExecutionDiagnostic{FailureClass: FailureReviewerProtocolIncomplete}) {
			t.Errorf("a run that ended on %s must not be retried: that would renew the budget", reason)
		}
	}
	for _, bound := range []AttemptBound{BoundRunActiveWork, BoundAssuranceWork} {
		if reviewerFailureRecoverable(failed("execution.invoke_attempts_exhausted"), &ExecutionDiagnostic{FailureClass: FailureProviderNoProgress, Bound: bound}) {
			t.Errorf("an attempt ended by the %s bound must not be retried", bound)
		}
	}
	if reviewerFailureRecoverable(failed("execution.invoke_attempts_exhausted"), nil) {
		t.Error("a failure with no recorded class must not be retried: nothing proves it was an invocation failure")
	}
	if reviewerFailureRecoverable(EngineeringRun{Disposition: Waiting, Reason: ReasonGoalStateReached}, &ExecutionDiagnostic{FailureClass: FailureReviewerProtocolIncomplete}) {
		t.Error("a run that has not failed must not be retried")
	}
}

// Only a reviewer stage is eligible. A zero reconciler has no store, so a guard
// that let any other role through would panic reading one instead of answering.
func TestOnlyAReviewerStageIsEverRetried(t *testing.T) {
	for name, stage := range map[string]domain.PlanStage{
		"implementer": {ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer},
		"gate":        {ID: "assurance", Kind: domain.StageAssuranceGate},
	} {
		reason, err := PlanReconciler{}.retryFailedReviewer(domain.EngineeringPlan{}, stage, PlanStageProjection{}, PlanSnapshot{}, EngineeringRun{Disposition: Failed})
		if err != nil || reason != "" {
			t.Errorf("%s: retry=%q err=%v, want neither", name, reason, err)
		}
	}
}
