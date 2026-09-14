package runtime

// The closed loop, end to end, deterministically.
//
// This is the regression for #126, and it is one test rather than five because
// the defects it covers were one defect wearing five faces. The #119 dogfood
// produced a candidate that failed verification, settled the producer stage
// "completed" on that failure, released an independent reviewer against the
// trusted base instead of the candidate, never entered remediation, and
// reported the whole plan "completed" over an assurance gate that had never
// been evaluated. Every one of those followed from a verification verdict that
// routed nowhere.
//
// So the shape below is the shape of the fix: a producer that fails, remediates
// and succeeds; a reviewer that receives the EXACT candidate, blocks it, and
// sends the producer back; a replacement candidate that invalidates the stale
// review; and a plan that reports completed only when every required obligation
// in the approved document is actually satisfied.
//
// It consumes no provider: the producer's work is modelled by writing the same
// durable events a real run writes. That is deliberate - the question here is
// what the LIFECYCLE does with a verdict, not what a model writes.

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// closedLoopStages is the #119 shape: one producer, one independent reviewer
// that must differ from it, and the assurance gate the contract's claim
// requires.
func closedLoopStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification},
			// The #119 obligation: the reviewer must not be the worker that
			// produced the work. A material producer is never its own sole
			// acceptance evidence.
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"implementation"},
			}},
		{ID: "assurance", Kind: domain.StageAssuranceGate, DependsOn: []string{"review"},
			RequiredClaims: []string{"verification"}},
	}
}

// recordFailedAssurance records a verdict that the candidate was JUDGED and NOT
// accepted, which is the exact observation the #119 producer's run carried while
// its stage settled "completed".
func recordFailedAssurance(t *testing.T, fixture *planRunFixture, runID, head string) {
	t.Helper()
	recordCandidate(t, fixture, runID, head)
	appendRunEventFor(t, fixture, runID, head, EventAssuranceObserved, AssuranceObservedPayload{
		ProviderID: "go", VerifierDefinition: strings.Repeat("d", 64), Passed: false,
		FailureClass: FailureVerification,
		Commit:       head, Tree: head,
	})
}

func planStageState(t *testing.T, fixture *planRunFixture, stageID string) PlanStageProjection {
	t.Helper()
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Stages[stageID]
}

// planStatus is what the fleet summary - the line an operator scans - says about
// this plan, judged against the approved document exactly as production does.
func planStatus(t *testing.T, fixture *planRunFixture) string {
	t.Helper()
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, found, err := fixture.store.PlanRevision(fixture.plan.ID, fixture.plan.Revision)
	if err != nil || !found {
		t.Fatalf("read the approved revision: found=%v err=%v", found, err)
	}
	return planState(plan.Stages, snapshot)
}

func TestTheClosedLoopSurvivesAFailedVerificationAndABlockingReview(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	fixture.approve(t)
	fixture.reconcile(t)

	producer := planStageState(t, fixture, "implementation").RunID
	if producer == "" {
		t.Fatal("the producer stage created no run")
	}

	// 1-3. CANDIDATE A FAILS VERIFICATION, and the producer does not complete.
	//
	// This is the #119 state exactly: a run parked at goal state carrying a
	// failing verdict about its current head. The stage must not settle
	// accepted on it, because "the run stopped" and "the work was accepted" are
	// different facts.
	recordFailedAssurance(t, fixture, producer, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, producer, "aaaaaaaaaaaa")
	fixture.reconcile(t)

	if state := planStageState(t, fixture, "implementation").State; state == PlanStageCompleted {
		t.Fatal("a producer whose verification FAILED settled completed, which is the #119 defect")
	}

	// 4. AND THE REVIEWER IS NOT RELEASED. A dependent released by a failed
	// producer is the second half of that defect: the reviewer would be given
	// the trusted base and would review nothing.
	if review := planStageState(t, fixture, "review"); review.RunID != "" {
		t.Fatalf("the reviewer was released against a failed producer: %#v", review)
	}
	// The plan is not finished either, and says so.
	if status := planStatus(t, fixture); status == PlanStateCompleted {
		t.Fatal("the plan reported completed while its producer had not been accepted")
	}

	// 5-7. REMEDIATION. The failing verdict routes to the producer, which is
	// what makes the loop recoverable rather than parked forever waiting for
	// forge feedback that cannot arrive. Candidate B passes.
	if route := RouteFailure(FailureVerification); route != RouteProviderRemediation {
		t.Fatalf("a verification failure routes to %q, so no remediation is ever planned", route)
	}
	recordCandidateAndAssurance(t, fixture, producer, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, producer, "bbbbbbbbbbbb")
	fixture.reconcile(t)

	// 8. The producer settles ACCEPTED now, on the evidence rather than on the
	// wait reason - the run's disposition and reason never changed.
	if state := planStageState(t, fixture, "implementation").State; state != PlanStageCompleted {
		t.Fatalf("the producer did not settle accepted on a passing verdict: %q", state)
	}

	// 9-10. THE REVIEWER IS RELEASED AGAINST CANDIDATE B, EXACTLY.
	fixture.reconcile(t)
	reviewProjection := planStageState(t, fixture, "review")
	if reviewProjection.RunID == "" {
		t.Fatal("the reviewer was not released after the producer was accepted")
	}
	if reviewProjection.AgentID == planStageState(t, fixture, "implementation").AgentID {
		t.Fatalf("the reviewer is the producer's own worker: %q", reviewProjection.AgentID)
	}
	assignment, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("the reviewer froze no assignment: found=%v err=%v", found, err)
	}
	subject, ok := reviewSubject(assignment)
	if !ok {
		t.Fatalf("the reviewer froze no upstream candidate: %#v", assignment.Context.UpstreamOutputs)
	}
	if subject.Candidate != "bbbbbbbbbbbb" {
		t.Fatalf("the reviewer was assigned candidate %q, and the accepted work is bbbbbbbbbbbb", subject.Candidate)
	}
	// And the run it was given is bound to that candidate rather than to the
	// trusted base. This is D3: the #119 reviewer's frozen assignment named the
	// right commit and its workspace was the base.
	reviewRun, found, err := fixture.store.Run(reviewProjection.RunID)
	if err != nil || !found {
		t.Fatalf("read the reviewer run: found=%v err=%v", found, err)
	}
	if reviewRun.Plan == nil {
		t.Fatal("the reviewer run carries no plan binding")
	}
	if reviewRun.Plan.BaseRevision != "bbbbbbbbbbbb" &&
		(reviewRun.Plan.UpstreamCandidate == nil || reviewRun.Plan.UpstreamCandidate.Revision != "bbbbbbbbbbbb") {
		t.Fatalf("the reviewer run was not bound to the exact candidate: base=%q upstream=%#v",
			reviewRun.Plan.BaseRevision, reviewRun.Plan.UpstreamCandidate)
	}

	// 11. THE REVIEWER BLOCKS candidate B.
	blockReview(t, fixture, "review", reviewProjection.RunID, []string{"review:needs-changes"})
	settleRunAtGoalState(t, fixture, reviewProjection.RunID, "rrrrrrrrrrrr")
	fixture.reconcile(t)

	if state := planStageState(t, fixture, "review").State; state == PlanStageCompleted {
		t.Fatal("a reviewer that BLOCKED the candidate settled completed")
	}
	// 12. And the gate below it is not satisfied by a blocked review.
	if state := planStageState(t, fixture, "assurance").State; state == PlanStageSatisfied {
		t.Fatal("the assurance gate was satisfied over a blocking review")
	}
	if status := planStatus(t, fixture); status == PlanStateCompleted {
		t.Fatal("the plan reported completed while its reviewer was blocking")
	}

	// 13-14. CANDIDATE C, and the stale review does not survive it. The review
	// that exists is about B; C is different work, and a verdict about B is not
	// evidence about C.
	recordCandidateAndAssurance(t, fixture, producer, "cccccccccccc")
	settleRunAtGoalState(t, fixture, producer, "cccccccccccc")
	fixture.reconcile(t)

	stale := planStageState(t, fixture, "review")
	if stale.Review.Accepted("cccccccccccc", "cccccccccccc") {
		t.Fatal("a verdict about candidate B was read as a verdict about candidate C")
	}
	if state := planStageState(t, fixture, "assurance").State; state == PlanStageSatisfied {
		t.Fatal("the gate stayed satisfied over work nobody reviewed")
	}

	// 15-17. THE REVIEWER RECEIVES CANDIDATE C AND ACCEPTS IT.
	fixture.reconcile(t)
	redone := planStageState(t, fixture, "review")
	if redone.RunID == "" {
		t.Fatal("the reviewer was not performed again against candidate C")
	}
	next, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, redone.Generation, "review")
	if err != nil || !found {
		t.Fatalf("the re-performance froze no assignment: found=%v err=%v", found, err)
	}
	nextSubject, ok := reviewSubject(next)
	if !ok || nextSubject.Candidate != "cccccccccccc" {
		t.Fatalf("the re-performed review judges %#v, and the current work is cccccccccccc", nextSubject)
	}
	acceptReview(t, fixture, "review", redone.RunID)
	settleRunAtGoalState(t, fixture, redone.RunID, "rrrrrrrrrrr2")
	fixture.reconcile(t)

	if state := planStageState(t, fixture, "review").State; state != PlanStageCompleted {
		t.Fatalf("an accepting review did not settle the reviewer stage: %q", state)
	}

	// 18-19. THE GATE BECOMES SATISFIABLE, AND ONLY THEN IS THE PLAN COMPLETE.
	fixture.reconcile(t)
	if state := planStageState(t, fixture, "assurance").State; state != PlanStageSatisfied {
		t.Fatalf("the gate was not satisfied by an accepted review over verified work: %q", state)
	}
	if status := planStatus(t, fixture); status != PlanStateCompleted {
		t.Fatalf("every required obligation is satisfied and the plan reports %q", status)
	}

	// AND IT REPLAYS. Every state above is durable, so a fresh reduction of the
	// journal reaches the same answer - which is what makes this a property of
	// the record rather than of one process's memory.
	replayed, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Stages["review"].State != PlanStageCompleted ||
		replayed.Stages["assurance"].State != PlanStageSatisfied {
		t.Fatalf("the lifecycle did not replay identically: %#v", replayed.Stages)
	}
}

// D1, on its own terms: a required obligation that has no projection entry is
// OUTSTANDING, never satisfied.
//
// This is the arithmetic that reported #119 "completed". The gate had never been
// evaluated, so it was absent from snapshot.Stages entirely, contributed to
// neither the pending count nor the failed count, and the switch fell through to
// completed - while the scheduler simultaneously reported that same gate blocked.
func TestPlanCompletionIsJudgedAgainstTheApprovedDocument(t *testing.T) {
	required := closedLoopStages()
	satisfied := func(states map[string]PlanStageState) PlanSnapshot {
		snapshot := PlanSnapshot{Stages: map[string]PlanStageProjection{}}
		snapshot.Approved = PlanApproval{Status: domain.ApprovalApproved, Revision: 1}
		snapshot.Approval = snapshot.Approved
		for id, state := range states {
			snapshot.Stages[id] = PlanStageProjection{StageID: id, State: state}
		}
		return snapshot
	}
	cases := []struct {
		name   string
		states map[string]PlanStageState
		want   string
	}{
		{
			// The #119 arithmetic, exactly.
			name:   "a required gate that was never projected",
			states: map[string]PlanStageState{"implementation": PlanStageCompleted, "review": PlanStageCompleted},
			want:   PlanStateExecuting,
		},
		{
			name: "a required gate still pending",
			states: map[string]PlanStageState{
				"implementation": PlanStageCompleted, "review": PlanStageCompleted, "assurance": PlanStagePending},
			want: PlanStateExecuting,
		},
		{
			name: "a required stage that failed",
			states: map[string]PlanStageState{
				"implementation": PlanStageFailed, "review": PlanStagePending, "assurance": PlanStagePending},
			want: PlanStateBlocked,
		},
		{
			name: "an invalidated stage is work that has to happen again",
			states: map[string]PlanStageState{
				"implementation": PlanStageCompleted, "review": PlanStageInvalidated, "assurance": PlanStagePending},
			want: PlanStateExecuting,
		},
		{
			name: "everything required is satisfied",
			states: map[string]PlanStageState{
				"implementation": PlanStageCompleted, "review": PlanStageCompleted, "assurance": PlanStageSatisfied},
			want: PlanStateCompleted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planState(required, satisfied(tc.states)); got != tc.want {
				t.Fatalf("planState = %q, want %q", got, tc.want)
			}
		})
	}
}

// A plan whose required graph cannot be read is not complete. Answering
// "completed" for it would be the same silence-as-success in a different place.
func TestAPlanWithNoRequiredGraphIsNotComplete(t *testing.T) {
	snapshot := PlanSnapshot{
		Stages:   map[string]PlanStageProjection{},
		Approved: PlanApproval{Status: domain.ApprovalApproved, Revision: 1},
		Approval: PlanApproval{Status: domain.ApprovalApproved, Revision: 1},
	}
	if got := planState(nil, snapshot); got == PlanStateCompleted {
		t.Fatal("a plan with no readable required graph reported completed")
	}
}

// A producer whose remediation budget is EXHAUSTED fails its stage.
//
// The failing-verdict arm of stageAcceptance deliberately settles nothing: the
// runtime is about to remediate, and terminalizing there would end a stage that
// was going to be fixed. That is only correct because the run terminalizes
// ITSELF when the budget runs out, and the terminal arm then settles the stage
// failed on the run's own verdict. Without this test the first half would read
// as "a failed verification never fails a stage", which is the opposite of what
// is intended.
func TestAProducerThatRanOutOfRemediationFailsItsStage(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	fixture.approve(t)
	fixture.reconcile(t)

	producer := planStageState(t, fixture, "implementation").RunID
	recordFailedAssurance(t, fixture, producer, "aaaaaaaaaaaa")

	// Unsettled while the run can still act on the verdict.
	settleRunAtGoalState(t, fixture, producer, "aaaaaaaaaaaa")
	fixture.reconcile(t)
	if state := planStageState(t, fixture, "implementation").State; state == PlanStageFailed {
		t.Fatal("the stage failed while its run still had remediation to do")
	}

	// The run gives up. A terminal run is a terminal stage, and the reviewer
	// below it stays unreleased.
	run, found, err := fixture.store.Run(producer)
	if err != nil || !found {
		t.Fatalf("read the producer run: found=%v err=%v", found, err)
	}
	run.Disposition, run.Reason = Failed, "attempts_exhausted"
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	fixture.reconcile(t)

	if state := planStageState(t, fixture, "implementation").State; state != PlanStageFailed {
		t.Fatalf("an exhausted producer did not fail its stage: %q", state)
	}
	if review := planStageState(t, fixture, "review"); review.RunID != "" {
		t.Fatalf("the reviewer was released by a failed producer: %#v", review)
	}
	if status := planStatus(t, fixture); status != PlanStateBlocked {
		t.Fatalf("a plan with a failed required stage reports %q, want %q", status, PlanStateBlocked)
	}
}
