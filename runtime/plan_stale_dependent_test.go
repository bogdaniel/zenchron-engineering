package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A completed independent review is not reusable once the work it reviewed has
// moved.
//
// Reopening the gate above it is not enough. The shape is
// `implementation -> independent review -> gate`: the reviewer's frozen
// assignment names the exact candidate it consumed, and a producer run at
// goal_state_reached is not finished - reviewer feedback re-activates it and it
// produces a different candidate. The gate reopens because its proof names the
// producer's run; the REVIEW stayed completed, so the gate could be satisfied
// again by an independent review that was never performed on the work now
// being gated.
func TestACompletedReviewIsInvalidatedWhenTheWorkItReviewedMoves(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
		{ID: "assurance", Kind: domain.StageAssuranceGate, DependsOn: []string{"review"},
			RequiredClaims: []string{"verification"}},
	})
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

	// Candidate A, verified, and the producer parks at goal state - which is
	// not "finished".
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
	// The reviewer's frozen assignment must actually name what it consumed, or
	// this test is asserting nothing.
	assignment, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(assignment.Context.UpstreamOutputs) == 0 || assignment.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the review assignment does not name the candidate it consumed: %#v", assignment.Context.UpstreamOutputs)
	}

	recordCandidateAndAssurance(t, fixture, review, "rrrrrrrrrrrr")
	settleRunAtGoalState(t, fixture, review, "rrrrrrrrrrrr")
	fixture.reconcile(t)
	fixture.reconcile(t)

	satisfied, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if satisfied.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the review did not complete against candidate A: %#v", satisfied.Stages["review"])
	}
	if satisfied.Stages["assurance"].State != PlanStageSatisfied {
		t.Fatalf("the gate was not satisfied: %#v", satisfied.Stages["assurance"])
	}

	// Feedback re-activates the producer, which moves to candidate B. The
	// review that was performed was about A.
	//
	// The run's own record moves with the journal, exactly as the runtime's
	// reconcile of that run does it - the event and the row are written by the
	// same pass, and the plan compares against the row the assignment froze.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")
	fixture.reconcile(t)

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].State == PlanStageCompleted {
		t.Fatal("the review stayed completed after the work it reviewed was replaced")
	}
	if after.Stages["assurance"].State == PlanStageSatisfied {
		t.Fatal("the gate stayed satisfied over a review that was never performed on the current work")
	}

	// It is PERFORMED AGAIN under the same approved plan: a new execution
	// generation, its own assignment and its own run. The approved obligation -
	// this role, this profile version and digest, this worker, this trust
	// ceiling - did not change, so nothing is re-planned and nobody is asked to
	// approve anything.
	fixture.reconcile(t)
	redone, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redone.Stages["review"].Generation != 1 {
		t.Fatalf("the stage is at generation %d, want 1", redone.Stages["review"].Generation)
	}
	rerun := redone.Stages["review"].RunID
	if rerun == "" || rerun == review {
		t.Fatalf("the re-performance adopted the discarded run: %q", rerun)
	}
	// A stage that is RUNNING is not invalidated and has no reason. Both belong
	// to the performance that ended, and leaving them showed an operator
	// `invalidated_under` beside work that is under way.
	if redone.Stages["review"].InvalidatedUnder != 0 || redone.Stages["review"].Reason != "" {
		t.Fatalf("the new performance still carries the last one's invalidation: %#v", redone.Stages["review"])
	}
	next, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 1, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the re-performance was not frozen against anything")
	}
	if got := next.Context.UpstreamOutputs[0].Candidate; got != "bbbbbbbbbbbb" {
		t.Fatalf("the re-performance consumes %q, which is the candidate whose replacement caused it", got)
	}
	// Same obligation, exactly.
	previous, _, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if next.Role != previous.Role || next.Agent != previous.Agent ||
		next.TrustRequirement != previous.TrustRequirement ||
		next.InvocationMode != previous.InvocationMode ||
		next.Profile.ID != previous.Profile.ID ||
		next.Profile.Version != previous.Profile.Version ||
		next.Profile.Digest != previous.Profile.Digest {
		t.Fatalf("the re-performance changed the approved obligation:\n%#v\n%#v", previous, next)
	}
	// The discarded run is RETIRED: attributed and stopped, not left executing
	// work nobody will read.
	retired := false
	for _, runID := range redone.RetiredRuns {
		retired = retired || runID == review
	}
	if !retired {
		t.Fatalf("the invalidated stage's run was not retired: %#v", redone.RetiredRuns)
	}
	// Stopped for what actually happened. Nothing superseded this run: the
	// revision that created it is still governing, and its stage is simply
	// being performed again.
	stoppedRun, found, err := fixture.store.Run(review)
	if err != nil || !found {
		t.Fatalf("read the retired run: found=%v err=%v", found, err)
	}
	if stoppedRun.Reason != "plan_stage_reperformed" {
		t.Fatalf("the retired run was cancelled as %q, which says a revision replaced it", stoppedRun.Reason)
	}
	// And what it goes on spending is attributed TO ITS STAGE. The stage no
	// longer names the run, so an attribution that took the stage id from the
	// projection recorded none, and the per-stage totals a profile's own budget
	// is enforced against undercounted every retired run.
	spentBefore := redone.StageConsumed["review"].ProviderInvocations
	recordExecutionAttempts(t, fixture, review, 3)
	fixture.reconcile(t)
	spent, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spent.StageConsumed["review"].ProviderInvocations <= spentBefore {
		t.Fatalf("the retired run spent more and the review stage's total is %d, unchanged from %d",
			spent.StageConsumed["review"].ProviderInvocations, spentBefore)
	}

	// And it settles. Further passes neither re-invalidate nor start anything
	// else: one replacement, not a loop.
	for i := 0; i < 3; i++ {
		fixture.reconcile(t)
	}
	settled, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Stages["review"].RunID != rerun {
		t.Fatalf("the stage was restarted again: %q then %q", rerun, settled.Stages["review"].RunID)
	}
	if settled.Stages["review"].Generation != 1 {
		t.Fatalf("the stage advanced to generation %d without its input moving again", settled.Stages["review"].Generation)
	}
	settledEvents := 0
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventPlanStageSettled {
			continue
		}
		var payload PlanStageSettledPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.StageID == "review" && payload.Outcome == "invalidated" {
			settledEvents++
		}
	}
	if settledEvents != 1 {
		t.Fatalf("the review was invalidated %d times", settledEvents)
	}
	// The record of what the FIRST performance consumed is untouched: it
	// happened, and the journal says what it was.
	if previous.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the record of the first performance was rewritten: %#v", previous.Context.UpstreamOutputs)
	}
}

// settleRunAtGoalState parks a child run where a producer waits: finished with
// what it was asked to do, still live and still able to move.
func settleRunAtGoalState(t *testing.T, fixture *planRunFixture, runID, head string) {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	run.Disposition = Waiting
	run.Reason = "goal_state_reached"
	run.Candidate.Revision, run.Candidate.Tree = head, head
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
}

// The invalidation reason is a JOURNAL FIELD, and production ids overflow it.
//
// Two full 40-hex candidate heads plus a stage id is 186 bytes before the id.
// The journal refuses a field above 200, so with ordinary stage ids the append
// was refused, the sweep errored, and the reconciler failed on that plan every
// tick, forever. The earlier test passed only because its heads were twelve
// characters long - a regression test that stops short of production sizes is
// how this ships green.
func TestTheInvalidationReasonFitsTheJournal(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation-core", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "independent-review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation-core"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation-core"].RunID

	// Real commit-sized heads, not twelve characters. The reason names both of
	// them and the upstream stage id: 76 + 40 + 12 + 19 + 18 + 40 = 205 bytes,
	// against a 200-byte field bound.
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	recordCandidateAndAssurance(t, fixture, implementation, first)
	settleRunAtGoalState(t, fixture, implementation, first)
	fixture.reconcile(t)

	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Stages["independent-review"].RunID
	if review == "" {
		t.Fatal("the review stage created no run")
	}
	recordCandidateAndAssurance(t, fixture, review, strings.Repeat("r", 40))
	settleRunAtGoalState(t, fixture, review, strings.Repeat("r", 40))
	fixture.reconcile(t)
	fixture.reconcile(t)

	recordCandidate(t, fixture, implementation, second)
	settleRunAtGoalState(t, fixture, implementation, second)

	// The whole assertion: this pass does not error.
	if _, err := fixture.reconciler.Reconcile(context.Background(), fixture.plan.ID); err != nil {
		t.Fatalf("the invalidation could not be journalled: %v", err)
	}
	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The stage was invalidated and is being performed again, so the state has
	// already moved on - a running stage carries no invalidation. The
	// GENERATION is what survives it, and it advances in one place only: the
	// fold of a revision-scoped invalidation. What this test is about is the
	// RECORD, which had to be appendable at all.
	if after.Stages["independent-review"].Generation != 1 {
		t.Fatalf("the invalidation was not recorded: %#v", after.Stages["independent-review"])
	}
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != EventPlanStageSettled {
			continue
		}
		var payload PlanStageSettledPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Outcome != "invalidated" {
			continue
		}
		found = true
		if len(payload.Reason) > maxPayloadFieldBytes {
			t.Fatalf("the journalled reason is %d bytes", len(payload.Reason))
		}
	}
	if !found {
		t.Fatal("no invalidation was journalled")
	}
}

// A crash between the two settle appends is re-derived, not lost.
//
// Invalidating a stage and invalidating its dependents are separate appends. A
// crash in between left the dependent COMPLETED under a dependency that had
// been invalidated, and nothing brought it back: the head-movement sweep only
// looks at completed stages whose upstream moved, and by then the root was no
// longer completed and the dependent's own upstream had never moved. The
// propagation is re-derived from durable state every pass instead.
func TestPropagationIsRederivedAfterACrashBetweenAppends(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")
	fixture.reconcile(t)
	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.Stages["review"].RunID
	recordCandidateAndAssurance(t, fixture, review, "rrrrrrrrrrrr")
	settleRunAtGoalState(t, fixture, review, "rrrrrrrrrrrr")
	fixture.reconcile(t)

	// The crash: the ROOT's invalidation landed and the dependent's did not.
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanStageSettled, PlanStageSettledPayload{
			StageID: "implementation", Outcome: "invalidated", Revision: fixture.plan.Revision,
			Reason: "invalidated by an operator action that then crashed",
		}); err != nil {
		t.Fatal(err)
	}
	crashed, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if crashed.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the fixture does not reproduce the crash window: %#v", crashed.Stages["review"])
	}

	fixture.reconcile(t)
	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].State != PlanStageInvalidated {
		t.Fatalf("the dependent of an invalidated stage stayed usable: %#v", after.Stages["review"])
	}
}

// The revision scope is only meaningful on an invalidation.
//
// It is what makes a stage unperformable until a new revision, so a completed
// or failed settle carrying one would read back as a blocking state nobody
// recorded, and a negative one is not a revision at all.
func TestOnlyAnInvalidationIsScopedToARevision(t *testing.T) {
	for name, payload := range map[string]PlanStageSettledPayload{
		"completed with a revision": {StageID: "s", Outcome: "completed", Revision: 2},
		"failed with a revision":    {StageID: "s", Outcome: "failed", Revision: 2},
		"a negative revision":       {StageID: "s", Outcome: "invalidated", Revision: -1},
	} {
		if err := eventPayloads[EventPlanStageSettled](mustPayload(t, payload)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if err := eventPayloads[EventPlanStageSettled](mustPayload(t, PlanStageSettledPayload{
		StageID: "s", Outcome: "invalidated", Revision: 2,
	})); err != nil {
		t.Fatalf("a revision-scoped invalidation was refused: %v", err)
	}
	if err := eventPayloads[EventPlanStageSettled](mustPayload(t, PlanStageSettledPayload{
		StageID: "s", Outcome: "completed",
	})); err != nil {
		t.Fatalf("an ordinary settle was refused: %v", err)
	}
}

// A re-performance that would change the OBLIGATION is refused.
//
// A new execution generation exists because an execution fact moved. If
// re-resolving would now produce a different role, profile, worker, trust mode
// or independence binding, that is not the same obligation the operator
// approved, and it goes through the approval boundary as a revision. Obligation
// renewal may be automatic; authority change may not.
func TestARePerformanceThatChangesTheObligationIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	plan := fixture.plan
	stage := plan.Stages[0]

	previous := domain.AgentAssignment{
		SchemaVersion: domain.SchemaVersion, ID: "assignment-a", StageID: stage.ID,
		Plan: domain.PlanRef{ID: plan.ID, Revision: plan.Revision, Digest: plan.Digest},
		Role: domain.RoleImplementer, InvocationMode: domain.InvocationModeMutating,
		TrustRequirement: domain.TrustRequirementOperatorTrusted,
		Profile: domain.ProfileBinding{
			ID: "codex", Version: 1, Digest: strings.Repeat("d", 64),
			Capabilities:     []domain.EngineeringCapability{domain.CapabilityCodeChange},
			TrustRequirement: domain.TrustRequirementOperatorTrusted,
		},
		Agent: domain.AgentBinding{
			ID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
			TrustMode: domain.TrustRequirementOperatorTrusted,
		},
		Contract: domain.ObjectRevision{ID: "contract", Revision: "1"},
		Selection: domain.ResolutionExplanation{
			Considered: []domain.CandidateEvaluation{},
			Selected:   "codex", Reason: "the only eligible worker",
		},
		Context: domain.ContextPack{
			Objective: "o", AcceptanceCriteria: []string{"a"},
			Included: domain.RequiredContextClasses(), Excluded: []domain.ContextClass{},
		},
	}
	if err := fixture.store.PutPlanAssignment(plan.ID, plan.Revision, 0, previous); err != nil {
		t.Fatal(err)
	}

	same := previous
	if block := fixture.reconciler.refusePrivilegeChange(plan, stage, 1, same); block != nil {
		t.Fatalf("an unchanged obligation was refused: %#v", block)
	}

	for name, changed := range map[string]func(domain.AgentAssignment) domain.AgentAssignment{
		"a different worker":  func(a domain.AgentAssignment) domain.AgentAssignment { a.Agent.ID = "claude"; return a },
		"a different role":    func(a domain.AgentAssignment) domain.AgentAssignment { a.Role = domain.RoleReviewer; return a },
		"a different profile": func(a domain.AgentAssignment) domain.AgentAssignment { a.Profile.Version = 2; return a },
		"a lower trust mode": func(a domain.AgentAssignment) domain.AgentAssignment {
			a.Agent.TrustMode = domain.TrustRequirementProtected
			return a
		},
		"a different independence obligation": func(a domain.AgentAssignment) domain.AgentAssignment {
			a.Independence = []domain.IndependenceBinding{{
				Dimension: domain.IndependenceVendorFamily, DifferentFrom: []string{"backend"},
				Class: "openai", SatisfiedBy: "distinct_worker",
			}}
			return a
		},
	} {
		block := fixture.reconciler.refusePrivilegeChange(plan, stage, 1, changed(previous))
		if block == nil {
			t.Fatalf("%s was performed again without an approval", name)
		}
		if block.Kind != "authority" || !strings.Contains(block.Reason, "propose a revision") {
			t.Fatalf("%s produced %#v", name, block)
		}
	}
}

// A producer that is mid-remediation has not produced anything to review, and
// the sweep must not spend a generation on its interim heads.
//
// The run row's candidate is refreshed on every reconcile of that run - every
// commit and every checkpoint - not on publication or settlement. Reviewer
// feedback re-activates the producer, and its first checkpoint moves the row.
// Firing there froze the next generation against a head the producer was still
// changing, which then had to be invalidated again the moment it settled: a
// child run and a share of the approved invocation envelope burnt on every
// cycle, and, if the interim commit never became reachable, a generation
// pinned to a head nothing can build on.
func TestTheSweepIgnoresAProducersInterimHeads(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
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

	// Feedback re-activates the producer, and it checkpoints. This head is
	// WORK IN PROGRESS: the run is active, and nobody has said it is done.
	reactivateRunAtHead(t, fixture, implementation, "interim111111")
	fixture.reconcile(t)
	fixture.reconcile(t)

	during, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if during.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the review was invalidated against an interim head: %#v", during.Stages["review"])
	}
	if during.Stages["review"].Generation != 0 {
		t.Fatalf("a generation was spent on an interim head: %#v", during.Stages["review"])
	}

	// The producer settles on the head it actually finished with. NOW the
	// review is stale, and the new generation is frozen against that head -
	// not against the checkpoint in between.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")
	fixture.reconcile(t)
	fixture.reconcile(t)

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].Generation != 1 {
		t.Fatalf("the review was not performed again once the producer settled: %#v", after.Stages["review"])
	}
	next, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 1, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the re-performance was not frozen against anything")
	}
	if got := next.Context.UpstreamOutputs[0].Candidate; got != "bbbbbbbbbbbb" {
		t.Fatalf("the re-performance consumes %q, which is not the head the producer settled on", got)
	}
}

// The same rule where it matters most: at the moment a performance is FROZEN.
//
// Deciding that work is stale and binding its replacement are two separate
// reads of the producer's head, and they are not always the same pass: a stage
// whose start was blocked - by the plan's concurrency ceiling, its budget, an
// engine that could not be built - starts on a later tick, and feedback can put
// the producer back to work in between. Starting there froze the replacement,
// immutably, against a head being changed while it read it.
func TestANewGenerationIsNotFrozenAgainstAReactivatedProducer(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	// The producer is back at work on an interim head, and the review stage is
	// asked to start now - which is the state a blocked start leaves behind.
	reactivateRunAtHead(t, fixture, implementation, "interim222222")
	snapshot, err = fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	stage := fixture.plan.Stages[1]
	resolution, err := fixture.service.Resolve(fixture.plan, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assignment, ok := resolution.Assignment(stage.ID)
	if !ok {
		t.Fatal("the review stage could not be assigned, so this test asserts nothing")
	}
	if got := assignment.Context.UpstreamOutputs[0].Candidate; got != "interim222222" {
		t.Fatalf("resolution read %q, so the interim head is not what a start would freeze", got)
	}

	started, blocked, err := fixture.reconciler.startAgentStage(context.Background(),
		fixture.plan, stage, snapshot, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if started != nil {
		t.Fatalf("the stage started against a head its producer is still changing: %#v", started)
	}
	if blocked == nil || blocked.Kind != "upstream" {
		t.Fatalf("the start was not refused for the reason it happened: %#v", blocked)
	}
	if !strings.Contains(blocked.Reason, "implementation") {
		t.Fatalf("the block does not name the producer: %s", blocked.Reason)
	}
	// And NOTHING durable was written. The freeze is immutable, so a row
	// written here would bind every later attempt to the interim head.
	if _, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, stage.ID); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("the replacement was frozen against the interim head")
	}

	// Once the producer settles, the same call starts against the head it
	// settled on.
	recordCandidateAndAssurance(t, fixture, implementation, "cccccccccccc")
	settleRunAtGoalState(t, fixture, implementation, "cccccccccccc")
	fixture.reconcile(t)
	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].RunID == "" {
		t.Fatalf("the review never started once its producer settled: %#v", after.Stages["review"])
	}
	frozen, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the review was not frozen against anything")
	}
	if got := frozen.Context.UpstreamOutputs[0].Candidate; got != "cccccccccccc" {
		t.Fatalf("the review consumes %q, which is not the head the producer settled on", got)
	}
}

// reactivateRunAtHead is a producer that reviewer feedback put back to work: it
// is active again and has committed something, and it is not done.
func reactivateRunAtHead(t *testing.T, fixture *planRunFixture, runID, head string) {
	t.Helper()
	recordCandidate(t, fixture, runID, head)
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	run.Disposition = Active
	run.Reason = ""
	run.Candidate.Revision, run.Candidate.Tree = head, head
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
}
