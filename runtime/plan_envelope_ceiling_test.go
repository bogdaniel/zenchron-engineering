package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// approveTightenedEnvelope re-stores the approved plan as revision 2 with a
// narrowed envelope and approves it, which is the only way a ceiling changes:
// through a revision an operator approves.
func approveTightenedEnvelope(t *testing.T, fixture *planRunFixture, narrow func(*domain.PlanBudgetEnvelope)) {
	t.Helper()
	tightened := fixture.plan
	tightened.Revision = 2
	previous := fixture.plan.Revision
	tightened.Provenance.PreviousRevision = &previous
	narrow(&tightened.BudgetEnvelope)
	digest, err := tightened.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	tightened.Digest = digest
	if _, err := fixture.store.PutPlanRevision(tightened); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutPlanContract(tightened.ID, tightened.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Approve(tightened.ID, tightened.Revision, tightened.Digest, shownAssignments(t, fixture.service, tightened.ID, tightened.Revision), "operator", ""); err != nil {
		t.Fatal(err)
	}
	fixture.plan = tightened
}

// The plan's own concurrency ceiling holds even when several independent
// stages are dependency-ready and the fleet has room.
func TestThePlanConcurrencyCeilingStartsOneOfTwoReadyStages(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	approveTightenedEnvelope(t, fixture, func(envelope *domain.PlanBudgetEnvelope) { envelope.MaxConcurrency = 1 })

	report := fixture.reconcile(t)
	if len(report.Started) != 1 {
		t.Fatalf("a concurrency ceiling of one started %d stages", len(report.Started))
	}
	gated := false
	for _, blocked := range report.Blocked {
		gated = gated || blocked.Kind == "concurrency"
	}
	if !gated {
		t.Fatalf("the second stage was not blocked on concurrency: %#v", report.Blocked)
	}
}

// A stated monetary ceiling that no provider reports against is refused at
// proposal: the plan must not look as if it honors a limit nothing enforces.
func TestAProposalUnderACostCeilingNothingEnforcesIsRefused(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	envelope := defaultPlanFixtureEnvelope
	ceiling := int64(1000)
	envelope.MaxCostMicros = &ceiling
	answer := plannerAnswer(t, []domain.PlanStage{producerStage("only", "a")})
	_, _, err := proposeTeamWithin(t, fleet.phase8Fixture, fleet.deps.ConfigDigest, answer, envelope)
	var refused *PlanRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Detail, "max_cost_micros") {
		t.Fatalf("a proposal under an unenforceable cost ceiling was not refused: %v", err)
	}
	if _, found, err := fleet.store.Plan("plan-fixture"); err != nil || found {
		t.Fatalf("a refused proposal became a plan: found=%v err=%v", found, err)
	}
}
