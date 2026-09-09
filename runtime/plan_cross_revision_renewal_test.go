package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// approveNextRevision stores and approves a revision that carries every stage
// forward unchanged, planned against its own stored contract.
//
// It is the ordinary propose-approve an operator performs when something else
// in the plan changed: the stages this test cares about are byte-identical, and
// the plan revision, the plan digest and the per-revision contract pointer all
// necessarily advance. That combination is the whole subject of #108 and #114.
func approveNextRevision(t *testing.T, fixture *planRunFixture, contract domain.EngineeringWorkContract) domain.EngineeringPlan {
	t.Helper()
	previous := fixture.plan.Revision
	next := fixture.plan
	next.Revision = previous + 1
	next.Provenance.PreviousRevision = &previous
	next.Provenance.Contract = domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}
	digest, err := next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	next.Digest = digest
	if _, err := fixture.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutPlanContract(next.ID, next.Revision, contract); err != nil {
		t.Fatal(err)
	}
	fixture.plan = next
	fixture.approve(t)
	return next
}

// carriedContract is the fixture contract recompiled for a later plan revision:
// identical obligations, a new revision string, and the predecessor it now
// follows. Contracts are stored per plan revision, so this is what "the stage
// was carried forward unchanged" actually looks like on disk.
func carriedContract(base *phase8Fixture, revision, previous string) domain.EngineeringWorkContract {
	contract := planFixtureContract(base)
	contract.Revision = revision
	contract.Provenance.PreviousContractRevision = &previous
	return contract
}

// reviewStages is implementation -> independent review, the smallest shape in
// which an upstream candidate can move under a stage that has already been
// performed.
func reviewStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	}
}

// performUnderRevisionOne drives the fixture through the ordinary first pass:
// approve, implement candidate A, review it, settle both. It returns the run
// the implementation stage is parked in, so a caller can move its head.
func performUnderRevisionOne(t *testing.T, fixture *planRunFixture) string {
	t.Helper()
	fixture.approve(t)
	fixture.reconcile(t)

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")
	fixture.reconcile(t)

	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Stages["review"].RunID
	if review == "" {
		t.Fatal("the review stage created no run against candidate A")
	}
	recordCandidateAndAssurance(t, fixture, review, "rrrrrrrrrrrr")
	settleRunAtGoalState(t, fixture, review, "rrrrrrrrrrrr")
	fixture.reconcile(t)
	fixture.reconcile(t)

	completed, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the review did not complete against candidate A: %#v", completed.Stages["review"])
	}
	return implementation
}

// An unchanged stage renews across an approved plan revision.
//
// This is #108 and #114 through the production path, and the shape matters as
// much as the outcome. The renewal is not a copy of the revision-1 assignment
// with one field edited: it is whatever `PlanService.Resolve` produces for the
// approved revision 2, which necessarily carries revision 2, revision 2's plan
// digest and revision 2's contract pointer. The structural comparison used to
// read all three as an authority change and refuse the generation, so a stage
// whose upstream had moved could never be re-performed once any later revision
// had been approved - it demanded an approval for an obligation nobody had
// changed.
func TestAnUnchangedStageRenewsAcrossAnApprovedRevision(t *testing.T) {
	fixture := newPlanRunFixture(t, reviewStages())
	implementation := performUnderRevisionOne(t, fixture)

	// Revision 2, carrying both stages forward unchanged, planned against its
	// own copy of the same compiled contract.
	second := approveNextRevision(t, fixture, carriedContract(fixture.phase8Fixture, "2", "1"))
	fixture.reconcile(t)
	if second.Revision != 2 || second.Digest == fixture.plan.Provenance.Contract.Revision {
		t.Fatalf("revision 2 is not stored as a distinct revision: %#v", second.Provenance)
	}
	firstPerformance, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 1, 0, "review")
	if err != nil || !found {
		t.Fatalf("the revision-1 performance is not readable: found=%v err=%v", found, err)
	}

	// The producer is re-activated by feedback and settles a different
	// candidate. The review that was performed was about A.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")
	fixture.reconcile(t)

	invalidated := mustReplay(t, fixture)
	if invalidated.Stages["review"].State == PlanStageCompleted {
		t.Fatal("the review stayed completed after the work it reviewed was replaced")
	}

	// And it is PERFORMED AGAIN - automatically, under the approved revision 2.
	report := fixture.reconcile(t)
	for _, block := range report.Blocked {
		if block.StageID == "review" {
			t.Fatalf("the renewal was refused: %s (%s)", block.Reason, block.Kind)
		}
	}
	renewed := mustReplay(t, fixture)
	if renewed.Stages["review"].Generation != 1 {
		t.Fatalf("the stage is at generation %d, want 1", renewed.Stages["review"].Generation)
	}
	if renewed.Stages["review"].RunID == "" {
		t.Fatal("the renewal started no run")
	}
	next, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 2, 1, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the renewal was not frozen under the approved revision 2")
	}
	if got := next.Context.UpstreamOutputs[0].Candidate; got != "bbbbbbbbbbbb" {
		t.Fatalf("the renewal consumes %q, want the candidate whose replacement caused it", got)
	}
	// The frozen renewal IS the resolver's revision-2 output, and all three
	// approved-plan pointers really did advance. Without this the test would
	// pass against a renewal that had never crossed a revision at all.
	if next.Plan.Revision == firstPerformance.Plan.Revision ||
		next.Plan.Digest == firstPerformance.Plan.Digest ||
		next.Contract.Revision == firstPerformance.Contract.Revision {
		t.Fatalf("the renewal carries revision 1's pointers, so no cross-revision renewal was exercised:\nplan %#v contract %#v\nplan %#v contract %#v",
			firstPerformance.Plan, firstPerformance.Contract, next.Plan, next.Contract)
	}
	// Same obligation, exactly: the pointers moved and nothing else did.
	if next.Role != firstPerformance.Role || next.Agent != firstPerformance.Agent ||
		next.TrustRequirement != firstPerformance.TrustRequirement ||
		next.InvocationMode != firstPerformance.InvocationMode ||
		next.Contract.ID != firstPerformance.Contract.ID ||
		next.Profile.ID != firstPerformance.Profile.ID ||
		next.Profile.Version != firstPerformance.Profile.Version ||
		next.Profile.Digest != firstPerformance.Profile.Digest {
		t.Fatalf("the renewal changed the approved obligation:\n%#v\n%#v", firstPerformance, next)
	}
	// The revision-1 performance is still exactly where it was. A renewal
	// records a new performance; it does not rewrite the one it renews.
	again, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 1, 0, "review")
	if err != nil || !found {
		t.Fatalf("the revision-1 performance was lost: found=%v err=%v", found, err)
	}
	if again.ID != firstPerformance.ID || again.Plan != firstPerformance.Plan {
		t.Fatalf("the revision-1 performance was rewritten: %#v", again)
	}
}

// The same sequence with MATERIALLY CHANGED obligations is refused.
//
// #114's other half. Erasing the per-revision contract pointer from the
// comparison is only safe because the compiled contract's CONTENT is compared
// instead: a revision that recompiles the contract into different obligations
// is a different obligation, and it goes through the approval boundary like any
// other authority change rather than being renewed automatically.
//
// The change is a compiled INVARIANT, deliberately. Objectives, acceptance
// criteria, obligations, permissions, prohibitions and required claims all
// reach the worker through the ContextPack, so a change to any of those is
// already visible in the assignment record and blocks without reading the
// contract at all. Invariants are compiled into the contract and appear
// nowhere in any role's pack: nothing but comparing the contract itself can
// see one move, which is what makes this a causal test of #114's fix rather
// than of the structural comparison beside it.
func TestChangedCompiledObligationsRefuseAutomaticRenewal(t *testing.T) {
	fixture := newPlanRunFixture(t, reviewStages())
	implementation := performUnderRevisionOne(t, fixture)

	changed := carriedContract(fixture.phase8Fixture, "2", "1")
	changed.Invariants = map[string]domain.Requirement{
		"no-secret-material": {Statement: "No credential is written to the repository.", Material: true},
	}
	approveNextRevision(t, fixture, changed)
	fixture.reconcile(t)

	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")
	fixture.reconcile(t)

	report := fixture.reconcile(t)
	blocked := false
	for _, block := range report.Blocked {
		if block.StageID != "review" {
			continue
		}
		blocked = true
		if block.Kind != "authority" {
			t.Fatalf("changed obligations blocked as %q: %s", block.Kind, block.Reason)
		}
		if !strings.Contains(block.Reason, "propose a revision") {
			t.Fatalf("the refusal does not say what to do: %s", block.Reason)
		}
	}
	if !blocked {
		t.Fatal("a stage was renewed automatically under materially changed obligations")
	}
	after := mustReplay(t, fixture)
	if after.Stages["review"].RunID != "" {
		t.Fatalf("the refused renewal started run %s anyway", after.Stages["review"].RunID)
	}
	if _, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 2, 1, "review"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("the refused renewal froze an assignment under the new revision")
	}
}

func mustReplay(t *testing.T, fixture *planRunFixture) PlanSnapshot {
	t.Helper()
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
