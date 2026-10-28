package runtime

// A judged failure of a final-integration feature test must not be routed like
// an external refusal. These pin the accounting facts that the failure/hold
// design for acceptance_feature depends on, so a change to any of them is seen
// before a feature failure is classified on top of it.

import (
	"encoding/json"
	"testing"
	"time"
)

// CHARACTERIZATION OF A CURRENT DEFECT, not an intended invariant: this asserts
// today's behavior so a feature-acceptance failure is never routed through it.
// An assurance result never says a provider ran, so the wait path treats it as
// work that did not happen: the attempt AND the execution time are given back.
// That is right for an account refusal and wrong for a 13 minute browser run
// that judged the candidate.
func TestAWaitRoutedAssuranceResultWouldHaveItsSpentWorkRefunded(t *testing.T) {
	result, err := json.Marshal(assuranceRecord{FailureClass: FailureAssurancePrerequisite})
	if err != nil {
		t.Fatal(err)
	}
	if providerExecuted(result) {
		t.Fatal("an assurance record claims a provider executed")
	}
	if _, waiting := waitRoutedFailure(result); !waiting {
		t.Fatal("the wait-routed assurance class stopped routing to a wait")
	}

	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	started := op.Attempt
	clock.advance(20 * time.Minute) // the verification really ran
	if _, err := scheduler.Finish(op.ID, OperationFailed); err != nil {
		t.Fatal(err)
	}
	// This is exactly the call the wait branch of runOperation makes.
	restored, err := scheduler.RestoreAttempt(op.ID, !providerExecuted(result))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Attempt != started-1 || restored.ConsumedExecution != 0 {
		t.Fatalf("attempt %d->%d, consumed %s: the wait path no longer refunds an assurance attempt", started, restored.Attempt, restored.ConsumedExecution)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 30*time.Minute {
		t.Fatalf("the next attempt received %s, want the full 30m: twenty minutes of verification were refunded", got)
	}
}

// CHARACTERIZATION OF A CURRENT LIMIT: the legacy no-ceiling case is the
// behavior of runs created before the assurance ceiling existed.
// What survives a refund is the journal-derived assurance meter, and only for a
// run that froze an assurance ceiling. A legacy run has no such backstop, so a
// wait route would be an unbounded rerun loop there.
func TestOnlyTheJournalMeterSurvivesARefundAndOnlyUnderAnAssuranceCeiling(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0).UTC()
	state := &runState{rt: &EngineeringRuntime{}, events: []EngineeringEvent{
		assuranceEvent(t, EventOperationBefore, "assure", OpAssuranceGo, t0),
		assuranceEvent(t, EventOperationAfter, "assure", OpAssuranceGo, t0.Add(20*time.Minute)),
	}}
	if got := state.assuranceElapsed(t0.Add(time.Hour)); got != 20*time.Minute {
		t.Fatalf("the journal meter read %s, want the 20m the verification ran", got)
	}
	// no frozen budgets: a run created before the assurance ceiling existed
	if _, bounded := state.assuranceRemaining(t0.Add(time.Hour)); bounded {
		t.Fatal("a run with no assurance ceiling claims a backstop it never froze")
	}
}

// CHARACTERIZATION OF A CURRENT DEFECT for a judged failure: the retry cannot
// change a deterministic answer. When RouteHold lands this test is replaced.
// A judged failure reclassified as integration_conflict is retryable: it is
// the same bounded loop of full re-verifications of the same integrated tree.
func TestAReclassifiedIntegrationFailureIsRetriedAgainstTheSameCandidate(t *testing.T) {
	if !reattemptable(RouteFailure(FailureIntegrationConflict)) {
		t.Fatal("integration_conflict stopped being reattemptable")
	}
	if RouteFailure(FailureIntegrationConflict) == RouteProviderRemediation {
		t.Fatal("an integration unit's failure routes to a provider, which it never has")
	}
}
