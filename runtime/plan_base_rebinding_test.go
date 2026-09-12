package runtime

// Base rebinding: a plan outlives merges to the default branch.
//
// The #119 dogfood ended here. A plan's identity is derived from its repository
// and its source issue - deliberately, so "two plans for one source" is
// unrepresentable - and its revision subject is an exact base commit, also
// deliberately, because every candidate, test result and verdict under it is a
// statement about that exact tree. The cross-revision law compared the WHOLE
// subject, so the two facts collided: once the repository's trusted base moved,
// the plan could never be revised, and there was no second identity to escape
// to. An unapproved plan had to be approved before the next merge or be dead.
//
// These tests hold the repaired transition to all of its obligations at once:
// the rebind is possible, it is authorized by observation rather than by a
// document, it is material invalidation, it retires the work it replaces
// without losing its spend, and it survives a restart unchanged.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// baseA and baseB are two exact trusted bases of one repository: the
// pre-adoption commit a pending plan was bound to, and the commit a merge to the
// default branch made current.
var (
	baseA = strings.Repeat("a", 40)
	baseB = strings.Repeat("b", 40)
)

// rebindProposal is a COMPILABLE proposal for one plan identity at one base.
//
// It is deliberately the ordinary shape: one implementer, one independent
// reviewer in a different execution agent, and the assurance gate the contract's
// claim requires. Nothing here is about rebinding; what varies between calls is
// the subject and whether an observation accompanies it.
func rebindProposal(planID, repository, base, observed string) ProposeInput {
	subject := domain.Subject{Repository: repository, Revision: base}
	contract := planFixtureContract(&phase8Fixture{base: base})
	contract.Subject = subject
	return ProposeInput{
		PlanID: planID, Objective: "Resolve the M2 hardening cohort.",
		Subject: subject, ObservedBase: observed, Repository: repository,
		Contract: contract, Issue: 119,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject,
		},
		Reasoned: []domain.PlanStage{
			{ID: "harden-runtime", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "Harden the runtime transitions.", InvocationMode: domain.InvocationModeMutating,
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
			{ID: "verify-runtime", Kind: domain.StageAgent, Role: domain.RoleReviewer,
				Objective: "Verify the candidate independently.", DependsOn: []string{"harden-runtime"},
				InvocationMode:       domain.InvocationModeMutating,
				RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification},
				Independence: &domain.IndependenceRequirement{
					Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"harden-runtime"},
				}},
		},
		Reasoning: &domain.PlanReasoningProvenance{
			AgentID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
			TrustMode: domain.TrustRequirementOperatorTrusted, Model: "gpt-5",
			InvocationMode:        domain.InvocationModeNonMutatingPlanning,
			WorkspaceDigestBefore: strings.Repeat("d", 64), WorkspaceDigestAfter: strings.Repeat("d", 64),
			WorkspaceUnchanged: true,
		},
	}
}

// A — the exact #119 failure.
//
// A plan is proposed, nothing is approved, and the repository's trusted base
// moves. Replanning the same source must produce a new revision of the SAME plan
// bound to the new base, awaiting the ordinary approval, with the pending
// revision it replaces still durable.
func TestAPendingPlanSurvivesAMergeToTheDefaultBranch(t *testing.T) {
	f := newAttemptFixture(t)
	first, err := f.service.Propose(context.Background(), rebindProposal("plan-rebind", "acme/repo", baseA, baseA))
	if err != nil {
		t.Fatalf("the first proposal was refused: %v", err)
	}
	if first.Subject.Revision != baseA {
		t.Fatalf("the first revision is not bound to base A: %q", first.Subject.Revision)
	}

	// The merge. Nothing was approved and nothing executed; only the observed
	// base moved.
	second, err := f.service.Propose(context.Background(), rebindProposal("plan-rebind", "acme/repo", baseB, baseB))
	if err != nil {
		t.Fatalf("replanning after a merge was refused, which is the #119 blocker: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("the rebind created a second identity: %q then %q", first.ID, second.ID)
	}
	if second.Revision != first.Revision+1 {
		t.Fatalf("the rebind did not produce the next revision: %d then %d", first.Revision, second.Revision)
	}
	if second.Subject.Repository != first.Subject.Repository {
		t.Fatalf("the rebind moved the repository: %q then %q", first.Subject.Repository, second.Subject.Repository)
	}
	if second.Subject.Revision != baseB {
		t.Fatalf("the new revision is not bound to the observed base: %q", second.Subject.Revision)
	}
	// EXACT, still. The repair permits the base to move and never permits it to
	// become approximate.
	if second.Subject.Revision == "" || len(second.Subject.Revision) != len(baseB) {
		t.Fatalf("the new subject is not an exact revision: %q", second.Subject.Revision)
	}

	// Approval is still required, and the superseded revision is still readable.
	snapshot, err := f.store.ReplayPlan(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Approval.Status == domain.ApprovalApproved {
		t.Fatal("the rebind approved itself")
	}
	if _, found, err := f.store.PlanRevision(second.ID, first.Revision); err != nil || !found {
		t.Fatalf("the revision the rebind replaced is no longer durable (found=%v): %v", found, err)
	}

	// And the operator can SEE the transition without reading the database.
	view, err := f.service.ViewRevision(second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if view.BaseChange == nil {
		t.Fatal("the view does not report the base transition")
	}
	if view.BaseChange.From != baseA || view.BaseChange.To != baseB {
		t.Fatalf("the reported transition is wrong: %#v", view.BaseChange)
	}
	if view.BaseChange.FromRevision != first.Revision {
		t.Fatalf("the transition is not attributed to the revision it replaced: %#v", view.BaseChange)
	}
}

// A plan whose base did NOT move reports no transition. Without this the surface
// would announce a rebind on every ordinary revision.
func TestARevisionAtTheSameBaseReportsNoTransition(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), rebindProposal("plan-same-base", "acme/repo", baseA, baseA)); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Propose(context.Background(), rebindProposal("plan-same-base", "acme/repo", baseA, baseA))
	if err != nil {
		t.Fatalf("an ordinary same-base revision was refused: %v", err)
	}
	view, err := f.service.ViewRevision(second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if view.BaseChange != nil {
		t.Fatalf("a revision at the same base reported a base change: %#v", view.BaseChange)
	}
}

// B — retargeting the REPOSITORY stays refused, and says so in both names.
func TestARevisionCannotRetargetTheRepository(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), rebindProposal("plan-retarget", "acme/repo", baseA, baseA)); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Propose(context.Background(), rebindProposal("plan-retarget", "acme/other", baseA, baseA))
	if err == nil {
		t.Fatal("a revision moved the plan onto a different repository")
	}
	var invalid *planning.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the retarget refusal is not a typed validation error: %v", err)
	}
	joined := strings.Join(invalid.Reasons, "; ")
	if !strings.Contains(joined, "acme/repo") || !strings.Contains(joined, "acme/other") {
		t.Fatalf("the refusal does not name both repositories: %v", joined)
	}
}

// B (authority half) — a base nobody observed is refused.
//
// planning.Validate permits the base to move because it cannot observe a remote.
// If that were the whole change, any caller reaching Propose - including one
// relaying a commit a reasoning agent wrote into its output - could move a plan
// onto any commit it could name.
func TestABaseNobodyObservedIsRefused(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), rebindProposal("plan-unobserved", "acme/repo", baseA, baseA)); err != nil {
		t.Fatal(err)
	}

	// No observation at all: the field a caller that did not look leaves empty.
	_, err := f.service.Propose(context.Background(), rebindProposal("plan-unobserved", "acme/repo", baseB, ""))
	if err == nil {
		t.Fatal("a revision bound a base with no observation behind it")
	}
	if !strings.Contains(err.Error(), "no observed trusted base") {
		t.Fatalf("the refusal does not say the base was unobserved: %v", err)
	}

	// An observation that does not match the subject: the document names one
	// commit and the governed remote returned another.
	_, err = f.service.Propose(context.Background(), rebindProposal("plan-unobserved", "acme/repo", baseB, strings.Repeat("c", 40)))
	if err == nil {
		t.Fatal("a revision bound a base the observation contradicts")
	}
	if !strings.Contains(err.Error(), "observed") {
		t.Fatalf("the refusal does not name the observation: %v", err)
	}

	// And both refusals are preserved with BOTH subjects, which is what the
	// dogfood could not answer.
	snapshot, err := f.store.ReplayPlan("plan-unobserved")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 2 {
		t.Fatalf("the refused rebinds were not both preserved: %d attempts", len(snapshot.Attempts))
	}
	for _, attempt := range snapshot.Attempts {
		if attempt.Subject == nil || attempt.PreviousSubject == nil {
			t.Fatalf("a subject refusal recorded neither side of it: %#v", attempt)
		}
		if attempt.Subject.Revision == attempt.PreviousSubject.Revision {
			t.Fatalf("the attempt's two subjects are the same: %#v", attempt)
		}
		if attempt.PreviousSubject.Revision != baseA {
			t.Fatalf("the attempt does not name the base it was compared against: %#v", attempt.PreviousSubject)
		}
	}
}

// rebindRevision stores and approves a revision of the fixture's plan bound to a
// new base, exactly as the propose-then-approve path would leave it.
func rebindRevision(t *testing.T, f *planRunFixture, base string) domain.EngineeringPlan {
	t.Helper()
	next := f.plan
	next.Revision = f.plan.Revision + 1
	next.Subject = domain.Subject{Repository: f.plan.Subject.Repository, Revision: base}
	previous := f.plan.Revision
	next.Provenance.PreviousRevision = &previous
	digest, err := next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	next.Digest = digest
	if _, err := f.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutPlanContract(next.ID, next.Revision, planFixtureContract(f.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	return next
}

// C — work proven against base A is not work proven against base B.
//
// This is the part existing machinery could not do. InvalidatedStages compares
// stage CONTENT, and a rebind changes no stage - so every completed performance,
// candidate, verdict and satisfied gate survived a base change untouched,
// because the stage that produced it was byte-identical in both revisions.
func TestABaseRebindInvalidatesWorkProvenAgainstTheOldBase(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification},
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"implementation"},
			}},
	})
	fixture.approve(t)

	// A completed performance under base A, with a run behind it.
	seedPerformedStage(t, fixture, "implementation", "run-implementation-a")
	seedPerformedStage(t, fixture, "review", "run-review-a")
	before, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Stages["implementation"].State != PlanStageCompleted || before.Stages["review"].State != PlanStageCompleted {
		t.Fatalf("the fixture did not record work under base A: %#v", before.Stages)
	}

	next := rebindRevision(t, fixture, baseB)

	// What the operator is told BEFORE approving: the rebound revision redoes
	// both performances.
	view, err := fixture.service.ViewRevision(next.ID, next.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if view.Preview == nil || len(view.Preview.Invalidated) != 2 {
		t.Fatalf("the approval preview does not say the old-base work is redone: %#v", view.Preview)
	}
	if view.BaseChange == nil || view.BaseChange.To != baseB {
		t.Fatalf("the approval preview does not report the base transition: %#v", view.BaseChange)
	}

	if _, err := fixture.service.Approve(next.ID, next.Revision, next.Digest,
		shownAssignments(t, fixture.service, next.ID, next.Revision), "operator", "rebound onto the merged base"); err != nil {
		t.Fatalf("approve the rebound revision: %v", err)
	}

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"implementation", "review"} {
		if after.Stages[stage].State != PlanStageInvalidated {
			t.Fatalf("stage %q kept its base-A work under base B: %#v", stage, after.Stages[stage])
		}
		// The run identity is cleared from the stage, so the new base's
		// performance cannot adopt the old base's run.
		if after.Stages[stage].RunID != "" {
			t.Fatalf("stage %q still names its base-A run: %#v", stage, after.Stages[stage])
		}
	}
	if len(after.Superseded) == 0 {
		t.Fatal("the rebind recorded no supersession")
	}
	last := after.Superseded[len(after.Superseded)-1]
	if len(last.InvalidatedStages) != 2 {
		t.Fatalf("the supersession does not name the invalidated work: %#v", last)
	}
}

// A same-base revision that changes ONE stage still invalidates only what it
// changed. The rebind rule is additional, not a replacement: without this the
// conservative sweep could quietly become the only behaviour and discard valid
// work on every ordinary revision.
func TestASameBaseRevisionStillInvalidatesOnlyWhatItChanged(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "docs", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Write it down.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)
	seedPerformedStage(t, fixture, "implementation", "run-implementation-same")
	seedPerformedStage(t, fixture, "docs", "run-docs-same")

	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Same base, one stage's objective edited.
	next := fixture.plan
	next.Revision = fixture.plan.Revision + 1
	next.Stages = []domain.PlanStage{fixture.plan.Stages[0], fixture.plan.Stages[1]}
	next.Stages[1].Objective = "Write it down, with examples."
	invalidated := InvalidatedStages(fixture.plan, next, snapshot)
	if len(invalidated) != 1 || invalidated[0] != "docs" {
		t.Fatalf("a same-base edit invalidated the wrong set: %v", invalidated)
	}
}

// D — a live run from the old base is retired, and its spend is not forgotten.
func TestABaseRebindRetiresTheOldBaseRunWithoutLosingItsSpend(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)

	// A RUNNING stage: started under base A, not settled.
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanRunStarted, PlanRunStartedPayload{StageID: "implementation", RunID: "run-live-a"}); err != nil {
		t.Fatal(err)
	}
	// And spend attributed to the plan while it ran.
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			StageID: "implementation", RunID: "run-live-a", ChildRuns: 1, ProviderInvocations: 3,
		}); err != nil {
		t.Fatal(err)
	}
	running, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Stages["implementation"].State != PlanStageRunning {
		t.Fatalf("the fixture has no live base-A run: %#v", running.Stages["implementation"])
	}
	spentBefore := running.Consumed

	next := rebindRevision(t, fixture, baseB)
	if _, err := fixture.service.Approve(next.ID, next.Revision, next.Digest,
		shownAssignments(t, fixture.service, next.ID, next.Revision), "operator", "rebound onto the merged base"); err != nil {
		t.Fatalf("approve the rebound revision: %v", err)
	}

	after, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The old run is RETIRED: the plan still knows it exists, and the stage no
	// longer names it, so the rebound revision cannot continue it as its own.
	retired := false
	for _, runID := range after.RetiredRuns {
		if runID == "run-live-a" {
			retired = true
		}
	}
	if !retired {
		t.Fatalf("the live base-A run was forgotten rather than retired: %#v", after.RetiredRuns)
	}
	if after.Stages["implementation"].RunID == "run-live-a" {
		t.Fatal("the rebound revision adopted the base-A run as its own")
	}
	// E — and its spend is still the plan's.
	if after.Consumed.ChildRuns < spentBefore.ChildRuns ||
		after.Consumed.ProviderInvocations < spentBefore.ProviderInvocations {
		t.Fatalf("the rebind reset consumption: before %#v after %#v", spentBefore, after.Consumed)
	}
	if after.Consumed.ProviderInvocations != 3 || after.Consumed.ChildRuns != 1 {
		t.Fatalf("the rebind changed what had been spent: %#v", after.Consumed)
	}
}

// E — a rebind is not a new budget ledger, and a revision that tries to claim
// back consumption is still refused.
func TestABaseRebindCannotReclaimConsumedBudget(t *testing.T) {
	f := newAttemptFixture(t)
	first, err := f.service.Propose(context.Background(), rebindProposal("plan-budget", "acme/repo", baseA, baseA))
	if err != nil {
		t.Fatal(err)
	}
	if err := appendPlanEvent(f.store, f.clock.Now(), first.ID,
		EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			StageID: "harden-runtime", RunID: "run-budget-a", ChildRuns: 2, ProviderInvocations: 7,
		}); err != nil {
		t.Fatal(err)
	}

	second, err := f.service.Propose(context.Background(), rebindProposal("plan-budget", "acme/repo", baseB, baseB))
	if err != nil {
		t.Fatalf("the rebind was refused: %v", err)
	}
	snapshot, err := f.store.ReplayPlan(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns != 2 || snapshot.Consumed.ProviderInvocations != 7 {
		t.Fatalf("the rebind moved consumption: %#v", snapshot.Consumed)
	}

	// A rebound revision whose envelope is BELOW what the plan already spent is
	// refused for the ordinary reason, with the new base changing nothing about
	// it.
	narrow := rebindProposal("plan-budget", "acme/repo", baseB, baseB)
	narrow.Contract.PlanRequirements = nil
	f.service.Envelope = domain.PlanBudgetEnvelope{MaxChildRuns: 1, MaxConcurrency: 1, MaxProviderInvocations: 1}
	if _, err := f.service.Propose(context.Background(), narrow); err == nil {
		t.Fatal("a rebound revision claimed back consumed budget")
	}
}

// F — the transition replays identically from the journal.
func TestABaseRebindReplaysIdenticallyAfterRestart(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)
	seedPerformedStage(t, fixture, "implementation", "run-replay-a")
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			StageID: "implementation", RunID: "run-replay-a", ChildRuns: 1, ProviderInvocations: 4,
		}); err != nil {
		t.Fatal(err)
	}
	next := rebindRevision(t, fixture, baseB)
	if _, err := fixture.service.Approve(next.ID, next.Revision, next.Digest,
		shownAssignments(t, fixture.service, next.ID, next.Revision), "operator", "rebound"); err != nil {
		t.Fatal(err)
	}
	live, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := fixture.stateDir

	// CLOSE and REOPEN. Nothing process-local may decide any of this.
	fixture.store.Close()
	reopened, err := OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, err := reopened.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}

	governing, liveOK := live.ApprovedRevision()
	replayedGoverning, replayedOK := replayed.ApprovedRevision()
	if liveOK != replayedOK || governing != replayedGoverning {
		t.Fatalf("the governing revision changed across a restart: %d/%v then %d/%v",
			governing, liveOK, replayedGoverning, replayedOK)
	}
	stored, found, err := reopened.PlanRevision(fixture.plan.ID, replayedGoverning)
	if err != nil || !found {
		t.Fatalf("the governing revision is not stored after a restart (found=%v): %v", found, err)
	}
	if stored.Subject.Revision != baseB {
		t.Fatalf("the replayed subject is not the rebound base: %q", stored.Subject.Revision)
	}
	if replayed.Consumed != live.Consumed {
		t.Fatalf("consumption changed across a restart: %#v then %#v", live.Consumed, replayed.Consumed)
	}
	if len(replayed.RetiredRuns) != len(live.RetiredRuns) {
		t.Fatalf("retired runs changed across a restart: %#v then %#v", live.RetiredRuns, replayed.RetiredRuns)
	}
	if replayed.Stages["implementation"].State != live.Stages["implementation"].State {
		t.Fatalf("the invalidation changed across a restart: %#v then %#v",
			live.Stages["implementation"], replayed.Stages["implementation"])
	}
	if replayed.Stages["implementation"].State != PlanStageInvalidated {
		t.Fatalf("the replayed stage is not invalidated: %#v", replayed.Stages["implementation"])
	}
	if len(replayed.Superseded) != len(live.Superseded) {
		t.Fatalf("the supersession record changed across a restart: %#v then %#v", live.Superseded, replayed.Superseded)
	}
}

// seedPerformedStage records a stage that RAN and completed under the current
// revision, with a child run behind it.
func seedPerformedStage(t *testing.T, fixture *planRunFixture, stageID, runID string) {
	t.Helper()
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanRunStarted, PlanRunStartedPayload{StageID: stageID, RunID: runID}); err != nil {
		t.Fatal(err)
	}
	if err := appendPlanEvent(fixture.store, fixture.clock.Now(), fixture.plan.ID,
		EventPlanStageSettled, PlanStageSettledPayload{StageID: stageID, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
}

// ONE EXACT SUBJECT: a plan and the obligations it is judged against.
//
// Base rebinding is what made this reachable. The compiler proved the contract's
// REPOSITORY matched the plan's, and the ordinary intent path recompiles the
// contract against the new base - so the invariant held because one caller
// behaved, not because anything enforced it. A revision bound to base B
// compiling against obligations compiled from base A would execute against one
// tree under the scope, facts, claims and acceptance criteria of another.
func TestARebindWithAStaleContractIsRefused(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), rebindProposal("plan-stale-contract", "acme/repo", baseA, baseA)); err != nil {
		t.Fatal(err)
	}

	// The plan moves to base B, the observation agrees, and the contract is left
	// behind at base A.
	stale := rebindProposal("plan-stale-contract", "acme/repo", baseB, baseB)
	stale.Contract = planFixtureContract(&phase8Fixture{base: baseA})
	stale.Contract.Subject = domain.Subject{Repository: "acme/repo", Revision: baseA}

	_, err := f.service.Propose(context.Background(), stale)
	if err == nil {
		t.Fatal("a revision compiled against obligations from a different exact base")
	}
	// The diagnostic names BOTH bindings, in full.
	for _, want := range []string{"acme/repo@" + baseB, "acme/repo@" + baseA, "different exact base"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not state %q: %v", want, err)
		}
	}

	// NO executable revision was persisted: the plan is still the one revision
	// that compiled.
	stored, found, err := f.store.Plan("plan-stale-contract")
	if err != nil || !found {
		t.Fatalf("the plan is gone (found=%v): %v", found, err)
	}
	if stored.Revision != 1 {
		t.Fatalf("a refused revision was persisted as executable: revision %d", stored.Revision)
	}
	if _, found, err := f.store.PlanRevision("plan-stale-contract", 2); err != nil || found {
		t.Fatalf("revision 2 exists after a refusal (found=%v): %v", found, err)
	}
	// And the refusal is durable evidence, with both subjects on it.
	snapshot, err := f.store.ReplayPlan("plan-stale-contract")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != 1 {
		t.Fatalf("the refusal was not preserved as an attempt: %d attempts", len(snapshot.Attempts))
	}
	attempt := snapshot.Attempts[0]
	if attempt.Subject == nil || attempt.Subject.Revision != baseB {
		t.Fatalf("the attempt does not record the subject it attempted: %#v", attempt.Subject)
	}
	if attempt.PreviousSubject == nil || attempt.PreviousSubject.Revision != baseA {
		t.Fatalf("the attempt does not record what it was compared against: %#v", attempt.PreviousSubject)
	}
}

// The positive path: the contract moves with the plan, and the contract STORED
// for that revision is the one bound to the new base.
func TestARebindWithAFreshContractStoresTheNewExactSubject(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), rebindProposal("plan-fresh-contract", "acme/repo", baseA, baseA)); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Propose(context.Background(), rebindProposal("plan-fresh-contract", "acme/repo", baseB, baseB))
	if err != nil {
		t.Fatalf("a rebind whose contract moved with it was refused: %v", err)
	}
	if second.Subject.Revision != baseB {
		t.Fatalf("the revision is not bound to the new base: %q", second.Subject.Revision)
	}
	contract, found, err := f.store.PlanContract(second.ID, second.Revision)
	if err != nil || !found {
		t.Fatalf("no contract is stored for the rebound revision (found=%v): %v", found, err)
	}
	if contract.Subject != second.Subject {
		t.Fatalf("the stored contract is bound to %#v and the revision to %#v", contract.Subject, second.Subject)
	}
	// The revision it replaced keeps ITS contract, at its own base: a rebind adds
	// a binding and rewrites none.
	previous, found, err := f.store.PlanContract(second.ID, second.Revision-1)
	if err != nil || !found {
		t.Fatalf("the replaced revision lost its contract (found=%v): %v", found, err)
	}
	if previous.Subject.Revision != baseA {
		t.Fatalf("the replaced revision's contract moved: %#v", previous.Subject)
	}
}

// And a contract whose REPOSITORY differs is still refused by the earlier, more
// specific diagnostic. The exact-subject law must not swallow it.
func TestAContractForADifferentRepositoryKeepsItsOwnDiagnostic(t *testing.T) {
	f := newAttemptFixture(t)
	wrong := rebindProposal("plan-wrong-repo-contract", "acme/repo", baseA, baseA)
	wrong.Contract.Subject = domain.Subject{Repository: "acme/other", Revision: baseA}
	_, err := f.service.Propose(context.Background(), wrong)
	if err == nil {
		t.Fatal("a plan compiled against a contract governing another repository")
	}
	if !strings.Contains(err.Error(), "the work contract governs") {
		t.Fatalf("the repository diagnostic was replaced: %v", err)
	}
}
