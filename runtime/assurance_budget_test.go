package runtime

// #640: a successful full-suite verification must not spend the execution
// allowance, and assurance must still be bounded by an allowance of its own.
//
// Run run-de5b6172075e37120b6abd30cb907e12 executed for 8m11s, verified for
// 43m09s and SUCCEEDED, then failed run_wall_budget_exhausted because both
// were charged to one 30 minute budget. Every test drives the real reconciler
// on the injected stepping clock; no test waits on real time for a duration.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// timedAssurance spends fixture-clock time inside Assure, the way a real
// full-suite verification spends wall time. exhausted makes it behave as the
// real verifier does when its context ends: no verdict, a cancellation class.
type timedAssurance struct {
	clock *steppingClock
	spend time.Duration
	calls int
	// bounded records whether the attempt arrived with a deadline.
	bounded bool
}

func (a *timedAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return []domain.EvidenceClass{AssuranceEvidenceClass}
}

func (a *timedAssurance) Assure(ctx context.Context, _ AssuranceRequest) (AssuranceResult, error) {
	a.calls++
	_, a.bounded = ctx.Deadline()
	a.clock.advance(a.spend)
	result := AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: true}
	if ctx.Err() != nil {
		result.Passed, result.FailureClass = false, execution.CancellationClass(context.Cause(ctx))
		return result, ctx.Err()
	}
	return result, nil
}

func assuranceFixture(t *testing.T, budgets RunBudgets, spend time.Duration, steps ...wallStep) (*phase8Fixture, *wallProvider, *timedAssurance) {
	t.Helper()
	fixture, provider := wallFixture(t, budgets, steps...)
	verifier := &timedAssurance{clock: fixture.clock, spend: spend}
	fixture.useAssurance(verifier)
	return fixture, provider, verifier
}

func assuranceOperations(state *runState) (ops []RunOperation) {
	for _, op := range sortOperations(mapValues(state.snapshot.Operations)) {
		if op.Kind == OpAssuranceGo {
			ops = append(ops, op)
		}
	}
	return ops
}

// INVARIANT (before/after): execution 8m + a SUCCESSFUL 43m assurance does not
// spend the 30m execution ceiling, the run does not fail, and total active
// work is still reported as execution + assurance.
func TestSuccessfulLongAssuranceDoesNotSpendTheExecutionBudget(t *testing.T) {
	budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: time.Hour, MaxExecutionAttempts: 2}
	fixture, provider, verifier := assuranceFixture(t, budgets, 43*time.Minute,
		wallStep{mutate: true, complete: true, spend: 8 * time.Minute})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	if state.snapshot.Reason == ReasonRunWallBudgetExhausted || state.snapshot.Disposition == Failed {
		t.Fatalf("a successful verification failed the run: %s/%s", state.snapshot.Disposition, state.snapshot.Reason)
	}
	if verifier.calls == 0 || !verifier.bounded {
		t.Fatalf("assurance ran %d time(s), bounded=%v: the physical attempt must carry a deadline", verifier.calls, verifier.bounded)
	}
	now := fixture.clock.Now()
	execution, assurance, total := state.executionElapsed(now), state.assuranceElapsed(now), state.activeElapsed(now)
	if assurance < 43*time.Minute || assurance > 44*time.Minute {
		t.Fatalf("assurance meter = %s, want ~43m", assurance)
	}
	// The stepping clock adds a second per read, so "about 8m" is under 20m;
	// what matters is that it is nowhere near 8m + 43m.
	if execution >= 20*time.Minute {
		t.Fatalf("execution meter = %s, want ~8m and never the assurance time", execution)
	}
	if execution+assurance != total {
		t.Fatalf("total active work %s != execution %s + assurance %s", total, execution, assurance)
	}
	if got := ActiveElapsed(state.run, state.events, now); got != total {
		t.Fatalf("the plan-aggregate fold reads %s, want the TOTAL %s", got, total)
	}
	for _, op := range assuranceOperations(state) {
		if op.WallBudget != time.Hour || op.DeadlineBound != BoundAssuranceWork {
			t.Fatalf("assurance operation budget=%s bound=%q, want the assurance allowance", op.WallBudget, op.DeadlineBound)
		}
	}
}

// INVARIANT: execution exhaustion is still enforced - the assurance allowance
// is not a second execution budget.
func TestExecutionExhaustionStillFailsTheRun(t *testing.T) {
	budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: time.Hour, MaxExecutionAttempts: 3}
	fixture, provider, verifier := assuranceFixture(t, budgets, time.Minute, wallStep{mutate: true}, wallStep{mutate: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	if state.snapshot.Disposition != Failed || state.snapshot.Reason != ReasonRunWallBudgetExhausted {
		t.Fatalf("run %s/%s, want failed/%s", state.snapshot.Disposition, state.snapshot.Reason, ReasonRunWallBudgetExhausted)
	}
	if verifier.calls != 0 {
		t.Fatalf("assurance ran %d time(s) for a run whose execution never completed", verifier.calls)
	}
}

// INVARIANT: assurance exhaustion fails the run with its own reason, retains
// the held candidate, and nothing about the exhausted verification satisfies
// assurance, authority or publication.
func TestAssuranceExhaustionFailsClosedAndKeepsTheCandidate(t *testing.T) {
	// Each fixture-clock read ticks a second, so a half-second allowance is
	// spent before the verifier can run: deterministic, no real waiting.
	budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: 500 * time.Millisecond, MaxExecutionAttempts: 2}
	fixture, provider, _ := assuranceFixture(t, budgets, time.Minute,
		wallStep{mutate: true, complete: true, spend: time.Minute})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	if state.snapshot.Disposition != Failed || state.snapshot.Reason != ReasonAssuranceBudgetExhausted {
		t.Fatalf("run %s/%s, want failed/%s", state.snapshot.Disposition, state.snapshot.Reason, ReasonAssuranceBudgetExhausted)
	}
	if !BudgetBoundary(state.snapshot.Disposition, state.snapshot.Reason) {
		t.Fatal("assurance exhaustion is not reported as a budget boundary")
	}
	if state.snapshot.HeldMaterial == nil || state.snapshot.HeldMaterial.Kind != HeldCommittedUnverified {
		t.Fatalf("held material = %+v, want the committed, unverified candidate", state.snapshot.HeldMaterial)
	}
	events := journalOf(t, fixture.runtime, runID)
	for _, event := range events {
		if event.Type != EventAssuranceObserved {
			continue
		}
		var observed AssuranceObservedPayload
		if err := json.Unmarshal(event.Payload, &observed); err != nil || observed.Passed {
			t.Fatalf("an exhausted verification journalled assurance %+v (%v)", observed, err)
		}
	}
	for _, op := range assuranceOperations(state) {
		if op.State == Succeeded {
			t.Fatalf("assurance operation %s succeeded after its allowance was spent", op.ID)
		}
	}
	notPublished(t, fixture, runID)
}

func assuranceEvent(t *testing.T, eventType, operationID, kind string, at time.Time) EngineeringEvent {
	t.Helper()
	payload, err := json.Marshal(RunOperation{ID: operationID, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return EngineeringEvent{Type: eventType, OperationID: operationID, OccurredAt: at, Payload: payload}
}

// INVARIANT: the meter is derived from the journal and never refunds. A
// crashed attempt is charged up to its successor's start, a retry adds to the
// total rather than restarting it, and only assurance operations count.
func TestAssuranceMeterIsJournalDerivedAndNeverRefunds(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0).UTC()
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }
	state := &runState{events: []EngineeringEvent{
		assuranceEvent(t, EventOperationBefore, "execution", OpExecutionInvoke, at(0)),
		assuranceEvent(t, EventOperationAfter, "execution", OpExecutionInvoke, at(9)),
		assuranceEvent(t, EventOperationBefore, "assure", OpAssuranceGo, at(10)),
		// The controller died here; a restart starts the same operation again.
		assuranceEvent(t, EventOperationBefore, "assure", OpAssuranceGo, at(30)),
		assuranceEvent(t, EventOperationAfter, "assure", OpAssuranceGo, at(35)),
	}}
	if got := state.assuranceElapsed(at(40)); got != 25*time.Minute {
		t.Fatalf("meter = %s, want 25m (20m crashed attempt + 5m retry), execution excluded", got)
	}
	state.events = append(state.events, assuranceEvent(t, EventOperationBefore, "assure", OpAssuranceGo, at(36)))
	if got := state.assuranceElapsed(at(40)); got != 29*time.Minute {
		t.Fatalf("meter = %s, want 29m: an unfinished attempt is charged to now", got)
	}
}

// INVARIANT: a run frozen before #640 keeps the legacy rule exactly
// (assurance charged to the run wall), and a negative frozen ceiling is
// corrupt state that fails closed.
func TestRunsWithoutAnAssuranceCeilingKeepTheLegacyRuleAndNegativeFailsClosed(t *testing.T) {
	budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: time.Hour, MaxExecutionAttempts: 2}
	fixture, provider, _ := assuranceFixture(t, budgets, 10*time.Minute, wallStep{mutate: true, complete: true, spend: time.Minute})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	now := fixture.clock.Now()
	legacy := *state.run.Budgets
	legacy.AssuranceWallLimit = 0
	state.run.Budgets = &legacy
	if state.executionElapsed(now) != state.activeElapsed(now) {
		t.Fatal("a legacy run's assurance time was split off its wall budget")
	}
	if got := state.operationWallBudget(OpAssuranceGo); got != 30*time.Minute {
		t.Fatalf("legacy assurance budget = %s, want the run wall", got)
	}
	if _, bounded := state.assuranceRemaining(now); bounded {
		t.Fatal("a legacy run claims an assurance ceiling it never froze")
	}
	legacy.AssuranceWallLimit = -time.Second
	if err := state.invariants(); err == nil {
		t.Fatal("a negative frozen assurance ceiling was accepted")
	}
}

// Status must describe persisted legacy counters without changing their charge.
func TestStatusAssuranceAccountingModes(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "legacy"}[legacy], func(t *testing.T) {
			budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: time.Hour, MaxExecutionAttempts: 2}
			fixture, provider, _ := assuranceFixture(t, budgets, 10*time.Minute, wallStep{mutate: true, complete: true, spend: time.Minute})
			runID := fixture.start()
			drive(fixture, provider, runID)
			if legacy {
				run := fixture.state(runID).run
				frozen := *run.Budgets
				frozen.AssuranceWallLimit = 0
				run.Budgets = &frozen
				if err := fixture.store.PutRun(run); err != nil {
					t.Fatal(err)
				}
				// Today's configuration must not determine the persisted mode.
			}
			report, err := fixture.runtime.Status(runID)
			if err != nil {
				t.Fatal(err)
			}
			if report.AssuranceElapsed < 10*time.Minute {
				t.Fatalf("missing observed assurance: %+v", report)
			}
			if legacy {
				if report.AccountingMode != "legacy" || report.ExecutionElapsed != report.ActiveElapsed {
					t.Fatalf("legacy charge misrepresented: %+v", report)
				}
			} else if report.AccountingMode != "split" || report.ExecutionElapsed+report.AssuranceElapsed != report.ActiveElapsed {
				t.Fatalf("split accounting misrepresented: %+v", report)
			}
			state := fixture.state(runID)
			if got := ActiveElapsed(state.run, state.events, report.Now); got != report.ActiveElapsed {
				t.Fatalf("plan active work %s != status %s", got, report.ActiveElapsed)
			}
		})
	}
}
