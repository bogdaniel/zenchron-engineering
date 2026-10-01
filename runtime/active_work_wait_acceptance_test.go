package runtime

// #83 acceptance: active engineering work != external wait != lifecycle age.
//
// The unit tests in budget_semantics_test.go pin the fold. These drive the real
// reconcile loop through a MULTI-DAY wait and prove the three clocks separate,
// that restart replays them exactly, and that the durable handoff record carries
// the same remaining active budget the reconciler enforces.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type activeWorkSnapshot struct {
	status  StatusReport
	budgets RemainingBudgets
}

func takeActiveWorkSnapshot(t *testing.T, fixture *phase8Fixture, runID string) activeWorkSnapshot {
	t.Helper()
	status, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.runtime.RequestAgentHandoff(runID, "claude", "acceptance probe for #83")
	var refused *AgentHandoffRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("handoff probe was not a typed refusal: %v", err)
	}
	return activeWorkSnapshot{status: status, budgets: record.Budgets}
}

// assertHandoffAgreesWithReconciler is acceptance proof 2 of the amended body:
// the durable handoff record's remaining wall budget is exactly the reconciler's
// WallLimit - activeElapsed, never a figure derived from lifecycle age.
func assertHandoffAgreesWithReconciler(t *testing.T, fixture *phase8Fixture, snap activeWorkSnapshot) {
	t.Helper()
	want := int64((fixture.deps.Budgets.WallLimit - snap.status.ActiveElapsed) / time.Second)
	if !snap.budgets.WallSeconds.Known || snap.budgets.WallSeconds.Remaining != want {
		t.Fatalf("handoff record carries wall_seconds %#v; reconciler active accounting leaves %ds (active %s, age %s)",
			snap.budgets.WallSeconds, want, snap.status.ActiveElapsed, snap.status.Elapsed)
	}
}

func waitAcceptanceFixture(t *testing.T) (*phase8Fixture, string) {
	t.Helper()
	return waitAcceptanceFixtureWithLifecycle(t, 0)
}

// waitAcceptanceFixtureWithLifecycle creates the run under a lifecycle
// deadline, which is frozen at creation (ADR-0003 B1).
func waitAcceptanceFixtureWithLifecycle(t *testing.T, lifecycle time.Duration) (*phase8Fixture, string) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	fixture.deps.Budgets.LifecycleDeadline = lifecycle
	registry := handoffRegistry(t)
	agent, err := registry.Agent("codex")
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.Agent, fixture.deps.Agents = agent, registry
	fixture.deps.Feedback = FeedbackPolicy{SelfLogins: []string{"zenchron-runtime"}}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.forge.ViewerActor = GitHubActor{Login: "zenchron-runtime", ID: 99}
	fixture.forge.Permissions["maintainer"] = PermissionWrite
	// The fixture's default provider mutates the candidate with the same
	// fixed content on every invocation, so a resumed invocation given the
	// review comment admitted below would leave the tree byte-identical to
	// what it already published. That is a legitimate no-change case (#376:
	// "a candidate can already satisfy feedback based on a misunderstanding"),
	// and this test is about the three clocks surviving the wait, not about
	// feedback remediation - so the provider states that explicitly through
	// the real resolution channel instead of leaving it to be inferred from
	// a clean return, which #376 forbids.
	fixture.provider.resolveFeedback = true

	runID := fixture.start()
	for pass := 0; pass < 12; pass++ {
		outcome := fixture.reconcile(runID)
		if outcome.Disposition == Waiting && externalWaitReasons[outcome.Reason] {
			if fixture.state(runID).projection.PullRequest == nil {
				t.Fatalf("run waits on %q without a published pull request", outcome.Reason)
			}
			return fixture, runID
		}
		if terminalDisposition(outcome.Disposition) {
			t.Fatalf("run ended before waiting for review: %s/%s", outcome.Disposition, outcome.Reason)
		}
	}
	t.Fatal("run never reached an external wait")
	return nil, ""
}

// TestAMultiDayReviewWaitSeparatesTheThreeClocks is acceptance 1-4, 7 and 8,
// plus the handoff-agreement proof, on the production reconcile path.
func TestAMultiDayReviewWaitSeparatesTheThreeClocks(t *testing.T) {
	fixture, runID := waitAcceptanceFixture(t)

	// From here the clock moves only when the test says so, so every interval
	// below is exact. Within a tick a poll therefore costs zero, isolating the
	// wait itself (TestAWaitSurvivesThePollsTakenDuringIt covers polls that
	// cost time).
	fixture.clock.step = 0

	// Acceptance 1: bounded work was done and was charged.
	before := takeActiveWorkSnapshot(t, fixture, runID)
	if before.status.ActiveElapsed <= 0 {
		t.Fatalf("the work before publication consumed no active budget: %+v", before.status)
	}
	assertHandoffAgreesWithReconciler(t, fixture, before)

	// Acceptance 2-3: three days pass awaiting review. The supervisor keeps
	// polling, which is the deduplicated journal production writes (one
	// run.waiting, then operation events). Active must not move by a single
	// nanosecond while wait and age grow by exactly the span.
	const span = 72 * time.Hour
	for tick := 0; tick < 18; tick++ {
		fixture.clock.advance(span / 18)
		if outcome := fixture.reconcile(runID); outcome.Disposition != Waiting {
			t.Fatalf("tick %d: a run awaiting review left its wait: %s/%s", tick, outcome.Disposition, outcome.Reason)
		}
	}
	during := takeActiveWorkSnapshot(t, fixture, runID)
	if during.status.ActiveElapsed != before.status.ActiveElapsed {
		t.Fatalf("a %s review wait changed active consumption: %s -> %s", span, before.status.ActiveElapsed, during.status.ActiveElapsed)
	}
	if got := during.status.ExternalWaitElapsed - before.status.ExternalWaitElapsed; got != span {
		t.Fatalf("external wait advanced by %s over a %s wait", got, span)
	}
	if got := during.status.Elapsed - before.status.Elapsed; got != span {
		t.Fatalf("lifecycle age advanced by %s over a %s wait", got, span)
	}
	// Acceptance 8: status reports the three clocks as distinct numbers (the
	// wait field is derived as age - active, so their sum is not re-asserted).
	if during.status.Elapsed <= fixture.deps.Budgets.WallLimit {
		t.Fatalf("the fixture did not outlive its wall budget in calendar time (%s <= %s); nothing was proven", during.status.Elapsed, fixture.deps.Budgets.WallLimit)
	}
	// The handoff record made DURING the wait carries the same remaining
	// active budget as before it, and agrees with the reconciler.
	assertHandoffAgreesWithReconciler(t, fixture, during)
	if during.budgets != before.budgets {
		t.Fatalf("a wait changed the carried budgets:\n before %#v\n after  %#v", before.budgets, during.budgets)
	}
	// External waits do not reset attempt counters. The carried budgets above
	// already pin execution/remediation/assurance/continuations; polls may add
	// observation counts, but nothing may go backwards.
	for kind, n := range before.status.Attempts {
		if during.status.Attempts[kind] < n {
			t.Fatalf("attempt counter %s was reset by the wait: %d -> %d", kind, n, during.status.Attempts[kind])
		}
	}

	// Acceptance 7: a restart mid-wait replays every counter exactly from
	// durable state. Nothing in memory survives reopen.
	reopen(t, fixture)
	restarted := takeActiveWorkSnapshot(t, fixture, runID)
	if restarted.status.ActiveElapsed != during.status.ActiveElapsed ||
		restarted.status.ExternalWaitElapsed != during.status.ExternalWaitElapsed ||
		restarted.status.Elapsed != during.status.Elapsed {
		t.Fatalf("restart changed the clocks: active %s->%s wait %s->%s age %s->%s",
			during.status.ActiveElapsed, restarted.status.ActiveElapsed,
			during.status.ExternalWaitElapsed, restarted.status.ExternalWaitElapsed,
			during.status.Elapsed, restarted.status.Elapsed)
	}
	if restarted.budgets != during.budgets {
		t.Fatalf("restart renewed the carried budgets:\n before %#v\n after  %#v", during.budgets, restarted.budgets)
	}
	assertHandoffAgreesWithReconciler(t, fixture, restarted)

	// Acceptance 4: review arrives and the SAME run resumes, with exactly the
	// remaining active budget it had before the wait began.
	number := fixture.state(runID).projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{{
		ID: 8301, Author: GitHubActor{Login: "maintainer", ID: 7},
		Body: UntrustedText("please add a doc comment"), CreatedAt: fixture.clock.Now(),
	}}
	if observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID); err != nil || observation.Admitted != 1 {
		t.Fatalf("review was not admitted: %+v %v", observation, err)
	}
	if got := fixture.runtime.remainingBudgets(fixture.state(runID)); got.WallSeconds != before.budgets.WallSeconds {
		t.Fatalf("resuming after the wait had %#v active wall budget, want exactly %#v", got.WallSeconds, before.budgets.WallSeconds)
	}
	// Let the resumed work take real (injected) time, then prove it and only
	// it was charged: active grows by at most the span the resume occupied,
	// and the remaining budget is exactly the pre-wait remainder minus it.
	fixture.clock.step = time.Second
	invocations := len(fixture.provider.requests)
	resumeStart := fixture.clock.Now()
	outcome := fixture.reconcile(runID)
	resumeEnd := fixture.clock.Now()
	if outcome.Disposition == Failed {
		t.Fatalf("a run resumed after a %s review wait was failed: %s", span, outcome.Reason)
	}
	if len(fixture.provider.requests) <= invocations {
		t.Fatal("the review did not resume the same run's worker")
	}
	fixture.clock.step = 0
	after := takeActiveWorkSnapshot(t, fixture, runID)
	resumed := after.status.ActiveElapsed - before.status.ActiveElapsed
	if resumed <= 0 || resumed > resumeEnd.Sub(resumeStart) {
		t.Fatalf("resumed work charged %s active; the resume occupied %s", resumed, resumeEnd.Sub(resumeStart))
	}
	if got, want := after.budgets.WallSeconds.Remaining, int64((fixture.deps.Budgets.WallLimit-before.status.ActiveElapsed-resumed)/time.Second); got != want {
		t.Fatalf("remaining after resume = %ds, want pre-wait remainder minus resumed work = %ds", got, want)
	}
	assertHandoffAgreesWithReconciler(t, fixture, after)
}

// TestAClosedExternalWaitStaysExcluded pins the CLOSED-wait branch of the
// fold: once a later disposition ends the wait, its idle span must remain
// excluded rather than being charged retroactively.
func TestAClosedExternalWaitStaysExcluded(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	work := operationPair("exec", start.Add(time.Minute), 10*time.Minute)
	waitAt := start.Add(11 * time.Minute)
	closeAt := waitAt.Add(72 * time.Hour)
	closers := map[string]EngineeringEvent{
		"internal wait": waitEvent(closeAt, "operation_unavailable"),
		"terminal":      {Type: EventRunCompleted, OccurredAt: closeAt},
	}
	for name, closer := range closers {
		events := append(append([]EngineeringEvent(nil), work...), waitEvent(waitAt, "execution_provider_quota"), closer)
		state := &runState{run: EngineeringRun{CreatedAt: start}, events: events}
		if got := state.activeElapsed(closeAt); got != 11*time.Minute {
			t.Fatalf("%s: a closed 72h external wait was charged: active %s, want 11m", name, got)
		}
	}
}

// TestProviderWaitsDoNotSpendActiveWork is acceptance 5: the provider
// account/quota/rate-limit waits pause active work exactly as review does, and
// the budget gate and handoff arithmetic both honour it across days.
func TestProviderWaitsDoNotSpendActiveWork(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"execution_provider_account_unavailable", "execution_provider_quota", "execution_provider_rate_limited", "execution_provider_unavailable"} {
		work := operationPair("exec", start.Add(time.Minute), 10*time.Minute)
		events := append(work, waitEvent(start.Add(11*time.Minute), reason))
		clock := &steppingClock{at: start.Add(11 * time.Minute)}
		budgets := RunBudgets{WallLimit: 30 * time.Minute}

		state := conditionsFixture(start, clock, budgets, events)
		atWait := state.activeElapsed(clock.at)
		clock.advance(3 * 24 * time.Hour)
		state = conditionsFixture(start, clock, budgets, events)
		if got := state.activeElapsed(clock.at); got != atWait {
			t.Fatalf("%s: a three-day wait moved active work %s -> %s", reason, atWait, got)
		}
		if disposition, why := state.conditions(); disposition == Failed {
			t.Fatalf("%s: a three-day provider wait failed the run: %s", reason, why)
		}
		// The same law the handoff record uses.
		if got := state.rt.remainingBudgets(state).WallSeconds; got.Remaining != int64((budgets.WallLimit-atWait)/time.Second) {
			t.Fatalf("%s: handoff wall budget %#v disagrees with active accounting %s", reason, got, atWait)
		}
	}
}

// TestALifecycleDeadlineExpiresIndependentlyOfActiveWork is acceptance 6 on
// the production path: after the same multi-day review wait, an operator
// lifecycle deadline ends the run although active budget remains.
func TestALifecycleDeadlineExpiresIndependentlyOfActiveWork(t *testing.T) {
	fixture, runID := waitAcceptanceFixtureWithLifecycle(t, 48*time.Hour)
	fixture.clock.step = 0
	fixture.clock.advance(72 * time.Hour)
	fixture.runtime = fixture.newRuntime(fixture.deps)

	status, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveElapsed >= fixture.deps.Budgets.WallLimit {
		t.Fatalf("active budget was exhausted, so this would not isolate the lifecycle deadline: %s", status.ActiveElapsed)
	}
	if outcome := fixture.reconcile(runID); outcome.Disposition != Failed || outcome.Reason != "run_lifecycle_deadline_exhausted" {
		t.Fatalf("a configured lifecycle deadline did not end the run: %s/%s", outcome.Disposition, outcome.Reason)
	}
}
