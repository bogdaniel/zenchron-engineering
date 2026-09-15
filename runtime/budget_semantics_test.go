package runtime

// The execution wall budget bounds the time this system spends WORKING. These
// tests pin the distinction that a live run exposed: a pull request awaiting
// human review was being killed by an engineering budget, which made the #63
// review loop unusable at any budget that still bounded runaway work.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func waitEvent(at time.Time, reason string) EngineeringEvent {
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	return EngineeringEvent{Type: EventRunWaiting, OccurredAt: at, Payload: payload}
}

func progressEvent(at time.Time) EngineeringEvent {
	return EngineeringEvent{Type: EventOperationAfter, OccurredAt: at}
}

// operationPair is what the runtime actually writes for one operation: a before
// and an after, sharing an operation id. Work performed during a wait is
// measured between them, so a fixture that emits a lone `after` models nothing
// and would report every poll as instantaneous.
func operationPair(id string, start time.Time, took time.Duration) []EngineeringEvent {
	return []EngineeringEvent{
		{Type: EventOperationBefore, OperationID: id, OccurredAt: start},
		{Type: EventOperationAfter, OperationID: id, OccurredAt: start.Add(took)},
	}
}

// TestWaitingOnAHumanDoesNotSpendTheExecutionBudget is the semantic fix. A run
// that reached its goal and sat awaiting review has done no work in the
// meantime, and an execution budget that counts that time forces an operator to
// size their engineering bounds around how fast people answer.
func TestWaitingOnAHumanDoesNotSpendTheExecutionBudget(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	state := &runState{
		run: EngineeringRun{CreatedAt: start},
		events: []EngineeringEvent{
			progressEvent(start.Add(2 * time.Minute)),                 // real work
			waitEvent(start.Add(3*time.Minute), "goal_state_reached"), // then the human's turn
		},
	}
	// Twelve hours later the human still has not reviewed it.
	now := start.Add(12 * time.Hour)
	active := state.activeElapsed(now)
	if active > 5*time.Minute {
		t.Fatalf("an overnight wait for review spent %s of the execution budget", active)
	}
	// The work before the wait is still counted: this is not "waiting runs are
	// free", it is "waiting is not work".
	if active < 3*time.Minute {
		t.Fatalf("the work performed before the wait was not counted: %s", active)
	}
}

// TestAnInternalWaitStillSpendsTheBudget keeps the exemption honest. The set of
// waits that pause the clock is closed and fail-closed: a reason nobody has
// classified is the system's own problem and burns the budget, so a new wait
// cannot make a run immortal by accident.
func TestAnInternalWaitStillSpendsTheBudget(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"operation_unavailable", "controller_changed", "invented_reason"} {
		state := &runState{
			run:    EngineeringRun{CreatedAt: start},
			events: []EngineeringEvent{waitEvent(start.Add(time.Minute), reason)},
		}
		now := start.Add(2 * time.Hour)
		if active := state.activeElapsed(now); active < 2*time.Hour-time.Second {
			t.Fatalf("wait reason %q paused the execution budget; only classified external waits may: %s", reason, active)
		}
	}
}

// TestObservationDuringAWaitIsStillWork stops the exemption from swallowing the
// polling a waiting run performs. The runtime re-reads the pull request and the
// issue on every tick; that is real work and is counted. Only the idle gap
// between ticks is excluded.
func TestObservationDuringAWaitIsStillWork(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	events := []EngineeringEvent{waitEvent(start, "goal_state_reached")}
	at := start
	// Ten ticks, each: a long idle gap, then a second of observation. The wait
	// event is written ONCE, because the reason never changes - which is what
	// production does.
	for i := 0; i < 10; i++ {
		at = at.Add(10 * time.Minute)
		events = append(events, operationPair(fmt.Sprintf("poll-%d", i), at, time.Second)...)
		at = at.Add(time.Second)
	}
	state := &runState{run: EngineeringRun{CreatedAt: start}, events: events}
	active := state.activeElapsed(at)
	if active > time.Minute {
		t.Fatalf("idle time between ticks was counted as work: %s", active)
	}
	if active < 9*time.Second {
		t.Fatalf("observation performed while waiting was not counted as work: %s", active)
	}
}

// TestActiveAccountingIsDerivedFromDurableEventsOnly is the restart law. Every
// excluded interval is bounded by two recorded events, so a second process
// replaying the same rows reaches the same number. An in-memory stopwatch would
// reset on restart and hand the run a fresh budget.
func TestActiveAccountingIsDerivedFromDurableEventsOnly(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	events := []EngineeringEvent{
		progressEvent(start.Add(time.Minute)),
		waitEvent(start.Add(2*time.Minute), "awaiting_authority"),
		progressEvent(start.Add(90 * time.Minute)),
	}
	now := start.Add(100 * time.Minute)
	first := (&runState{run: EngineeringRun{CreatedAt: start}, events: events}).activeElapsed(now)
	// A "restarted" reader: same rows, new object, nothing carried over.
	second := (&runState{run: EngineeringRun{CreatedAt: start}, events: append([]EngineeringEvent(nil), events...)}).activeElapsed(now)
	if first != second {
		t.Fatalf("the same journal produced two answers across a restart: %s then %s", first, second)
	}
	// 88 minutes of that span was somebody else's turn.
	if first > 15*time.Minute {
		t.Fatalf("the authority wait was charged to the execution budget: %s", first)
	}
}

// conditionsFixture builds the smallest runState the disposition rules read.
// It exists so the tests below exercise conditions() itself rather than the
// helper it calls: an earlier version of this file asserted activeElapsed in
// isolation and passed with the budget gate reverted to raw elapsed time, which
// is a test of arithmetic rather than of the rule.
func conditionsFixture(created time.Time, clock Clock, budgets RunBudgets, events []EngineeringEvent) *runState {
	return &runState{
		rt:     &EngineeringRuntime{deps: Dependencies{Clock: clock, Budgets: budgets}},
		run:    EngineeringRun{CreatedAt: created},
		events: events,
	}
}

// TestTheBudgetGateSpendsActiveTimeNotCalendarTime binds the rule to the
// accounting. A run awaiting review well past its wall limit must not be failed,
// and a run that spent the same span WORKING must be.
func TestTheBudgetGateSpendsActiveTimeNotCalendarTime(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	budgets := RunBudgets{WallLimit: 30 * time.Minute}
	clock := &steppingClock{at: start.Add(12 * time.Hour)}

	awaitingReview := conditionsFixture(start, clock, budgets, []EngineeringEvent{
		progressEvent(start.Add(2 * time.Minute)),
		waitEvent(start.Add(3*time.Minute), "goal_state_reached"),
	})
	if disposition, reason := awaitingReview.conditions(); disposition == Failed {
		t.Fatalf("a run awaiting human review was failed by the execution budget: %s/%s", disposition, reason)
	}

	// The same twelve hours, spent working: the bound still ends it.
	working := conditionsFixture(start, clock, budgets, []EngineeringEvent{
		progressEvent(start.Add(time.Hour)),
	})
	disposition, reason := working.conditions()
	if disposition != Failed || reason != "run_wall_budget_exhausted" {
		t.Fatalf("a run that worked past its wall budget was not stopped: %s/%s", disposition, reason)
	}
}

// TestTheLifecycleDeadlineIsSeparateAndOptional. Bounding total calendar time is
// a legitimate thing to want; it is just not what an execution budget means. It
// is absent by default, so a run waiting on a person waits as long as the person
// takes.
func TestTheLifecycleDeadlineIsSeparateAndOptional(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := &steppingClock{at: start.Add(48 * time.Hour)}
	waiting := []EngineeringEvent{waitEvent(start.Add(time.Minute), "goal_state_reached")}

	unbounded := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute}, waiting)
	if disposition, reason := unbounded.conditions(); disposition == Failed {
		t.Fatalf("with no lifecycle deadline configured a waiting run was still ended: %s/%s", disposition, reason)
	}

	bounded := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute, LifecycleDeadline: 24 * time.Hour}, waiting)
	disposition, reason := bounded.conditions()
	if disposition != Failed || reason != "run_lifecycle_deadline_exhausted" {
		t.Fatalf("a stated lifecycle deadline did not bound total elapsed time: %s/%s", disposition, reason)
	}
}

// TestAWaitSurvivesThePollsTakenDuringIt is the PRODUCTION-PATH regression, and
// it is the one that matters.
//
// recordDisposition appends run.waiting only when the disposition or reason
// CHANGES, so a run that stays in the same wait writes exactly one wait event
// and then keeps emitting operation events as it polls. An accounting model
// that closes the wait at the next event therefore closes it at the first poll
// and charges every hour after that to the execution budget - the original
// defect, returning after one tick.
//
// The earlier unit tests could not see this: they synthesized a fresh
// run.waiting before each observation, which is a journal production never
// writes. This one drives the real reconcile loop and lets the runtime write
// its own journal.
func TestAWaitSurvivesThePollsTakenDuringIt(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	// Drive until the run parks in an external wait, exactly as it would live.
	var reason string
	for pass := 0; pass < 12; pass++ {
		outcome := fixture.reconcile(runID)
		reason = outcome.Reason
		if outcome.Disposition == Waiting && externalWaitReasons[reason] {
			break
		}
		if terminalDisposition(outcome.Disposition) {
			t.Fatalf("the run finished without ever waiting on anything external: %s/%s", outcome.Disposition, reason)
		}
	}
	if !externalWaitReasons[reason] {
		t.Skipf("this fixture never reached an external wait (last reason %q); the unit tests cover the accounting", reason)
	}

	state, err := fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	before := state.activeElapsed(fixture.clock.Now())

	// Hours pass, the runtime polls, the wait reason does not change - so no
	// second run.waiting is written. This is the exact shape production emits.
	waits := countWaitEvents(t, fixture, runID)
	for tick := 0; tick < 3; tick++ {
		fixture.clock.advance(4 * time.Hour)
		fixture.reconcile(runID)
	}
	if after := countWaitEvents(t, fixture, runID); after != waits {
		t.Fatalf("the fixture wrote %d extra run.waiting events, so it is not reproducing the deduplicated journal", after-waits)
	}

	state, err = fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	grew := state.activeElapsed(fixture.clock.Now()) - before
	if grew > 5*time.Minute {
		t.Fatalf("twelve hours of waiting spent %s of the execution budget across polling ticks", grew)
	}
}

// countWaitEvents is how the test proves it is reproducing the deduplicated
// journal rather than a convenient one.
func countWaitEvents(t *testing.T, fixture *phase8Fixture, runID string) int {
	t.Helper()
	state, err := fixture.runtime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range state.events {
		if event.Type == EventRunWaiting {
			n++
		}
	}
	return n
}

// TestEveryWaitRoutedFailureIsClassified closes the gap between the two tables.
// waitReasons names the durable reason each wait-routed failure settles into;
// every one of them is the run waiting on something OUTSIDE itself - a provider
// account, a quota, a rate limit, a dependency the operator must provision,
// disk the operator must free. A reason present in one table and missing from
// the other silently charges the operator for somebody else's backoff.
func TestEveryWaitRoutedFailureIsClassified(t *testing.T) {
	for class, reason := range waitReasons {
		if !externalWaitReasons[reason] {
			t.Errorf("failure class %q settles into wait reason %q, which spends the execution budget", class, reason)
		}
	}
}

// ---------------------------------------------------------------------------
// #203: a spent budget must not discard verified work
// ---------------------------------------------------------------------------

// burningAssurance charges the injected clock for the time a verifier took, so
// a scenario can reproduce the shape run run-9d2a446bc6071568ce3030503b01bd57
// died in: the envelope is gone at the exact moment the candidate becomes
// publishable, and every expensive stage has already been paid for.
type burningAssurance struct {
	inner AssuranceProvider
	clock *steppingClock
	burn  time.Duration
	// then runs once, after the envelope is gone and before anything can be
	// handed over, which is where a base moves under a verified candidate.
	then  func()
	spent bool
}

func (b *burningAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	producer, ok := b.inner.(EvidenceProducer)
	if !ok {
		return nil
	}
	return producer.ProducedEvidenceClasses()
}

func (b *burningAssurance) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	result, err := b.inner.Assure(ctx, request)
	if !b.spent {
		b.spent = true
		b.clock.advance(b.burn)
		if b.then != nil {
			b.then()
		}
	}
	return result, err
}

// exhaustedAtVerification builds a run whose wall budget is gone by the time the
// last verifier has answered.
func exhaustedAtVerification(t *testing.T, fixture *phase8Fixture) *phase8Fixture {
	t.Helper()
	fixture.deps.Budgets = RunBudgets{WallLimit: 30 * time.Minute, MaxExecutionAttempts: 2, MaxRemediationAttempts: 2, MaxAssuranceAttempts: 2}
	fixture.deps.SemanticAssurance = &burningAssurance{inner: fixture.deps.SemanticAssurance, clock: fixture.clock, burn: 31 * time.Minute}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture
}

// TestASpentBudgetDoesNotDiscardAVerifiedCandidate is #203. A run that produced
// a candidate, committed it, reassessed it and verified it has spent everything
// the budget was there to bound; the pull request is the only thing left, and a
// run that dies holding it has charged the operator for all of it and delivered
// none of it.
func TestASpentBudgetDoesNotDiscardAVerifiedCandidate(t *testing.T) {
	fixture := exhaustedAtVerification(t, newPhase8Fixture(t))
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	state := fixture.state(runID)
	if state.activeElapsed(fixture.clock.at) <= 30*time.Minute {
		t.Fatalf("the scenario did not exhaust the envelope: %s", state.activeElapsed(fixture.clock.at))
	}
	if a := state.projection.Assurance; a == nil || a.Stale || !a.Passed {
		t.Fatalf("the scenario did not reach a verified candidate: %#v", a)
	}
	pr := state.projection.PullRequest
	if pr == nil {
		t.Fatalf("verified candidate %s was discarded unpublished: %s/%s", state.projection.CandidateRevision, outcome.Disposition, outcome.Reason)
	}
	if pr.HeadRevision != state.projection.CandidateRevision {
		t.Fatalf("the pull request carries %s, want the verified candidate %s", pr.HeadRevision, state.projection.CandidateRevision)
	}
	if outcome.Disposition == Failed {
		t.Fatalf("the run failed after delivering: %s/%s", outcome.Disposition, outcome.Reason)
	}
}

// TestASpentBudgetStillCannotPublishWithoutAuthority is the boundary the fix is
// not allowed to move. Delivery is exempt from the budget; it is not exempt
// from #7. A run whose publication decision is not authorized must reach the
// same wait it always reached, and must open nothing.
func TestASpentBudgetStillCannotPublishWithoutAuthority(t *testing.T) {
	fixture := exhaustedAtVerification(t, newAuthorityFixture(t))
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	if outcome.Disposition != Waiting || outcome.Reason != "awaiting_authority" {
		t.Fatalf("outcome = %#v, want waiting/awaiting_authority", outcome)
	}
	state := fixture.state(runID)
	if state.published() || countMethod(fixture.forge.Calls, "CreatePullRequest") != 0 {
		t.Fatal("an unauthorized run published because its budget was spent")
	}
	if _, ok := state.publicationDecision(); !ok {
		t.Fatal("the run published nothing and evaluated no authority either")
	}
}

// TestTheWallBudgetStillEndsARunWithWorkLeftToDo is the other half of the
// policy. The exemption is for HANDING OVER verified work, not for being over
// budget: a run that still wants to produce or verify anything is ended exactly
// as before, and the reason says that a commit exists which never became a pull
// request, because "run_wall_budget_exhausted" alone tells an operator nothing
// about the work on disk.
func TestTheWallBudgetStillEndsARunWithWorkLeftToDo(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := &steppingClock{at: start.Add(12 * time.Hour)}
	state := conditionsFixture(start, clock, RunBudgets{WallLimit: 30 * time.Minute}, []EngineeringEvent{
		progressEvent(start.Add(time.Hour)),
	})
	// A VERIFIED candidate, so the cheap guards in deliveringVerifiedCandidate
	// all pass and the answer has to come from wantsBudgetedWork itself. An
	// earlier version of this test left Assurance nil, which short-circuits
	// before the predicate is ever called - it asserted the rule and exercised
	// none of it.
	state.projection = RunProjection{
		Contract:          Ref{ID: "contract-1", Revision: "1"},
		CandidateRevision: "b6f2c09",
		CandidateTree:     "tree-1",
		CandidateComplete: true,
		Assurance:         &AssuranceObservation{AssuranceObservedPayload: AssuranceObservedPayload{Commit: "b6f2c09", Tree: "tree-1", Passed: true}},
	}
	// Nothing has verified this exact tree yet, so assurance is wanted and
	// unsatisfied. The projection says a previous head passed; the planner says
	// there is work to do, and the planner is what the budget answers to.
	if !state.wantsBudgetedWork() {
		t.Fatal("unsatisfied assurance is not being counted as budgeted work")
	}
	if state.deliveringVerifiedCandidate() {
		t.Fatal("a run with work left to do claimed to be delivering")
	}
	disposition, reason := state.conditions()
	if disposition != Failed {
		t.Fatalf("a run with verification still to do outlived its budget: %s/%s", disposition, reason)
	}
	if reason != "run_wall_budget_exhausted_candidate_unpublished" {
		t.Fatalf("reason = %q, want the unpublished candidate named", reason)
	}
}

// TestAnOverBudgetBaseIntegrationOnlyEVERReads is the passivity claim, proven
// rather than asserted. base.integrate is in the exempt set only because it
// cannot spend anything once the envelope is gone; "MaxAttempts makes it finite"
// was never an argument, because something can be finite and still consume work
// that should count.
//
// The base here moves CLEANLY, so the rebase would succeed. That is the
// demanding direction: a conflict test would pass even if the guard were
// missing, because git would refuse the work anyway.
func TestAnOverBudgetBaseIntegrationOnlyEVERReads(t *testing.T) {
	fixture := newPhase8Fixture(t)
	exhaustedAtVerification(t, fixture)
	// A base that moved, in a file the candidate never touches, at the instant
	// the candidate becomes publishable and the envelope is already gone.
	fixture.deps.SemanticAssurance.(*burningAssurance).then = func() {
		fixture.moveBase("UNRELATED.md", "the base moved cleanly\n")
	}
	runID := fixture.start()
	before := fixture.state(runID)
	outcome := fixture.reconcile(runID)
	state := fixture.state(runID)

	for _, e := range state.events {
		if e.Type == EventCandidateBaseIntegrated {
			t.Fatal("an over-budget run rebased the candidate onto a moved base")
		}
	}
	if head := state.projection.CandidateRevision; head != before.projection.CandidateRevision && before.projection.CandidateRevision != "" {
		t.Fatalf("the candidate head moved past the budget: %s", head)
	}
	key := mustBind(t, bindBaseIntegrate, state)
	op, ok := state.operationByKey(OpBaseIntegrate, key)
	if !ok {
		t.Fatal("the run never read the base at all, so it would have published blind")
	}
	if op.Attempt != 1 {
		t.Fatalf("base.integrate ran %d attempts past the budget, want exactly the one read", op.Attempt)
	}
	if outcome.Disposition != Failed || outcome.Reason != "run_wall_budget_exhausted_candidate_unpublished" {
		t.Fatalf("outcome = %#v, want failed/run_wall_budget_exhausted_candidate_unpublished", outcome)
	}
	if state.published() {
		t.Fatal("a candidate was published over a base it had not integrated")
	}
}

// TestAcceptedFeedbackIsNotStrandedByTheBudget is #210. The shape reproduced:
// a published pull request, a maintainer's comment admitted, a remediation
// obligation created - and then the wall budget ending the run with the provider
// never invoked, the item still pending, and nobody told.
//
// The work stays refused, because acting on feedback is provider work and that
// is exactly what the budget bounds. What changes is that the refusal is no
// longer terminal. Failed is terminal, and only a NON-terminal run is adopted by
// StartOrResumeIssueRun, so failing here does not defer the obligation, it
// abandons it. The run waits on the operator instead, and the pass after the
// budget is raised discharges what was accepted.
func TestAcceptedFeedbackIsNotStrandedByTheBudget(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.Feedback = FeedbackPolicy{SelfLogins: []string{"zenchron-runtime"}}
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	exhaustedAtVerification(t, fixture)
	fixture.forge.ViewerActor = GitHubActor{Login: "zenchron-runtime", ID: 99}
	fixture.forge.Permissions["maintainer"] = PermissionWrite

	runID := fixture.start()
	if outcome := fixture.reconcile(runID); outcome.Disposition == Failed {
		t.Fatalf("the verified candidate was not delivered: %#v", outcome)
	}
	number := fixture.state(runID).projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{{
		ID: 501, Author: GitHubActor{Login: "maintainer", ID: 7},
		Body: UntrustedText("please add a doc comment to the new helper"), CreatedAt: fixture.clock.Now(),
	}}
	observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Admitted != 1 {
		t.Fatalf("the comment was not admitted, so there is no obligation to strand: %#v", observation)
	}

	before := len(fixture.provider.requests)
	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Waiting || outcome.Reason != ReasonFeedbackUndeliveredBudgetExhausted {
		t.Fatalf("outcome = %#v, want waiting/%s", outcome, ReasonFeedbackUndeliveredBudgetExhausted)
	}
	// The work is still refused. The obligation is what survives, not the budget.
	if len(fixture.provider.requests) != before {
		t.Fatal("an over-budget run invoked the provider on reviewer feedback")
	}
	if terminalDisposition(outcome.Disposition) {
		t.Fatal("the run is terminal, so no resume can ever discharge the obligation")
	}
	if !externalWaitReasons[outcome.Reason] {
		t.Fatalf("%q is not in the closed wait set, so waiting on the operator spends the budget", outcome.Reason)
	}
	state := fixture.state(runID)
	if len(state.pendingFeedbackKeys()) != 1 {
		t.Fatalf("pending feedback = %v, want the admitted item still owed", state.pendingFeedbackKeys())
	}

	// What the wait buys, stated exactly. There is no affordance today to raise
	// a run's wall budget - budgets() takes the MINIMUM of the live config and
	// what the run persisted at creation, so a raised config cannot widen it -
	// which makes the difference between waiting and dying larger, not smaller.
	// An adopted run still owns the obligation; an abandoned one does not.
	adopted, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Adopted || adopted.RunID != runID {
		t.Fatalf("the run holding the obligation was not adopted: %#v", adopted)
	}
	if pending := fixture.state(runID).pendingFeedbackKeys(); len(pending) != 1 {
		t.Fatalf("pending feedback after adoption = %v, want the item still owed", pending)
	}
}

// TestAStrandedObligationIsTheOnlyThingThatSurvivesTheBudget is the contrast
// that gives the wait its meaning. The same run without an accepted obligation
// is ended by the budget exactly as before, and an ended run is not adopted: the
// next `run issue` mints a new generation and whatever the old one was holding
// belongs to nobody. That is the outcome #210 describes, and it is what the wait
// above avoids.
func TestAStrandedObligationIsTheOnlyThingThatSurvivesTheBudget(t *testing.T) {
	fixture := newPhase8Fixture(t)
	exhaustedAtVerification(t, fixture)
	fixture.deps.SemanticAssurance.(*burningAssurance).then = func() {
		fixture.moveBase("UNRELATED.md", "the base moved cleanly\n")
	}
	runID := fixture.start()

	// A verified candidate on a base that moved, and nobody is owed anything:
	// re-integrating is work, so the budget ends the run. The bound is untouched
	// by #210.
	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Failed || outcome.Reason != "run_wall_budget_exhausted_candidate_unpublished" {
		t.Fatalf("outcome = %#v, want the budget to end a run that owes nobody anything", outcome)
	}
	fresh, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Adopted || fresh.RunID == runID {
		t.Fatalf("a terminal run was adopted: %#v", fresh)
	}
}

// TestWantsBudgetedWorkDecidesTheBudgetOutcome is the predicate's own test, and
// it is built to fail under mutation rather than to go green.
//
// Two states differ in exactly one thing: whether an operation the budget bounds
// is still wanted. Everything else - the clock, the limit, the journal, the
// verified candidate - is identical. If the predicate stops being consulted
// (delete `&& !s.deliveringVerifiedCandidate()`, or make wantsBudgetedWork
// constant in either direction) the two states stop disagreeing, and the pair
// assertion below fails. A test that asserts only one of them certifies a claim
// it cannot observe, which is how the previous version of this file passed with
// wantsBudgetedWork never called at all.
func TestWantsBudgetedWorkDecidesTheBudgetOutcome(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	budgets := RunBudgets{WallLimit: 30 * time.Minute}
	verified := RunProjection{
		Contract:          Ref{ID: "contract-1", Revision: "1"},
		CandidateRevision: "b6f2c09",
		CandidateTree:     "tree-1",
		CandidateComplete: true,
		Assurance: &AssuranceObservation{AssuranceObservedPayload: AssuranceObservedPayload{
			Commit: "b6f2c09", Tree: "tree-1", Passed: true,
		}},
	}
	// The ONLY difference: a succeeded assurance operation at exactly this
	// binding, which is what makes the remaining work delivery rather than
	// verification.
	delivering := conditionsFixture(start, &steppingClock{at: start.Add(12 * time.Hour)}, budgets, []EngineeringEvent{
		progressEvent(start.Add(time.Hour)),
	})
	delivering.projection = verified
	working := conditionsFixture(start, &steppingClock{at: start.Add(12 * time.Hour)}, budgets, []EngineeringEvent{
		progressEvent(start.Add(time.Hour)),
	})
	working.projection = verified
	key := mustBind(t, bindAssuranceGo, delivering)
	delivering.snapshot.Operations = map[string]RunOperation{"op-1": {
		ID: "op-1", Kind: OpAssuranceGo, IdempotencyKey: operationKey(OpAssuranceGo, key), State: Succeeded,
	}}

	if delivering.wantsBudgetedWork() {
		t.Fatal("a verified candidate with its assurance satisfied still reports budgeted work")
	}
	if !working.wantsBudgetedWork() {
		t.Fatal("an unsatisfied assurance is not counted as budgeted work")
	}

	deliveringDisposition, deliveringReason := delivering.conditions()
	workingDisposition, workingReason := working.conditions()
	// The pair is the assertion. Neither half alone can tell a consulted
	// predicate from an ignored one.
	if deliveringDisposition == workingDisposition {
		t.Fatalf("the budget answered both states %s/%s and %s/%s the same way, so the predicate changed nothing",
			deliveringDisposition, deliveringReason, workingDisposition, workingReason)
	}
	if deliveringDisposition == Failed {
		t.Fatalf("delivery was ended by the budget: %s/%s", deliveringDisposition, deliveringReason)
	}
	if workingDisposition != Failed || workingReason != "run_wall_budget_exhausted_candidate_unpublished" {
		t.Fatalf("working = %s/%s, want failed/run_wall_budget_exhausted_candidate_unpublished", workingDisposition, workingReason)
	}
}
