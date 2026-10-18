package runtime

import (
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
