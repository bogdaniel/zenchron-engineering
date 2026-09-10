package runtime

// Defects the review of #64 surfaced in the plan service, each held by the
// smallest test that fails if the defect is restored.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ClaimPlan is a conditional insert, so `false` means another proposal created
// this plan first. The loser used to continue: it appended a SECOND proposed
// event for a revision that already existed, and wrote a contract and a source
// binding the winner never agreed to.
//
// Concurrent proposals are legitimate - each one that sees a stored plan
// proposes the NEXT revision. What is never legitimate is two proposed events
// for the SAME revision, which is exactly what the lost claim produced. The
// interleaving cannot be forced through this seam, so the test drives the real
// window and asserts that invariant rather than a schedule.

// A plan row is stamped by the runtime's clock, not by the wall clock.
//
// Runs and operations carry the created instant their caller's clock produced;
// the plan store read time.Now() itself. A plan document deliberately carries
// no timestamp - it would make the digest depend on when the plan was written -
// so this row is the only record of when a plan appeared, and a store that
// reads its own clock puts it on a different axis from every other row and out
// of reach of the clock the rest of the runtime is driven by.
func TestAPlanIsStampedByTheRuntimeClock(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	contract := planFixtureContract(fixture.phase8Fixture)
	before := fixture.clock.Now().UnixNano()
	if _, err := fixture.service.Propose(context.Background(), ProposeInput{
		PlanID: "plan-stamped", Objective: "Make the widget idempotent.",
		Subject:  domain.Subject{Repository: "acme/repo", Revision: fixture.base},
		Contract: contract, Issue: fixture.issue,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1",
			Subject: domain.Subject{Repository: "acme/repo", Revision: fixture.base},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var stamped int64
	if err := fixture.store.db.QueryRow(`SELECT created_unix_nano FROM plans WHERE id = ?`, "plan-stamped").Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	// The fixture clock advances on every read, so the assertion is that the
	// stamp came from ITS timeline. A wall-clock read lands outside this
	// window entirely: the fixture's clock is not set to the present.
	if after := fixture.clock.Now().UnixNano(); stamped < before || stamped > after {
		t.Fatalf("the plan is stamped %d, outside the runtime clock's window [%d, %d]", stamped, before, after)
	}
}

func TestConcurrentProposalsNeverRecordOneRevisionTwice(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	contract := planFixtureContract(fixture.phase8Fixture)
	input := ProposeInput{
		PlanID: "plan-contended", Objective: "Make the widget idempotent.",
		Subject:  domain.Subject{Repository: "acme/repo", Revision: fixture.base},
		Contract: contract, Issue: fixture.issue,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1",
			Subject: domain.Subject{Repository: "acme/repo", Revision: fixture.base},
		},
	}

	var waiting, start sync.WaitGroup
	start.Add(1)
	errs := make([]error, 8)
	for i := range errs {
		waiting.Add(1)
		go func(slot int) {
			defer waiting.Done()
			start.Wait()
			_, errs[slot] = fixture.service.Propose(context.Background(), input)
		}(i)
	}
	start.Done()
	waiting.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatalf("every concurrent proposal failed: %v", errors.Join(errs...))
	}
	events, err := fixture.store.PlanEvents("plan-contended")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, event := range events {
		if event.Type != EventPlanProposed {
			continue
		}
		var payload PlanProposedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if seen[payload.Revision] {
			t.Fatalf("revision %d was proposed twice: a lost claim proposed over the winner", payload.Revision)
		}
		seen[payload.Revision] = true
	}
	if len(seen) == 0 {
		t.Fatal("no proposal was recorded at all")
	}
}

// Approval moves forward. Re-approving a superseded revision left the durable
// approval record describing something other than the executing work, and made
// the next supersession measure against the wrong predecessor.
func TestApprovingASupersededRevisionIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	if _, err := fixture.service.Approve(fixture.plan.ID, 1, fixture.plan.Digest, shownAssignments(t, fixture.service, fixture.plan.ID, 1), "operator", ""); err != nil {
		t.Fatal(err)
	}
	second := fixture.plan
	second.Revision = 2
	digest, err := second.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	second.Digest = digest
	if _, err := fixture.store.PutPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	// The contract this revision was planned against. Approval binds the
	// assignments it resolves, and resolution reads the obligations from
	// here - so a revision stored without one is a state the propose path
	// never produces.
	if err := fixture.store.PutPlanContract(second.ID, second.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Approve(second.ID, 2, second.Digest, shownAssignments(t, fixture.service, second.ID, 2), "operator", ""); err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.Approve(fixture.plan.ID, 1, fixture.plan.Digest, shownAssignments(t, fixture.service, fixture.plan.ID, 1), "operator", "")
	var refused *PlanRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("approving the superseded revision 1 was allowed: %v", err)
	}
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Approval.Revision != 2 || snapshot.Approval.Status != domain.ApprovalApproved {
		t.Fatalf("the approval record reads revision %d (%s), want an approved revision 2", snapshot.Approval.Revision, snapshot.Approval.Status)
	}
}

// The supersession event is measured from the APPROVED revision. Reading the
// latest decision instead meant the event was never emitted at all, because
// proposing a revision resets that decision to pending.
func TestApprovingALaterRevisionRecordsTheSupersession(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	if _, err := fixture.service.Approve(fixture.plan.ID, 1, fixture.plan.Digest, shownAssignments(t, fixture.service, fixture.plan.ID, 1), "operator", ""); err != nil {
		t.Fatal(err)
	}
	second := fixture.plan
	second.Revision = 2
	second.Objective = "Make the widget idempotent, and say so."
	digest, err := second.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	second.Digest = digest
	if _, err := fixture.store.PutPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	// The contract this revision was planned against. Approval binds the
	// assignments it resolves, and resolution reads the obligations from
	// here - so a revision stored without one is a state the propose path
	// never produces.
	if err := fixture.store.PutPlanContract(second.ID, second.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	// A proposal resets the pending decision, exactly as the real path does.
	if err := fixture.service.appendPlanEvent(second.ID, EventPlanProposed, PlanProposedPayload{
		Revision: 2, Digest: second.Digest, ObjectiveDigest: fixture.plan.Digest,
		StageCount: len(second.Stages), AgentStageCount: agentStageCount(second),
		Budget: budgetPayload(second.BudgetEnvelope), Origin: domain.ProposalOriginOperatorEdit,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Approve(second.ID, 2, second.Digest, shownAssignments(t, fixture.service, second.ID, 2), "operator", ""); err != nil {
		t.Fatal(err)
	}

	events, err := fixture.store.PlanEvents(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	superseded := 0
	for _, event := range events {
		if event.Type == EventPlanRevisionSuperseded {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("approving revision 2 over an approved revision 1 recorded %d supersession events, want 1", superseded)
	}
}

// A plan answers ONE source. A bind naming a different issue used to return
// success and change nothing, so a caller believed it had redirected a plan
// that went on creating runs against its original issue.
func TestRebindingAPlanToADifferentIssueIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	if err := fixture.store.BindPlanSource(fixture.plan.ID, fixture.issue); err != nil {
		t.Fatalf("re-binding a plan to the SAME issue is idempotent and must succeed: %v", err)
	}
	err := fixture.store.BindPlanSource(fixture.plan.ID, fixture.issue+1)
	if err == nil {
		t.Fatal("a plan bound to one issue was silently rebound to another")
	}
	_, issue, found, sourceErr := fixture.store.PlanSource(fixture.plan.ID)
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}
	if !found || issue != fixture.issue {
		t.Fatalf("the plan now answers issue #%d, want #%d", issue, fixture.issue)
	}
	if err := fixture.store.BindPlanSource("plan-that-does-not-exist", 7); err == nil {
		t.Fatal("binding a source to a plan that does not exist reported success")
	}
}

// A revision cannot claim a ceiling below what the plan has already spent.
//
// `Propose` replayed the plan and passed its consumption into the compiler,
// and the compiler dropped it before validating - so the consumed-budget
// refusals saw zero on the only path that compiles a real revision, and the
// non-reset law was unenforced exactly where it applies.
func TestARevisionCannotClaimACeilingBelowWhatIsSpent(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	fixture.approve(t)
	fixture.reconcile(t)

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns < 2 {
		t.Fatalf("the fixture spent %d child runs, want at least 2 to test a lower ceiling", snapshot.Consumed.ChildRuns)
	}

	// One child run is fewer than the two already created.
	tighter := fixture.service
	tighter.Envelope = domain.PlanBudgetEnvelope{MaxChildRuns: 1, MaxConcurrency: 1, MaxProviderInvocations: 12}
	_, err = tighter.Propose(context.Background(), ProposeInput{
		PlanID: fixture.plan.ID, Objective: fixture.plan.Objective, Subject: fixture.plan.Subject,
		Contract: planFixtureContract(fixture.phase8Fixture), Issue: fixture.issue,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1",
			Subject: fixture.plan.Subject,
		},
	})
	if err == nil {
		t.Fatal("a revision claimed a child-run ceiling below what the plan had already spent")
	}
	if !strings.Contains(err.Error(), "already been created") {
		t.Fatalf("the refusal does not name the consumption: %v", err)
	}
}

// A decision NAMES the content it decides. The CLI required the digest; the
// service did not, so any other caller - the control endpoint among them -
// could approve a revision by number alone.
func TestADecisionWithoutADigestIsRefusedAtTheServiceBoundary(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	before, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, digest := range []string{"", "   "} {
		if _, err := fixture.service.Approve(fixture.plan.ID, fixture.plan.Revision, digest, shownAssignments(t, fixture.service, fixture.plan.ID, fixture.plan.Revision), "operator", ""); err == nil {
			t.Fatalf("an approval naming digest %q was accepted", digest)
		}
		if _, err := fixture.service.Reject(fixture.plan.ID, fixture.plan.Revision, digest, "", "operator", ""); err == nil {
			t.Fatalf("a rejection naming digest %q was accepted", digest)
		}
	}
	after, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a digestless decision appended %d events", len(after)-len(before))
	}
	if _, err := fixture.service.Approve(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest, shownAssignments(t, fixture.service, fixture.plan.ID, fixture.plan.Revision), "operator", ""); err != nil {
		t.Fatalf("the exact digest was refused: %v", err)
	}
}

// A refused revision stays refused, whatever is validated after it.
//
// The projection kept ONE validation record, and `decide` compared it by
// revision - so a later revision's verdict replaced an earlier refusal and the
// refused revision passed the check by having been superseded in a field
// rather than by having been fixed.
func TestARefusedRevisionStaysRefusedAfterALaterValidation(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())

	// Revision 2 is stored and REFUSED.
	second := fixture.plan
	second.Revision = 2
	second.Objective = "Make the widget idempotent, twice."
	digest, err := second.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	second.Digest = digest
	if _, err := fixture.store.PutPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	// The contract this revision was planned against. Approval binds the
	// assignments it resolves, and resolution reads the obligations from
	// here - so a revision stored without one is a state the propose path
	// never produces.
	if err := fixture.store.PutPlanContract(second.ID, second.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.recordValidation(second.ID, second.Revision, second.Digest,
		domain.ProposalRefused, errors.New("the reviewer stage has no independent worker")); err != nil {
		t.Fatal(err)
	}
	// Revision 3 is then validated, which used to overwrite the only record.
	third := second
	third.Revision = 3
	third.Objective = "Make the widget idempotent, three times."
	if digest, err = third.ContentDigest(); err != nil {
		t.Fatal(err)
	}
	third.Digest = digest
	if _, err := fixture.store.PutPlanRevision(third); err != nil {
		t.Fatal(err)
	}
	// The contract this revision was planned against. Approval binds the
	// assignments it resolves, and resolution reads the obligations from
	// here - so a revision stored without one is a state the propose path
	// never produces.
	if err := fixture.store.PutPlanContract(third.ID, third.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.recordValidation(third.ID, third.Revision, third.Digest, domain.ProposalValid, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.service.Approve(second.ID, second.Revision, second.Digest, shownAssignments(t, fixture.service, second.ID, second.Revision), "operator", ""); err == nil {
		t.Fatal("a refused revision was approved because a later revision was validated after it")
	}
	if _, err := fixture.service.Approve(third.ID, third.Revision, third.Digest, shownAssignments(t, fixture.service, third.ID, third.Revision), "operator", ""); err != nil {
		t.Fatalf("the validated revision was refused: %v", err)
	}
}

// An operator may change their mind twice. Approve, reject, approve again: the
// last decision is the one that governs, and the rejection stays in the record
// as the historical fact it is.
func TestAnOperatorCanApproveAfterRejecting(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	id, revision, digest := fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest

	if _, err := fixture.service.Approve(id, revision, digest, shownAssignments(t, fixture.service, id, revision), "operator", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Reject(id, revision, digest, "", "operator", "on reflection, no"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Approve(id, revision, digest, shownAssignments(t, fixture.service, id, revision), "operator", "yes after all"); err != nil {
		t.Fatalf("an operator could not approve after rejecting: %v", err)
	}

	snapshot, err := fixture.store.ReplayPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Approval.Status != domain.ApprovalApproved || snapshot.Approval.Note != "yes after all" {
		t.Fatalf("the newest decision is %+v, want the second approval", snapshot.Approval)
	}
	governing, ok := snapshot.ApprovedRevision()
	if !ok || governing != revision {
		t.Fatalf("governing revision = %d (%v), want %d", governing, ok, revision)
	}
	if !snapshot.Rejected[revision] {
		t.Fatal("the rejection was erased; it is a fact about what happened, not a mode")
	}
}
