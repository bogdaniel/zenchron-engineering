package runtime

// A frozen assignment that no run was ever created under must not become a
// hidden stale freeze.
//
// startAgentStage persists the immutable assignment BEFORE it builds the engine
// and creates the run, and `plan.stage_assigned` is appended only after the run
// exists. A start that failed in between therefore left a frozen row that
// nothing points at - and PutPlanAssignment keeps the first row written for a
// (revision, stage, generation), so a retry read that row back and executed it.
//
// The upstream guard used to run against the assignment the resolver had just
// produced, which on a retry is a DIFFERENT document from the one that would
// execute. So the safety check passed on the replacement and the stale freeze
// ran against upstream work that had since been replaced.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func orphanStages() []domain.PlanStage {
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

// The whole sequence, through the real failure and retry path, across a
// process restart.
func TestAnAbandonedFrozenAssignmentDoesNotExecuteStaleUpstreamWork(t *testing.T) {
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

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}
	// The producer settles candidate A.
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	// The dependent's assignment is frozen against A, and the start then FAILS
	// before the stage was ever associated with a run.
	startFails = true
	report := fixture.reconcile(t)
	if !blockedFor(report, "review") {
		t.Fatalf("the failing start did not block the review stage: %#v", report.Blocked)
	}
	abandoned, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("this test needs the failure to happen AFTER the assignment was frozen, and nothing was frozen")
	}
	if len(abandoned.Context.UpstreamOutputs) == 0 || abandoned.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the abandoned assignment does not name candidate A: %#v", abandoned.Context.UpstreamOutputs)
	}
	orphaned, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.Stages["review"].AssignmentID != "" || orphaned.Stages["review"].RunID != "" {
		t.Fatalf("the failed start still associated the stage with a run: %#v", orphaned.Stages["review"])
	}

	// The producer reactivates and settles a DIFFERENT candidate. Nothing
	// invalidates the dependent: it never completed, so the stale-work sweep
	// does not look at it.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")

	// RESTART. Everything from here reads the durable state a new process
	// would find: a frozen assignment naming a candidate that has been
	// replaced, and no record of the start that froze it.
	restarted := restartReconciler(t, fixture)
	for i := 0; i < 3; i++ {
		if _, err := restarted.Reconcile(context.Background(), fixture.plan.ID); err != nil {
			t.Fatalf("reconcile after restart: %v", err)
		}
	}

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	// THE ASSERTION THAT MATTERS: whatever ran, it was not the assignment
	// frozen against the replaced candidate.
	if after.Stages["review"].AssignmentID == abandoned.ID {
		t.Fatalf("the stage executed the assignment frozen against candidate A: %s", abandoned.ID)
	}
	if after.Stages["review"].RunID == "" {
		t.Fatal("the stage never recovered: nothing ran at all after the producer settled its replacement")
	}
	executing, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision,
		after.Stages["review"].Generation, "review")
	if err != nil || !found {
		t.Fatalf("the executing assignment is not readable: found=%v err=%v", found, err)
	}
	if executing.ID != after.Stages["review"].AssignmentID {
		t.Fatalf("the journal names assignment %s and the stored one is %s", after.Stages["review"].AssignmentID, executing.ID)
	}
	if len(executing.Context.UpstreamOutputs) == 0 || executing.Context.UpstreamOutputs[0].Candidate != "bbbbbbbbbbbb" {
		t.Fatalf("the recovered performance consumes %#v, want the candidate the producer settled on",
			executing.Context.UpstreamOutputs)
	}
	// And the abandoned row is still exactly where it was. Immutable
	// assignment history is not rewritten to make a retry work.
	again, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("the abandoned assignment was deleted: found=%v err=%v", found, err)
	}
	if again.ID != abandoned.ID || again.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the abandoned assignment was rewritten: %#v", again)
	}
	// The discard is journalled: an operator can see that a frozen assignment
	// was abandoned and why, rather than finding a generation that appeared
	// from nowhere.
	if !strings.Contains(after.Stages["review"].Reason, "no run was created under it") &&
		!invalidationSaid(t, fixture, "review", "no run was created under it") {
		t.Fatalf("the abandoned freeze was discarded with no durable reason: %#v", after.Stages["review"])
	}
}

// A retry after a failed start that the producer did NOT outrun re-uses the
// assignment it froze. The recovery above must not become "throw the freeze
// away whenever a start failed": the frozen document is what an operator
// approved this performance to run under.
func TestARetryAfterAFailedStartKeepsTheAssignmentItFroze(t *testing.T) {
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
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	startFails = true
	fixture.reconcile(t)
	frozen, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil || !found {
		t.Fatalf("nothing was frozen: found=%v err=%v", found, err)
	}

	startFails = false
	fixture.reconcile(t)
	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stages["review"].Generation != 0 {
		t.Fatalf("an ordinary retry burned an execution generation: %d", after.Stages["review"].Generation)
	}
	if after.Stages["review"].AssignmentID != frozen.ID {
		t.Fatalf("the retry ran assignment %q and the frozen one is %q", after.Stages["review"].AssignmentID, frozen.ID)
	}
	if after.Stages["review"].RunID == "" {
		t.Fatal("the retry started nothing")
	}
}

// A freeze whose RUN was created, but whose association event was lost, is not
// abandoned.
//
// `plan.stage_assigned` is appended after the run exists, so a crash between
// the two leaves a live run the projection does not name - which looks exactly
// like a failed start. Discarding that freeze would advance the generation and
// leave the run executing: unstopped, and attributed to no stage, which is
// consumed plan budget made invisible. The stage is associated with the run
// that exists instead, and the ordinary sweep handles it from there.
func TestAFreezeWhoseRunSurvivedIsAssociatedRatherThanDiscarded(t *testing.T) {
	fixture := newPlanRunFixture(t, orphanStages())
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	// The review stage's own start, up to and including the run - and then
	// nothing. This is the durable state a crash before the association append
	// leaves behind.
	resolution, err := fixture.service.Resolve(fixture.plan, mustReplayPlan(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	review, ok := resolution.Assignment("review")
	if !ok {
		t.Fatal("the review stage resolved to nothing")
	}
	if err := fixture.store.PutPlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, review); err != nil {
		t.Fatal(err)
	}
	outcome, err := fixture.runtime.StartPlanStageRun(context.Background(), fixture.issue, RunPlanBinding{
		PlanID: fixture.plan.ID, Revision: fixture.plan.Revision, PlanDigest: fixture.plan.Digest,
		StageID: "review", AssignmentID: review.ID, Generation: 0,
		BaseRevision: "aaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	lost := mustReplayPlan(t, fixture)
	if lost.Stages["review"].AssignmentID != "" || lost.Stages["review"].RunID != "" {
		t.Fatalf("this test needs the association to be MISSING, and it is present: %#v", lost.Stages["review"])
	}

	// And the producer settles a replacement in that window.
	recordCandidate(t, fixture, implementation, "bbbbbbbbbbbb")
	settleRunAtGoalState(t, fixture, implementation, "bbbbbbbbbbbb")

	fixture.reconcile(t)
	after := mustReplayPlan(t, fixture)
	if after.Stages["review"].Generation != 0 {
		t.Fatalf("the surviving run was abandoned for a new generation: generation %d", after.Stages["review"].Generation)
	}
	if after.Stages["review"].RunID != outcome.RunID {
		t.Fatalf("the stage names run %q and the run that exists is %q", after.Stages["review"].RunID, outcome.RunID)
	}
	if after.Stages["review"].AssignmentID != review.ID {
		t.Fatalf("the stage was associated with assignment %q and the run was created under %q",
			after.Stages["review"].AssignmentID, review.ID)
	}
}

// One predicate answers "did this stage's input move", for the sweep and for
// the start path. It used to exist only in the sweep, which is why a frozen
// assignment nothing had run was never measured against it.
func TestTheMovedInputPredicateIsSharedBySweepAndStart(t *testing.T) {
	fixture := newPlanRunFixture(t, orphanStages())
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	producer := snapshot.Stages["implementation"].RunID
	consumer := domain.AgentAssignment{
		Context: domain.ContextPack{UpstreamOutputs: []domain.UpstreamOutput{
			{StageID: "implementation", RunID: producer, Candidate: "aaaaaaaaaaaa"},
		}},
	}

	// A producer still at work is NOT movement, whatever its interim head says.
	// The run row's candidate is refreshed on every commit and checkpoint, long
	// before the producer has settled on anything a consumer should read.
	moveRunHeadWithoutSettling(t, fixture, producer, "bbbbbbbbbbbb")
	if moved, err := fixture.reconciler.movedUpstream(consumer); err != nil {
		t.Fatal(err)
	} else if moved != "" {
		t.Fatalf("an unsettled producer read as movement: %s", moved)
	}

	// Settled on the same candidate is not movement either.
	settleRunAtGoalState(t, fixture, producer, "aaaaaaaaaaaa")
	if moved, err := fixture.reconciler.movedUpstream(consumer); err != nil {
		t.Fatal(err)
	} else if moved != "" {
		t.Fatalf("a producer settled on the frozen candidate read as movement: %s", moved)
	}

	// Settled on a different one is.
	settleRunAtGoalState(t, fixture, producer, "bbbbbbbbbbbb")
	moved, err := fixture.reconciler.movedUpstream(consumer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(moved, "aaaaaaaaaaaa") || !strings.Contains(moved, "bbbbbbbbbbbb") {
		t.Fatalf("the movement is not named in terms an operator can act on: %q", moved)
	}
}

func blockedFor(report PlanTickReport, stageID string) bool {
	for _, block := range report.Blocked {
		if block.StageID == stageID {
			return true
		}
	}
	return false
}

// restartReconciler builds a reconciler over a SECOND store opened on the same
// durable state, so what it does is derived from what survived rather than from
// anything the first one held in memory.
func restartReconciler(t *testing.T, fixture *planRunFixture) PlanReconciler {
	t.Helper()
	store, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service := fixture.service
	service.Store = store
	return PlanReconciler{
		Store: store, Clock: fixture.clock, Service: service,
		Repository: "acme/repo", Issue: fixture.issue,
		Engine: func(repository, agentID string) (*EngineeringRuntime, error) {
			return fixture.runtime, nil
		},
	}
}

// invalidationSaid reports whether any invalidation recorded for a stage says
// what it was asked about.
func invalidationSaid(t *testing.T, fixture *planRunFixture, stageID, detail string) bool {
	t.Helper()
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventPlanStageSettled {
			continue
		}
		payload, err := decodePayload[PlanStageSettledPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if payload.StageID == stageID && strings.Contains(payload.Reason, detail) {
			return true
		}
	}
	return false
}

func mustReplayPlan(t *testing.T, fixture *planRunFixture) PlanSnapshot {
	t.Helper()
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// moveRunHeadWithoutSettling advances a run's own candidate record while the
// run is still working, which is what every commit and checkpoint does.
func moveRunHeadWithoutSettling(t *testing.T, fixture *planRunFixture, runID, head string) {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("read run %s: found=%v err=%v", runID, found, err)
	}
	if _, settled := stageOutcome(run); settled {
		t.Fatalf("run %s is already settled, so this helper proves nothing", runID)
	}
	run.Candidate.Revision, run.Candidate.Tree = head, head
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
}
