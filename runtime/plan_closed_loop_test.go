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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	// The reviewer runs through a provider that emits a real structured result
	// to the runtime-owned path: BLOCK on the first invocation, ACCEPT on the
	// second. Everything after that is the production admission path.
	reviewer := reviewerEngine(t, fixture,
		ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
			Findings: []ReviewerFinding{{Signature: "review:needs-changes", Detail: "the change is incomplete"}}},
		ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
			Reason: "every acceptance obligation was observed satisfied"},
	)
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
	produceCandidate(t, fixture, producer, "package a\n", false)
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
	candidateB := produceCandidate(t, fixture, producer, "package b\n", true)
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
	subject, ok := fixture.reconciler.reviewSubject(assignment)
	if !ok {
		t.Fatalf("the reviewer froze no upstream candidate: %#v", assignment.Context.UpstreamOutputs)
	}
	if subject.Candidate != candidateB {
		t.Fatalf("the reviewer was assigned candidate %q, and the accepted work is %s", subject.Candidate, candidateB)
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
	if reviewRun.Plan.BaseRevision != candidateB &&
		(reviewRun.Plan.UpstreamCandidate == nil || reviewRun.Plan.UpstreamCandidate.Revision != candidateB) {
		t.Fatalf("the reviewer run was not bound to the exact candidate: base=%q upstream=%#v",
			reviewRun.Plan.BaseRevision, reviewRun.Plan.UpstreamCandidate)
	}
	// 11. THE REVIEWER BLOCKS candidate B - through the production path. The
	// reviewer provider wrote a structured result to the runtime-owned path it
	// was given, the adapter read it, and the runtime admitted it against the
	// frozen assignment. Nothing is injected here: an injected event would
	// prove a state machine production cannot enter, which is the exact shape
	// of the gap this regression exists to close.
	driveReviewer(t, fixture, reviewProjection.RunID)
	if len(reviewer.Reviewed) == 0 {
		t.Fatal("the reviewer invocation was given no structured result path")
	}
	// AND ITS WORKSPACE HOLDS THE CANDIDATE IT WAS ASSIGNED. This is the #119
	// defect measured directly: there, the reviewer's frozen assignment named
	// the right commit and its workspace was the trusted base.
	assertReviewerSawCandidate(t, fixture, reviewProjection.RunID, producer, candidateB)
	settleRunAtGoalState(t, fixture, reviewProjection.RunID, "rrrrrrrrrrrr")
	fixture.reconcile(t)

	blocked := planStageState(t, fixture, "review")
	if blocked.Review == nil || blocked.Review.Verdict != StageReviewBlocked {
		t.Fatalf("the blocking verdict was not admitted through the production path: %#v", blocked.Review)
	}
	if blocked.Review.Candidate != candidateB {
		t.Fatalf("the admitted verdict is bound to %q, and the reviewer was given %s", blocked.Review.Candidate, candidateB)
	}

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
	candidateC := produceCandidate(t, fixture, producer, "package c\n", true)
	fixture.reconcile(t)

	stale := planStageState(t, fixture, "review")
	if stale.Review.Accepted(candidateC, candidateC) {
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
	nextSubject, ok := fixture.reconciler.reviewSubject(next)
	if !ok || nextSubject.Candidate != candidateC {
		t.Fatalf("the re-performed review judges %#v, and the current work is %s", nextSubject, candidateC)
	}
	driveReviewer(t, fixture, redone.RunID)
	settleRunAtGoalState(t, fixture, redone.RunID, "rrrrrrrrrrr2")
	fixture.reconcile(t)

	accepted := planStageState(t, fixture, "review")
	if accepted.Review == nil || accepted.Review.Verdict != StageReviewAccepted {
		t.Fatalf("the accepting verdict was not admitted through the production path: %#v", accepted.Review)
	}
	if accepted.Review.Candidate != candidateC {
		t.Fatalf("the accepting verdict is bound to %q, and the reviewer was given %s", accepted.Review.Candidate, candidateC)
	}

	if state := planStageState(t, fixture, "review").State; state != PlanStageCompleted {
		t.Fatalf("an accepting review did not settle the reviewer stage: %q", state)
	}

	// 18-19. THE GATE BECOMES SATISFIABLE, AND ONLY THEN IS THE PLAN COMPLETE.
	// Ticks, because the gate is evaluated after the stage it depends on has
	// settled: one pass records the settlement and the next reads it.
	for i := 0; i < 6; i++ {
		fixture.reconcile(t)
		if planStageState(t, fixture, "assurance").State == PlanStageSatisfied {
			break
		}
	}
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

// reviewerEngine registers a runtime whose execution provider is a reviewer
// that writes real structured results, and returns it so a test can assert the
// channel was used.
//
// The plan reconciler builds an engine from the frozen assignment's agent id,
// so registering one under "claude" is enough to make the reviewer stage - and
// only the reviewer stage - run through it.
func reviewerEngine(t *testing.T, fixture *planRunFixture, verdicts ...ReviewerResult) *FakeReviewerProvider {
	t.Helper()
	provider := NewFakeReviewerProvider(verdicts...)
	deps := fixture.deps
	deps.Provider = provider
	deps.Agent = ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}
	fixture.engines["claude"] = fixture.newRuntime(deps)
	return provider
}

// driveReviewer runs the reviewer's own EngineeringRun far enough to perform its
// invocation, which is what produces and admits the structured result.
//
// The plan reconciler starts child runs; it does not drive them - `serve` does
// that separately - so a plan-level test that never drove the run would never
// reach the provider at all.
func driveReviewer(t *testing.T, fixture *planRunFixture, runID string) {
	t.Helper()
	engine, ok := fixture.engines["claude"]
	if !ok {
		t.Fatal("no reviewer engine is registered")
	}
	// Driven to its GOAL STATE rather than merely to its invocation: a reviewer
	// run also verifies its own workspace, and the assurance gate below the
	// stage reads that evidence. Stopping at the invocation would leave the
	// gate with nothing to be satisfied by, which is a fixture artefact rather
	// than a lifecycle fact.
	invoked := false
	for i := 0; i < 12; i++ {
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatalf("drive reviewer run %s: %v", runID, err)
		}
		events, err := fixture.store.Events(runID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == EventExecutionCompleted {
				invoked = true
			}
		}
		run, found, err := fixture.store.Run(runID)
		if err != nil || !found {
			t.Fatalf("read reviewer run: found=%v err=%v", found, err)
		}
		if _, stopped := runSettled(run); stopped && invoked {
			return
		}
	}
	if invoked {
		return
	}
	t.Fatalf("the reviewer run %s never performed an invocation", runID)
}

// produceCandidate commits real content in a producer run's own runtime-owned
// workspace and records it as that run's verified candidate.
//
// The heads are REAL commits rather than twelve-character placeholders because
// the reviewer's workspace is now materialized from them: a synthetic head
// cannot be fetched, and a test that used one would be asserting the lifecycle
// while silently skipping the transfer it depends on.
func produceCandidate(t *testing.T, fixture *planRunFixture, runID, content string, passes bool) string {
	t.Helper()
	// The producer's workspace is cloned by its own run, so the run is driven
	// far enough to own one before anything is committed into it. A fixture
	// that wrote into a directory the runtime had not created would be
	// asserting against a workspace production never makes.
	dir := candidateDir(fixture.stateDir, runID)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		if _, err := fixture.runtime.Reconcile(context.Background(), runID); err != nil {
			t.Fatalf("drive producer run %s: %v", runID, err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the producer run %s never created a workspace: %v", runID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "candidate " + content}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	head, headErr := gitOutput(dir, "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatal(headErr)
	}
	tree, treeErr := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if treeErr != nil {
		t.Fatal(treeErr)
	}
	head, tree = strings.TrimSpace(head), strings.TrimSpace(tree)
	// The commit is made reachable from the fixture's origin as well.
	//
	// Which ROUTE delivers a candidate to a downstream stage - a clone of a
	// published commit, or a local transfer from the producer's workspace - is
	// the transport's business and is proven directly in
	// candidate_transport_test.go. This scenario is about the LIFECYCLE, so it
	// makes both routes available and then asserts the only thing that matters
	// here: the reviewer's workspace is the exact tree it was assigned.
	remote, err := GovernedRemote(fixture.origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteGit(dir, remote, nil).run(
		"push", remote.URL, "HEAD:refs/heads/"+plannedCandidateBranch(runID)); err != nil {
		t.Fatalf("publish the producer candidate: %v", err)
	}
	appendRunEventFor(t, fixture, runID, head, EventCandidateCommitted, CandidateCommittedPayload{
		Commit: head, Tree: tree, PathCount: 1, PathsDigest: strings.Repeat("c", 64),
	})
	appendRunEventFor(t, fixture, runID, head, EventAssuranceObserved, AssuranceObservedPayload{
		ProviderID: "go", VerifierDefinition: strings.Repeat("d", 64), Passed: passes,
		FailureClass: map[bool]FailureClass{false: FailureVerification, true: ""}[passes],
		Commit:       head, Tree: tree, Bundle: Ref{ID: "evidence-" + head, Revision: head},
	})
	// The INDEPENDENT semantic verdict is refreshed with it. A producer that
	// moved to a new candidate re-verifies; leaving the old verdict behind
	// would make it stale, and a gate correctly refuses to be proved by a
	// verdict about a tree that no longer exists.
	appendRunEventFor(t, fixture, runID, head, EventSemanticAssuranceObserved, AssuranceObservedPayload{
		ProviderID: semanticProviderID, VerifierDefinition: SemanticVerifierDefinition(),
		Passed: passes, Commit: head, Tree: tree, Semantic: true,
	})
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("read producer run: found=%v err=%v", found, err)
	}
	run.Disposition, run.Reason = Waiting, ReasonGoalStateReached
	run.Candidate.Revision, run.Candidate.Tree = head, tree
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	return head
}

// assertReviewerSawCandidate proves a reviewer's workspace is the exact upstream
// candidate, by whichever route the runtime delivered it.
func assertReviewerSawCandidate(t *testing.T, fixture *planRunFixture, reviewRunID, producerRunID, candidate string) {
	t.Helper()
	producer, found, err := fixture.store.Run(producerRunID)
	if err != nil || !found {
		t.Fatalf("read the producer run: found=%v err=%v", found, err)
	}
	if producer.Candidate.Revision != candidate {
		t.Fatalf("the producer is at %s and the reviewer was assigned %s", producer.Candidate.Revision, candidate)
	}
	// The reviewer's workspace CONTAINS the candidate. Exact-head equality is
	// the right question before the invocation and the wrong one after it: a
	// reviewer may commit review notes of its own, so its head legitimately
	// moves past the subject. What must remain true is that the subject is in
	// its history - a workspace at the trusted base, which is what #119
	// produced, fails this.
	dir := candidateDir(fixture.stateDir, reviewRunID)
	if _, err := runGit(dir, "merge-base", "--is-ancestor", candidate, "HEAD"); err != nil {
		head, _ := gitOutput(dir, "rev-parse", "HEAD")
		t.Fatalf("the reviewer workspace does not contain the candidate it was assigned: head=%s candidate=%s",
			strings.TrimSpace(head), candidate)
	}
	// And the run is DURABLY recorded as based on it, so the binding survives a
	// restart rather than living in a workspace nobody re-checks.
	reviewRun, found, err := fixture.store.Run(reviewRunID)
	if err != nil || !found {
		t.Fatalf("read the reviewer run: found=%v err=%v", found, err)
	}
	if reviewRun.Base.Revision != candidate {
		t.Fatalf("the reviewer run is based on %q and the candidate it was assigned is %s",
			reviewRun.Base.Revision, candidate)
	}
}

// plannedCandidateBranch is the ref a fixture publishes a producer candidate
// under. It mirrors the runtime's own naming closely enough to be recognizable
// without coupling the test to it.
func plannedCandidateBranch(runID string) string { return "zenchron/" + runID }

// A verdict admitted twice is ONE verdict, and a CONFLICTING second verdict for
// the same invocation is refused.
//
// Both halves matter. An operation retried after the append succeeded must not
// fail on its own earlier success, and one invocation must not be able to answer
// twice - a second answer is not an update.
func TestAVerdictIsIdempotentAndAConflictingOneIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	reviewerEngine(t, fixture,
		ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
			Findings: []ReviewerFinding{{Signature: "review:defect"}}})
	fixture.approve(t)
	fixture.reconcile(t)

	producer := planStageState(t, fixture, "implementation").RunID
	candidate := produceCandidate(t, fixture, producer, "package b\n", true)
	fixture.reconcile(t)
	fixture.reconcile(t)

	review := planStageState(t, fixture, "review")
	driveReviewer(t, fixture, review.RunID)

	recorded := planStageReviewEvents(t, fixture)
	if len(recorded) != 1 {
		t.Fatalf("one invocation produced %d verdicts", len(recorded))
	}

	// The SAME verdict again is a no-op: same attempt, same content.
	engine := fixture.engines["claude"]
	if _, err := engine.Reconcile(context.Background(), review.RunID); err != nil {
		t.Fatal(err)
	}
	if again := planStageReviewEvents(t, fixture); len(again) != 1 {
		t.Fatalf("re-admitting the identical verdict recorded %d events", len(again))
	}

	// A DIFFERENT verdict for the same invocation is refused rather than
	// replacing the first.
	state, err := engine.load(review.RunID)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := engine.planStage(state)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := fixture.store.Operations(review.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var invoke RunOperation
	for _, operation := range operations {
		if operation.Kind == OpExecutionInvoke {
			invoke = operation
		}
	}
	conflicting := ExecutionResult{ProviderID: "claude", Review: &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
	}}
	if err := engine.admitReview(state, stage, conflicting, invoke); err == nil {
		t.Fatal("a second, contradicting verdict for one invocation was admitted")
	}
	if final := planStageReviewEvents(t, fixture); len(final) != 1 || final[0].Verdict != StageReviewBlocked {
		t.Fatalf("the conflicting verdict changed the record: %+v", final)
	}

	// AND IT REPLAYS. The admitted verdict is durable, so a fresh reduction of
	// the journal reaches the same answer.
	replayed, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := replayed.Stages["review"].Review; got == nil || got.Verdict != StageReviewBlocked || got.Candidate != candidate {
		t.Fatalf("the admitted verdict did not replay: %+v", got)
	}
	// A new generation and a new authorizing operation each own their answer.
	state.run.Plan.Generation++
	if err := engine.admitReview(state, stage, conflicting, invoke); err != nil {
		t.Fatal(err)
	}
	invoke.ID += "-next"
	if err := engine.admitReview(state, stage, conflicting, invoke); err != nil {
		t.Fatal(err)
	}
	if len(planStageReviewEvents(t, fixture)) != 3 {
		t.Fatal("distinct invocations collided")
	}
	if err := appendPlanEvent(fixture.store, engine.deps.Clock.Now(), fixture.plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
		StageID: "review", Revision: fixture.plan.Revision,
		Outcome: "invalidated", Reason: "test retirement",
	}); err != nil {
		t.Fatal(err)
	}
	invoke.ID += "-retired"
	if err := engine.admitReview(state, stage, conflicting, invoke); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired reviewer was not explicitly refused: %v", err)
	}

}

// planStageReviewEvents is every admitted verdict on this plan's stream.
func planStageReviewEvents(t *testing.T, fixture *planRunFixture) []PlanStageReviewedPayload {
	t.Helper()
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []PlanStageReviewedPayload
	for _, event := range events {
		if event.Type != EventPlanStageReviewed {
			continue
		}
		var payload PlanStageReviewedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// A reviewer stage assigned to a worker that cannot return a structured verdict
// BLOCKS at resolution, before any invocation is spent.
//
// It is the same rule D4 applies to a missing toolchain: do not dispatch an
// assignment whose mandatory output the selected provider cannot produce. A
// reviewer that can never emit a verdict would run, succeed, and leave its
// stage unable ever to settle.
func TestAReviewerProviderWithoutTheVerdictProtocolIsRefusedBeforeDispatch(t *testing.T) {
	// No independence requirement on the reviewer, so the verdict protocol is
	// the ONLY thing that can block it. Independence outranks it in the block
	// precedence - correctly, it is the more actionable answer - and a fixture
	// carrying both would assert nothing about this rule.
	stages := closedLoopStages()
	for i := range stages {
		if stages[i].ID == "review" {
			stages[i].Independence = nil
		}
	}
	fixture := newPlanRunFixture(t, stages)
	agents := planAgents()
	for i := range agents {
		agents[i].StructuredVerdicts = false
	}
	fixture.service.Agents = agents

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := fixture.service.Resolve(fixture.plan, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, block := range resolution.Blocked {
		if block.StageID == "review" {
			blocked = true
			if !strings.Contains(block.Reason, "structured verdict") {
				t.Fatalf("the reviewer stage blocked for the wrong reason: %q", block.Reason)
			}
		}
	}
	if !blocked {
		t.Fatalf("a reviewer stage resolved to a worker that cannot produce a verdict: %+v", resolution.Assignments)
	}
}

// AN INVOCATION THAT DID NOT COMPLETE CONTRIBUTES NO VERDICT.
//
// A reviewer can write its result and then die: overrun its wall bound, be
// cancelled, be cut off by a controller shutdown, or simply exit non-zero. The
// file is on disk either way, and admitting it would let an unfinished
// invocation produce a finished answer - the exact failure this whole change
// exists to remove, reproduced inside its own repair.
//
// Every class is checked together because the execution model already
// normalizes them into "the invocation failed"; the point is that NONE of them
// is a route to durable acceptance.
func TestAVerdictFromAnIncompleteInvocationIsNeverAdmitted(t *testing.T) {
	for _, class := range []FailureClass{
		FailureExecutionIncomplete,
		FailureTransientProvider,
		FailureCompileTest,
	} {
		t.Run(string(class), func(t *testing.T) {
			fixture := newPlanRunFixture(t, closedLoopStages())
			inner := NewFakeReviewerProvider(ReviewerResult{
				SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
			})
			deps := fixture.deps
			deps.Provider = &dyingReviewerProvider{FakeReviewerProvider: inner, class: class}
			deps.Agent = ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}
			fixture.engines["claude"] = fixture.newRuntime(deps)

			fixture.approve(t)
			fixture.reconcile(t)
			producer := planStageState(t, fixture, "implementation").RunID
			produceCandidate(t, fixture, producer, "package b\n", true)
			fixture.reconcile(t)
			fixture.reconcile(t)

			review := planStageState(t, fixture, "review")
			if review.RunID == "" {
				t.Fatal("the reviewer stage created no run")
			}
			engine := fixture.engines["claude"]
			for i := 0; i < 6; i++ {
				if _, err := engine.Reconcile(context.Background(), review.RunID); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}
			// The reviewer DID write a result - this test is about what the
			// lifecycle does with it, not about a reviewer that stayed silent.
			if len(inner.Reviewed) == 0 {
				t.Fatal("the reviewer was never given a result path, so this proves nothing")
			}

			after := planStageState(t, fixture, "review")
			if after.Review != nil {
				t.Fatalf("a verdict from a %s invocation was admitted: %+v", class, after.Review)
			}
			if after.State == PlanStageCompleted {
				t.Fatal("the reviewer stage was accepted without an admitted verdict")
			}
			if gate := planStageState(t, fixture, "assurance"); gate.State == PlanStageSatisfied {
				t.Fatal("the assurance gate was satisfied over a verdict nobody admitted")
			}
			if status := planStatus(t, fixture); status == PlanStateCompleted {
				t.Fatal("the plan reported completed over a verdict nobody admitted")
			}
		})
	}
}

// dyingReviewerProvider writes its verdict and then reports that the invocation
// failed, which is what a reviewer cut off after answering looks like.
type dyingReviewerProvider struct {
	*FakeReviewerProvider
	class FailureClass
}

func (p *dyingReviewerProvider) Execute(ctx context.Context, r ExecutionRequest) (ExecutionResult, error) {
	result, err := p.FakeReviewerProvider.Execute(ctx, r)
	if r.ReviewerResultPath != "" {
		result.Outcome = OperationFailed
		result.Failure = &ProviderFailure{Classification: p.class}
		// A failed invocation carries no verdict out of the adapter either: the
		// production adapter returns before reading the file at all.
		result.Review = nil
	}
	return result, err
}

// DIVERGENT SIBLINGS: two producers, neither containing the other, one reviewer.
//
// This is the shape the #119 planner produced at revision 3, and the shape that
// used to take whichever candidate the loop visited last. The reviewer received
// one of the two trees, the other producer's work was never in its workspace,
// and its ACCEPT satisfied the gate for material it never saw. An independent
// review covering half a change is worse than none, because it reports as if it
// covered all of it.
//
// There is no composition here and deliberately so: the runtime cannot merge
// two candidates into one reviewable subject, so it refuses.
func TestAReviewerWithDivergentUpstreamCandidatesIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, twoProducerStages())
	reviewerEngine(t, fixture,
		ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	fixture.approve(t)
	fixture.reconcile(t)

	first := planStageState(t, fixture, "producer-a").RunID
	second := planStageState(t, fixture, "producer-b").RunID
	if first == "" || second == "" {
		t.Fatalf("both producers must run: %q %q", first, second)
	}
	// Siblings from one base: neither commit is an ancestor of the other.
	produceCandidate(t, fixture, first, "package a\n", true)
	produceCandidate(t, fixture, second, "package b\n", true)
	for i := 0; i < 4; i++ {
		fixture.reconcile(t)
	}

	review := planStageState(t, fixture, "review")
	if review.RunID != "" {
		t.Fatalf("the reviewer was invoked against one of two divergent candidates: %#v", review)
	}
	if review.Review != nil {
		t.Fatalf("a verdict was admitted for a subject nobody could identify: %+v", review.Review)
	}
	if gate := planStageState(t, fixture, "assurance"); gate.State == PlanStageSatisfied {
		t.Fatal("the gate was satisfied without any review at all")
	}
	if status := planStatus(t, fixture); status == PlanStateCompleted {
		t.Fatal("the plan reported completed with its reviewer never invoked")
	}
	// And the refusal is EXPLICIT rather than a silent stall.
	report := fixture.reconcile(t)
	blocked := ""
	for _, block := range report.Blocked {
		if block.StageID == "review" {
			blocked = block.Reason
		}
	}
	if !strings.Contains(blocked, "distinct upstream candidates") {
		t.Fatalf("the reviewer stage did not block with a reason naming the composition it cannot do: %q", blocked)
	}
}

// twoProducerStages is the #119 revision-3 shape: two independent implementers
// under one independent reviewer, and the gate the contract's claim requires.
func twoProducerStages() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "producer-a", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the first half.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "producer-b", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the second half.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"producer-a", "producer-b"}, Objective: "Review the combined work.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification},
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"producer-a", "producer-b"},
			}},
		{ID: "assurance", Kind: domain.StageAssuranceGate, DependsOn: []string{"review"},
			RequiredClaims: []string{"verification"}},
	}
}

// UNKNOWN GOVERNANCE STATE FAILS CLOSED.
//
// An approved revision that cannot be read leaves the plan's required
// obligations unknown. Falling back to the stored head revision - which may be
// a newer proposal nobody approved, with different or fewer obligations - would
// judge completion against a document nobody authorized, which is the same
// silence-as-success the approved-graph rule exists to remove.
func TestAnUnreadableApprovedRevisionNeverReportsCompleted(t *testing.T) {
	fixture := newPlanRunFixture(t, closedLoopStages())
	fixture.approve(t)

	// A NEWER revision is stored and unapproved, which is the dangerous shape:
	// the head document exists and is not the one that governs.
	next := fixture.plan
	next.Revision = fixture.plan.Revision + 1
	next.Stages = next.Stages[:1]
	previous := fixture.plan.Revision
	next.Provenance.PreviousRevision = &previous
	digest, err := next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	next.Digest = digest
	if _, err := fixture.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	// The approval still stands in the journal, and the revision it named is
	// gone from the store.
	if _, err := fixture.store.db.Exec(
		`DELETE FROM plan_revisions WHERE plan_id = ? AND revision = ?`,
		fixture.plan.ID, fixture.plan.Revision); err != nil {
		t.Fatal(err)
	}
	summaries := summarizePlans(fixture.store)
	if len(summaries) != 1 {
		t.Fatalf("expected one plan summary, got %d", len(summaries))
	}
	summary := summaries[0]
	if summary.State == PlanStateCompleted {
		t.Fatal("a plan whose approved revision cannot be read reported completed")
	}
	if summary.State != PlanStateBlocked {
		t.Fatalf("state %q, want %q", summary.State, PlanStateBlocked)
	}
	if !strings.Contains(summary.Error, "could not be read") {
		t.Fatalf("the summary does not say why the graph is unknown: %q", summary.Error)
	}
}
