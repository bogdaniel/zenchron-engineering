package runtime

// Where the three M2 stabilization boundaries meet.
//
// Each of them is proved on its own elsewhere: an approval binds the
// assignments it was shown (#107), an unchanged obligation renews across an
// approved revision (#108, #114), and a frozen assignment nothing ran cannot
// later execute stale upstream work (#109). What their own tests cannot show is
// that they compose - that recovery does not step outside the approval, that a
// revision approved while an abandoned freeze exists binds what it will
// actually perform, and that neither costs a run twice.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Intersection A: an abandoned freeze recovers under the approval that
// authorized it, against the candidate the producer settled on.
//
// The recovery path advances an execution generation and resolves a fresh
// assignment. That resolution has to come from the APPROVAL, not from whatever
// the registry says at recovery time - otherwise a start that failed becomes a
// way to execute a configuration nobody approved, reached by waiting.
func TestAnOrphanFreezeRecoversUnderTheApprovalThatAuthorizedIt(t *testing.T) {
	fixture := newPlanRunFixture(t, orphanStages())
	startFails := false
	fixture.reconciler.Engine = func(repository, agentID string) (*EngineeringRuntime, error) {
		fixture.engineCalls = append(fixture.engineCalls, agentID)
		if startFails {
			return nil, errors.New("the worker's engine could not be built")
		}
		return fixture.runtime, nil
	}
	fixture.approve(t)

	// What the approval authorized for the dependent stage.
	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	authorized, ok := bound["review"]
	if !ok {
		t.Fatalf("the approval bound nothing for the dependent stage: %#v", bound)
	}

	fixture.reconcile(t)
	implementation := mustReplayPlan(t, fixture).Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	// The dependent freezes against candidate X and the start then fails
	// before the stage was ever associated with a run.
	startFails = true
	fixture.reconcile(t)
	abandoned, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("nothing was frozen before the failure: found=%v err=%v", found, err)
	}
	if abandoned.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the abandoned assignment does not name candidate X: %#v", abandoned.Context.UpstreamOutputs)
	}

	// The operator edits the workforce while the plan is stuck, and the
	// producer settles a replacement.
	fixture.service.DefaultAgent = "claude"
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")

	// RESTART, and recover from durable state alone.
	restarted := restartReconciler(t, fixture)
	restarted.Service.DefaultAgent = "claude"
	for i := 0; i < 3; i++ {
		if _, err := restarted.Reconcile(context.Background(), fixture.plan.ID); err != nil {
			t.Fatalf("reconcile after restart: %v", err)
		}
	}

	after := mustReplayPlan(t, fixture)
	if after.Stages["review"].RunID == "" {
		t.Fatal("the stage never recovered")
	}
	if after.Stages["review"].AssignmentID == abandoned.ID {
		t.Fatalf("the stage executed the assignment frozen against candidate X: %s", abandoned.ID)
	}
	executing, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision,
		after.Stages["review"].Generation, "review")
	if err != nil || !found {
		t.Fatalf("the executing assignment is not readable: found=%v err=%v", found, err)
	}

	// THE APPROVAL still governs. The registry moved and the recovery did not
	// follow it.
	if executing.Agent != authorized.Agent || executing.Profile.ID != authorized.Profile.ID ||
		executing.Profile.Digest != authorized.Profile.Digest ||
		executing.TrustRequirement != authorized.TrustRequirement ||
		executing.InvocationMode != authorized.InvocationMode {
		t.Fatalf("recovery executed a configuration the approval never authorized:\napproved %#v\nexecuting %#v",
			authorized.Agent, executing.Agent)
	}
	// And it consumes the candidate the producer actually settled on.
	if got := executing.Context.UpstreamOutputs[0].Candidate; got != "bbbbbbbbbbbb" {
		t.Fatalf("the recovered performance consumes %q, want the settled replacement", got)
	}
	// The abandoned row is untouched provenance.
	again, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("the abandoned assignment was deleted: found=%v err=%v", found, err)
	}
	if again.ID != abandoned.ID || again.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the abandoned assignment was rewritten: %#v", again)
	}
	// ONE run for the stage, and the plan was charged for exactly the runs
	// that exist. A recovery that created a second performance, or one whose
	// spend nothing counted, would show here.
	runs := planStageRuns(t, fixture, "review")
	if len(runs) != 1 {
		t.Fatalf("the stage has %d runs, want exactly one: %v", len(runs), runs)
	}
	if after.Consumed.ChildRuns != len(planStageRuns(t, fixture, "")) {
		t.Fatalf("the plan is charged %d child runs and %d exist", after.Consumed.ChildRuns, len(planStageRuns(t, fixture, "")))
	}
}

// Intersection C: a revision approved while an abandoned freeze exists binds
// the performances it will actually create.
//
// The stage has a durable assignment and no association, so it reads as
// unstarted - which is exactly right: the freeze authorized nothing and the
// revision is about to replace it. What must not happen is the recovery
// resurrecting the old revision's configuration under the new one's authority.
func TestARevisionApprovedOverAnOrphanFreezeBindsWhatItWillPerform(t *testing.T) {
	fixture := newPlanRunFixture(t, orphanStages())
	startFails := false
	fixture.reconciler.Engine = func(repository, agentID string) (*EngineeringRuntime, error) {
		fixture.engineCalls = append(fixture.engineCalls, agentID)
		if startFails {
			return nil, errors.New("the worker's engine could not be built")
		}
		return fixture.runtime, nil
	}
	fixture.approve(t)
	fixture.reconcile(t)
	implementation := mustReplayPlan(t, fixture).Stages["implementation"].RunID
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	startFails = true
	fixture.reconcile(t)
	abandoned, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 1, 0, "review")
	if err != nil || !found {
		t.Fatalf("nothing was frozen before the failure: found=%v err=%v", found, err)
	}
	if abandoned.Agent.ID != "codex" {
		t.Fatalf("the fixture froze worker %q, and this test needs codex", abandoned.Agent.ID)
	}
	startFails = false

	// A revision that changes the stage, read and approved while that freeze
	// sits there - and the operator's default has moved since revision 1.
	second := fixture.plan
	second.Revision = 2
	previous := fixture.plan.Revision
	second.Provenance.PreviousRevision = &previous
	second.Stages = append([]domain.PlanStage(nil), fixture.plan.Stages...)
	second.Stages[1].Objective = "Review it, against the newer standard."
	digest, digestErr := second.ContentDigest()
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	second.Digest = digest
	if _, err := fixture.store.PutPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutPlanContract(second.ID, second.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	fixture.service.DefaultAgent = "claude"
	fixture.reconciler.Service = fixture.service

	preview, err := fixture.service.ViewRevision(fixture.plan.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	shown, ok := assignmentFor(preview.Assigned, "review")
	if !ok {
		t.Fatalf("the preview showed no assignment for the stage it will redo: %#v", preview.Blocked)
	}
	if shown.Agent.ID != "claude" {
		t.Fatalf("the preview resolved worker %q; this test needs revision 2 to differ from the freeze", shown.Agent.ID)
	}
	if _, err := fixture.service.Approve(second.ID, second.Revision, second.Digest,
		preview.AssignmentsDigest, "operator", ""); err != nil {
		t.Fatalf("approving revision 2 over an abandoned freeze: %v", err)
	}
	fixture.plan = second

	// It bound the stage it will perform, and it bound what it showed.
	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bound["review"]; !ok {
		t.Fatalf("revision 2 bound nothing for the stage it will redo: %#v", bound)
	}
	if bound["review"].Agent != shown.Agent {
		t.Fatalf("revision 2 bound %#v and showed %#v", bound["review"].Agent, shown.Agent)
	}
	// The supersession is recorded, and revision 1's freeze is left alone.
	superseded := mustReplayPlan(t, fixture)
	if len(superseded.Superseded) != 1 || superseded.Superseded[0].ToRevision != 2 {
		t.Fatalf("the approval recorded no supersession onto revision 2: %#v", superseded.Superseded)
	}
	kept, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 1, 0, "review")
	if err != nil || !found {
		t.Fatalf("revision 1's abandoned assignment was deleted: found=%v err=%v", found, err)
	}
	if kept.ID != abandoned.ID || kept.Agent.ID != "codex" {
		t.Fatalf("revision 1's abandoned assignment was rewritten: %#v", kept.Agent)
	}

	// And what executes under revision 2 is revision 2's authority.
	for i := 0; i < 3; i++ {
		fixture.reconcile(t)
	}
	after := mustReplayPlan(t, fixture)
	if after.Stages["review"].RunID == "" {
		t.Fatal("the stage never performed under revision 2")
	}
	executing, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 2, after.Stages["review"].Generation, "review")
	if err != nil || !found {
		t.Fatalf("revision 2 froze nothing for the stage: found=%v err=%v", found, err)
	}
	if executing.Agent != shown.Agent {
		t.Fatalf("revision 2 executed %#v, which is not what its approval authorized (%#v)", executing.Agent, shown.Agent)
	}
	if executing.Plan.Revision != 2 {
		t.Fatalf("the executing assignment is recorded under revision %d", executing.Plan.Revision)
	}
	if got := executing.Context.Objective; !strings.Contains(got, "newer standard") {
		t.Fatalf("the performance carries revision 1's objective: %q", got)
	}
}

// planStageRuns is every durable run this plan created for one stage, or for
// every stage when the id is empty. It reads the RUNS rather than the plan
// projection, because a second performance nothing points at is exactly the
// thing a projection would not show.
func planStageRuns(t *testing.T, fixture *planRunFixture, stageID string) []string {
	t.Helper()
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, run := range runs {
		if run.Plan == nil || run.Plan.PlanID != fixture.plan.ID {
			continue
		}
		if stageID != "" && run.Plan.StageID != stageID {
			continue
		}
		found = append(found, run.ID)
	}
	return found
}
