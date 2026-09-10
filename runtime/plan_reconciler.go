package runtime

// The plan reconciler.
//
// It answers exactly two questions, every tick, from durable state:
//
//	which typed gates have become satisfied by evidence, authority or human
//	decisions that already exist?
//	which agent stages are dependency-ready and should become ordinary
//	EngineeringRuns?
//
// It is NOT a scheduler. It creates or associates a run and stops; the existing
// #63 scheduler, its leases, its concurrency ceiling, its retries and its
// provider waits decide when that run actually executes. Nothing here holds a
// lease, counts an attempt, or drives a run.
//
// It is also not an authority. A gate is marked satisfied only from references
// the kernel already owns - an assurance observation bound to an exact tree, an
// authority decision, a recorded human approval - and the plan stores the
// reference rather than a copy, so a gate can never become a second evidence
// model.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// PlanReconciler drives approved plans over the existing runtime.
type PlanReconciler struct {
	Store   *SQLiteOperationStore
	Clock   Clock
	Service PlanService
	// Engine builds the engine for one repository worked by one agent. It is a
	// factory for the same reason the supervisor's is: a plan's stages are
	// worked by DIFFERENT agents, and an engine bound to one of them would
	// silently drive every stage with it.
	Engine func(repository, agentID string) (*EngineeringRuntime, error)
	// Repository is the identity a plan's runs are created in. A plan is bound
	// to one repository; cross-repository programs are #71.
	Repository string
	// Planner performs a decomposition stage's non-mutating invocation. It is a
	// seam supplied by the composition root, so this file constructs no
	// provider and knows no provider's name. Nil means decomposition stages
	// block with that reason rather than being executed some other way.
	Planner func(context.Context, PlanDecompositionRequest) (PlannerOutput, error)
	// Issue is the source the plan answers. Every stage run answers the same
	// source with a different stage objective, which is what keeps a plan's
	// runs ordinary runs rather than a second kind of work.
	Issue int
}

// PlanTickReport is one pass over one plan.
type PlanTickReport struct {
	PlanID   string `json:"plan_id"`
	Revision int    `json:"revision,omitempty"`
	// Waiting is why the plan advanced nothing, when it advanced nothing.
	Waiting string `json:"waiting,omitempty"`
	// Started names the stages that became EngineeringRuns in this pass.
	Started []PlanStageRun `json:"started,omitempty"`
	// Satisfied names the gates that became satisfied in this pass.
	Satisfied []string `json:"satisfied,omitempty"`
	// Settled names the stages whose child run reached a terminal state.
	Settled []string `json:"settled,omitempty"`
	// Blocked is every stage that cannot proceed, with the typed reason.
	Blocked []PlanStageBlock `json:"blocked,omitempty"`
	// Stopped names child runs this pass cancelled because a revision replaced
	// the stage they were doing.
	Stopped []string `json:"stopped,omitempty"`
	// Consumed is the aggregate the plan has spent, after this pass.
	Consumed domain.PlanConsumption `json:"consumed"`
}

// PlanStageRun is one stage-to-run association.
type PlanStageRun struct {
	StageID string `json:"stage_id"`
	RunID   string `json:"run_id"`
	AgentID string `json:"agent_id"`
}

// PlanStageBlock is one stage that cannot proceed and why.
type PlanStageBlock struct {
	StageID string `json:"stage_id"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
}

func (r PlanReconciler) now() time.Time {
	if r.Clock == nil {
		return RealClock{}.Now()
	}
	return r.Clock.Now()
}

// Reconcile advances one plan by one pass.
func (r PlanReconciler) Reconcile(ctx context.Context, planID string) (PlanTickReport, error) {
	report := PlanTickReport{PlanID: planID}
	snapshot, err := r.Store.ReplayPlan(planID)
	if err != nil {
		return report, err
	}
	approved, ok := snapshot.ApprovedRevision()
	if !ok {
		// The single most important thing this file does not do. A proposed
		// plan executes nothing: approval is an operator act, and there is no
		// automatic approval in this milestone.
		report.Waiting = "awaiting_operator_approval"
		report.Consumed = snapshot.Consumed
		return report, nil
	}
	plan, found, err := r.Store.PlanRevision(planID, approved)
	if err != nil {
		return report, err
	}
	if !found {
		return report, &PlanRefusedError{PlanID: planID, Detail: fmt.Sprintf("approved revision %d is not stored", approved)}
	}
	report.Revision = plan.Revision

	// A supersession is recorded in a separate append from the approval that
	// caused it, so a crash between the two lost the invalidations forever -
	// and dependents would then build on work the approved revision had
	// invalidated. It is re-derived here, once, from durable state.
	if snapshot, err = r.recordMissingSupersession(planID, plan, snapshot); err != nil {
		return report, err
	}

	resolution, err := r.Service.Resolve(plan, snapshot)
	if err != nil {
		return report, err
	}
	// A MATERIAL proposal against the approved revision pauses new work. The
	// plan an operator approved is no longer the plan the planner believes in,
	// and starting further stages under the old one would be executing a
	// decomposition nobody accepted. Work already running is not disturbed:
	// #63's own laws govern a live run, and this creates nothing new.
	pending, awaiting, err := r.Store.PendingMaterialProposal(planID, plan.Revision)
	if err != nil {
		return report, err
	}
	for _, blocked := range resolution.Blocked {
		report.Blocked = append(report.Blocked, PlanStageBlock{
			StageID: blocked.StageID, Kind: string(blocked.Kind), Reason: blocked.Reason,
		})
	}

	// Settle first: a stage whose run finished has to be settled before its
	// dependents are considered, or a dependent would wait a whole tick for
	// state that already exists.
	settled, err := r.settleFinishedStages(plan, snapshot)
	if err != nil {
		return report, err
	}
	report.Settled = settled
	if len(settled) > 0 {
		if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
			return report, err
		}
	}
	// A completed stage performed against upstream work that has since MOVED is
	// invalidated before anything reads it, together with everything
	// downstream. Reopening the gate above such a stage is not enough: the
	// review under it would be reused unperformed.
	staleWork, err := r.invalidateStaleCompletedStages(plan, snapshot)
	if err != nil {
		return report, err
	}
	if staleWork {
		if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
			return report, err
		}
		// And RE-RESOLVED. The resolution above seeded every stage that had an
		// assignment with the frozen one, which is the point - a stage already
		// executing keeps the identity an operator approved. A stage this sweep
		// just invalidated is no longer executing that performance, and reusing
		// its frozen assignment would hand the next one the upstream candidate
		// whose replacement caused the invalidation.
		if resolution, err = r.Service.Resolve(plan, snapshot); err != nil {
			return report, err
		}
	}
	// A gate proved against work that has since MOVED is re-opened before
	// anything reads it. A goal-state run is not finished: reviewer feedback
	// re-activates it and it can produce a different candidate, and a verdict
	// about the old one is not a verdict about the new one.
	reopened, err := r.reopenStaleGates(plan, snapshot)
	if err != nil {
		return report, err
	}
	if reopened {
		if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
			return report, err
		}
	}
	// What every child run has spent SINCE it was last attributed, settled or
	// not. A settled stage's run is not finished with the plan's budget: a
	// goal-state run stays live, admitted feedback re-activates it, and the
	// invocations it spends then are the plan's too.
	attributed := false
	for stageID, projection := range snapshot.Stages {
		if projection.RunID == "" {
			continue
		}
		recorded, err := r.attributeRunSpend(plan, snapshot, stageID, projection.RunID)
		if err != nil {
			return report, err
		}
		attributed = attributed || recorded
	}
	// And the runs of stages a revision INVALIDATED. Their stage no longer
	// names them, and they can still be live: a run does not stop spending
	// because the plan stopped looking at it. They are attributed, and then
	// STOPPED - the plan created that run, the plan has replaced the stage it
	// was doing, and leaving it running means two concurrent runs for one
	// stage, one of them producing work the plan will never read.
	for _, runID := range snapshot.RetiredRuns {
		run, found, err := r.Store.Run(runID)
		if err != nil {
			return report, err
		}
		// The STAGE it was doing comes from the run's own binding. The stage no
		// longer names the run, so the projection cannot say - and attributing
		// with an empty stage id kept the plan total right while every per-stage
		// total silently undercounted what a retired run went on spending.
		stageID := ""
		if found && run.Plan != nil {
			stageID = run.Plan.StageID
		}
		recorded, err := r.attributeRunSpend(plan, snapshot, stageID, runID)
		if err != nil {
			return report, err
		}
		attributed = attributed || recorded
		stopped, err := r.stopRetiredRun(runID, retirementReason(plan, snapshot, run, found))
		if err != nil {
			return report, err
		}
		if stopped {
			report.Stopped = append(report.Stopped, runID)
		}
	}
	if attributed {
		if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
			return report, err
		}
	}

	for _, stage := range plan.Stages {
		projection := snapshot.Stages[stage.ID]
		if terminalStageState(projection.State) || projection.State == PlanStageRunning {
			continue
		}

		if awaiting {
			report.Waiting = fmt.Sprintf("awaiting operator approval of revision %d proposed by %s",
				pending.Proposed.Revision, pending.ID)
			report.Blocked = append(report.Blocked, PlanStageBlock{
				StageID: stage.ID, Kind: "revision",
				Reason: "a material plan revision proposal is awaiting approval; nothing new starts under a decomposition nobody accepted",
			})
			continue
		}
		ready, reason := planDependenciesSatisfied(stage, snapshot)
		if !ready {
			report.Blocked = append(report.Blocked, PlanStageBlock{StageID: stage.ID, Kind: "dependencies", Reason: reason})
			continue
		}
		switch stage.Kind {
		case domain.StageAssuranceGate, domain.StageHumanDecisionGate:
			satisfaction, satisfied, err := r.gateSatisfaction(stage, plan, snapshot)
			if err != nil {
				return report, err
			}
			if !satisfied {
				report.Blocked = append(report.Blocked, PlanStageBlock{
					StageID: stage.ID, Kind: string(stage.Kind),
					Reason: "the durable evidence, authority or human decision this gate references does not prove it yet",
				})
				continue
			}
			if err := r.appendPlan(planID, EventPlanGateSatisfied, satisfaction); err != nil {
				return report, err
			}
			report.Satisfied = append(report.Satisfied, stage.ID)
		case domain.StageAgent:
			// A PLANNER-role stage is the one agent stage that does not become
			// an EngineeringRun. It must not be able to write at all, so it runs
			// through the verified non-mutating boundary and its output crosses
			// the approval boundary instead of the candidate one.
			if stage.InvocationMode == domain.InvocationModeNonMutatingPlanning {
				assignment, assigned := resolution.Assignment(stage.ID)
				if !assigned {
					continue
				}
				blocked, err := r.decomposeStage(ctx, plan, stage, assignment)
				if err != nil {
					return report, err
				}
				if blocked != nil {
					report.Blocked = append(report.Blocked, *blocked)
				} else {
					report.Settled = append(report.Settled, stage.ID)
				}
				if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
					return report, err
				}
				// The rest of this pass is abandoned if the approved revision
				// moved while the planner ran. Every step after this point -
				// the pending-proposal check, the dependency reads, the stage
				// starts - uses the document loaded at the top, and a run
				// started from it now would be created AFTER the supersession
				// computed its invalidations: it would never appear in
				// RetiredRuns, never be stopped, and would execute work the
				// approved plan no longer contains.
				if moved, err := r.approvedRevisionMoved(plan); err != nil {
					return report, err
				} else if moved != 0 {
					report.Waiting = fmt.Sprintf("revision %d was approved during this pass; it is abandoned here and recomputed from the new revision on the next tick", moved)
					return report, nil
				}
				// A decomposition that just proposed a MATERIAL revision pauses
				// the rest of this pass. Continuing would start a stage whose
				// dependency completed by proposing that this very stage should
				// be something else - which is the hidden autonomous plan
				// replacement the approval boundary exists to prevent, reached
				// by a loop rather than by a design.
				if pending, awaiting, err = r.Store.PendingMaterialProposal(planID, plan.Revision); err != nil {
					return report, err
				}
				continue
			}
			started, blocked, err := r.startAgentStage(ctx, plan, stage, snapshot, resolution)
			if err != nil {
				return report, err
			}
			if blocked != nil {
				report.Blocked = append(report.Blocked, *blocked)
				continue
			}
			if started != nil {
				report.Started = append(report.Started, *started)
			}
		}
		if snapshot, err = r.Store.ReplayPlan(planID); err != nil {
			return report, err
		}
	}
	report.Consumed = snapshot.Consumed
	sort.SliceStable(report.Blocked, func(i, j int) bool { return report.Blocked[i].StageID < report.Blocked[j].StageID })
	return report, nil
}

// recordMissingSupersession completes the record when the approved revision
// replaced an earlier one and no supersession event says so.
//
// It is a top-up, not a second path: the same InvalidatedStages computation the
// approval uses, applied to the same two revisions, appended once. A plan whose
// approval and supersession both landed passes straight through.
func (r PlanReconciler) recordMissingSupersession(planID string, plan domain.EngineeringPlan, snapshot PlanSnapshot) (PlanSnapshot, error) {
	// The predecessor is the last revision that GOVERNED, which is not the
	// revision this one was compiled against: after a rejected proposal,
	// Provenance.PreviousRevision names the highest stored revision - including
	// one that never ran - and invalidations computed against a plan that never
	// executed are invalidations of the wrong work in both directions.
	// EVERY missing link, not only the one into the current revision. Two
	// approvals with no tick between them leave the middle revision's
	// invalidations unrecorded, and dependents would then build on work that
	// revision had already invalidated.
	recorded := map[int]bool{}
	for _, supersession := range snapshot.Superseded {
		recorded[supersession.ToRevision] = true
	}
	governed := snapshot.GoverningHistory()
	sort.Ints(governed)
	appended := false
	for i := 1; i < len(governed); i++ {
		from, to := governed[i-1], governed[i]
		if recorded[to] {
			continue
		}
		previous, found, err := r.Store.PlanRevision(planID, from)
		if err != nil {
			return snapshot, err
		}
		next, nextFound, err := r.Store.PlanRevision(planID, to)
		if err != nil {
			return snapshot, err
		}
		if !found || !nextFound {
			continue
		}
		if err := r.appendPlan(planID, EventPlanRevisionSuperseded, PlanRevisionSupersededPayload{
			FromRevision: from, ToRevision: to,
			ProposalID:        next.Provenance.ProposalID,
			InvalidatedStages: InvalidatedStages(previous, next, snapshot),
		}); err != nil {
			return snapshot, err
		}
		appended = true
	}
	if !appended {
		return snapshot, nil
	}
	return r.Store.ReplayPlan(planID)
}

// stopRetiredRun cancels a child run whose stage a revision replaced. It is the
// plan's own work: the plan created the run, and the stage it was doing is no
// longer in the plan. An operator's own run is never touched by this - only a
// run this plan created and then superseded.
func (r PlanReconciler) stopRetiredRun(runID, reason string) (bool, error) {
	run, found, err := r.Store.Run(runID)
	if err != nil || !found {
		return false, err
	}
	if run.Plan == nil || run.Disposition == Cancelled || terminalDisposition(run.Disposition) {
		return false, nil
	}
	scheduler := Scheduler{Store: r.Store, Clock: r.Clock, Owner: run.ControllerSHA256}
	if _, err := CancelRun(r.Store, scheduler, r.now(), runID, reason); err != nil {
		return false, err
	}
	return true, nil
}

// retirementReason says WHY the plan stopped a child run, which is not always
// supersession. A run retired because its stage is being PERFORMED AGAIN under
// the revision that is still governing was not superseded by anything, and
// recording that word would tell an operator a revision replaced work when none
// did.
func retirementReason(plan domain.EngineeringPlan, snapshot PlanSnapshot, run EngineeringRun, found bool) string {
	if !found || run.Plan == nil || run.Plan.Revision != plan.Revision {
		return "plan_stage_superseded"
	}
	// Under the SAME revision, the one thing that retires a run is the stage
	// moving to a later execution generation. A supersession resets the stage's
	// generation, so this cannot mistake one for the other.
	if run.Plan.Generation < snapshot.Stages[run.Plan.StageID].Generation {
		return "plan_stage_reperformed"
	}
	return "plan_stage_superseded"
}

// reopenStaleGates un-satisfies any gate whose proof no longer describes the
// work. It re-opens rather than re-deciding: the next pass evaluates the gate
// against what exists now, through exactly the same check that satisfied it.
func (r PlanReconciler) reopenStaleGates(plan domain.EngineeringPlan, snapshot PlanSnapshot) (bool, error) {
	reopened := false
	for _, stage := range plan.Stages {
		projection, ok := snapshot.Stages[stage.ID]
		if !ok || projection.State != PlanStageSatisfied || projection.Gate == nil {
			continue
		}
		moved, err := r.provenHeadsMoved(projection.Gate.ProvenHeads)
		if err != nil {
			return reopened, err
		}
		if !moved {
			continue
		}
		if err := r.appendPlan(plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
			StageID: stage.ID, Outcome: planStageInvalidated,
			Reason: "the work this gate was proved against has changed",
		}); err != nil {
			return reopened, err
		}
		reopened = true
	}
	return reopened, nil
}

// provenHeadsMoved reports whether any run named in a gate's proof now carries
// a different head. A gate recorded before heads were captured has none, and is
// left alone: it is a gate about work that predates the question.
func (r PlanReconciler) provenHeadsMoved(proven []string) (bool, error) {
	for _, entry := range proven {
		runID, head, ok := strings.Cut(entry, "@")
		if !ok || runID == "" {
			continue
		}
		current, err := r.runHead(runID)
		if err != nil {
			return false, err
		}
		if current != head {
			return true, nil
		}
	}
	return false, nil
}

// runHead is one run's current candidate head, from its own journal.
func (r PlanReconciler) runHead(runID string) (string, error) {
	events, err := r.Store.Events(runID)
	if err != nil {
		return "", err
	}
	projected, err := Project(events)
	if err != nil {
		return "", err
	}
	return projected.Head(), nil
}

// invalidateStaleCompletedStages invalidates a COMPLETED agent stage whose
// frozen upstream input has MOVED, and everything downstream of it.
//
// Reopening a stale gate is not enough. The shape that matters is
// `implementation -> independent review -> gate`: the reviewer's frozen
// assignment names the exact candidate it consumed, and a producer run at
// goal_state_reached is not finished - reviewer feedback re-activates it and it
// produces a different candidate. The gate above reopens, because its proof
// names the producer's run; the REVIEW below it stayed completed, so the next
// evaluation of that gate could be satisfied again by an independent review
// that was never performed on the work now being gated.
//
// A run whose head cannot be read, or an assignment recorded before upstream
// identity was captured, is left alone: this invalidates on proof that the
// input moved, never on absence of proof that it did not. Where it does fire,
// it errs toward performing the review again rather than reusing one whose
// subject may have changed - the expensive direction is the safe one here.
func (r PlanReconciler) invalidateStaleCompletedStages(plan domain.EngineeringPlan, snapshot PlanSnapshot) (bool, error) {
	stale := map[string]string{}
	for _, stage := range plan.Stages {
		projection, ok := snapshot.Stages[stage.ID]
		if !ok || projection.State != PlanStageCompleted {
			continue
		}
		assignment, found, err := r.Service.frozenAssignment(plan, projection, stage.ID)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		moved, err := r.movedUpstream(assignment)
		if err != nil {
			return false, err
		}
		if moved != "" {
			stale[stage.ID] = moved
		}
	}
	// Propagation is seeded from DURABLE STATE as well as from what this pass
	// just found, so it is idempotent and crash-safe. The settles below are
	// separate appends: a crash between them left a dependent completed under a
	// dependency that had been invalidated, and nothing re-derived it - the
	// root was no longer completed, so the head-movement sweep above no longer
	// saw it, and the dependent's own upstream had never moved. Re-reading the
	// dependency's state every pass is what closes that, rather than ordering
	// the appends and hoping.
	//
	// ONLY a same-revision invalidation seeds it. A stage a SUPERSESSION
	// invalidated is waiting to be performed under the new revision, and
	// propagating from it would mark its dependents as invalidated under this
	// one - which is the state that says "not performable here" and would
	// block the whole graph permanently.
	for _, stage := range plan.Stages {
		projection, ok := snapshot.Stages[stage.ID]
		if ok && projection.State == PlanStageInvalidated && projection.InvalidatedUnder == plan.Revision {
			stale[stage.ID] = "it was invalidated"
		}
	}
	if len(stale) == 0 {
		return false, nil
	}
	// Downstream of a stage that must be redone is also invalid: its input is
	// about to be replaced. This is the same propagation a revision performs,
	// for the same reason.
	for progressed := true; progressed; {
		progressed = false
		for _, stage := range plan.Stages {
			if _, already := stale[stage.ID]; already {
				continue
			}
			for _, dependency := range stage.DependsOn {
				if _, invalid := stale[dependency]; invalid {
					stale[stage.ID] = "the work it depends on is being redone"
					progressed = true
					break
				}
			}
		}
	}
	ids := make([]string, 0, len(stale))
	for id := range stale {
		projection, ok := snapshot.Stages[id]
		if !ok || projection.State == PlanStagePending {
			// A stage that never did anything has nothing to invalidate; it is
			// simply performed under whatever the plan says now.
			continue
		}
		if projection.State == PlanStageInvalidated && projection.InvalidatedUnder == plan.Revision {
			// Already recorded. Appending it again on every tick is the same
			// unbounded loop in a quieter form.
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.appendPlan(plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
			StageID: id, Outcome: planStageInvalidated, Revision: plan.Revision,
			Reason: boundedDetail("the upstream work this stage was performed against has changed: " + stale[id]),
		}); err != nil {
			return false, err
		}
	}
	return len(ids) > 0, nil
}

// startedRun reports whether a run already exists for one performance of a
// stage, whatever the plan journal says about it.
//
// It asks the durable RUNS rather than the projection, because the projection
// is exactly what can be missing: `plan.stage_assigned` is appended after the
// run is created, so a crash between the two leaves a live run nothing names.
//
// It scans, and that is deliberate: there is no plan index over runs, and this
// is only reached on the rare path where a start failed AND the input it froze
// has since been replaced. An index for a branch taken that seldom would be
// carried by every write for the benefit of almost none.
func (r PlanReconciler) startedRun(plan domain.EngineeringPlan, stageID string, generation int) (bool, error) {
	runs, err := r.Store.Runs()
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Plan == nil {
			continue
		}
		if run.Plan.PlanID == plan.ID && run.Plan.Revision == plan.Revision &&
			run.Plan.StageID == stageID && run.Plan.Generation == generation {
			return true, nil
		}
	}
	return false, nil
}

// movedUpstream names the first upstream output an assignment froze whose
// producer has since SETTLED on a different candidate, or the empty string.
//
// It is ONE predicate with two callers, deliberately. The sweep uses it to
// decide that completed work is stale; the start path uses it to decide that a
// durable assignment nothing ever ran is stale. Two copies would be two answers
// to "did this stage's input move", and for a long time the second copy simply
// did not exist - which is how an assignment frozen against a candidate that
// had since been replaced could still be the one that executed.
//
// Two things make it conservative on purpose:
//
//   - It compares against the RUN'S OWN record of its candidate, which is the
//     same field the freeze copied, so the two are like for like. The journal
//     projection can be momentarily ahead of it mid-pass, which would read as
//     movement and throw away a review that had just been performed against the
//     candidate the assignment names.
//   - Only a head the producer is FINISHED WITH counts. The run row's candidate
//     is refreshed on every reconcile of that run - every commit and checkpoint
//   - not on publication or settlement. A producer re-activated by reviewer
//     feedback moves it on the first checkpoint, long before it has produced
//     anything a reviewer should consume, and firing there would bind the next
//     performance to an interim, possibly unpushed head.
//
// The detail is BOUNDED, because it becomes a journal field: two full candidate
// heads and a stage id exceed the 200-byte field bound with ordinary production
// ids, and the append would be refused - so the reconciler would error on that
// plan on every tick, forever.
func (r PlanReconciler) movedUpstream(assignment domain.AgentAssignment) (string, error) {
	for _, upstream := range assignment.Context.UpstreamOutputs {
		if upstream.RunID == "" || upstream.Candidate == "" {
			continue
		}
		run, found, err := r.Store.Run(upstream.RunID)
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		if _, settled := stageOutcome(run); !settled {
			continue
		}
		head := run.Candidate.Revision
		if head == "" || head == upstream.Candidate {
			continue
		}
		return boundedDetail(fmt.Sprintf("it consumed %s from stage %s, which is now at %s",
			upstream.Candidate, upstream.StageID, head)), nil
	}
	return "", nil
}

// refusePrivilegeChange is the boundary between renewing an obligation and
// changing one.
//
// A new execution generation exists because an execution FACT moved: the
// candidate this stage consumes was replaced. The approved obligation - this
// role, this profile at this exact version and digest, this worker, this trust
// ceiling, this invocation mode, this independence requirement - is unchanged,
// and performing it again needs no new approval. If re-resolving would now
// produce a different one of those, that is not the same obligation, and it
// goes through the approval boundary as a revision like anything else.
//
// The comparison is STRUCTURAL, not a list of fields somebody remembered. The
// named rows below exist to say WHICH thing changed in a message an operator
// can act on; what decides is the canonical form of the whole assignment with
// only the execution facts a generation is allowed to move erased. A field
// added to AgentAssignment later is therefore protected by default rather than
// by whoever edits this function next - which is how an authority boundary has
// to fail.
//
// Obligation renewal may be automatic. Authority change may not.
func (r PlanReconciler) refusePrivilegeChange(plan domain.EngineeringPlan, stage domain.PlanStage, generation int, next domain.AgentAssignment) *PlanStageBlock {
	// The PREVIOUS PERFORMANCE, wherever it was recorded - not
	// (this revision, generation-1). Assignment rows are written under the
	// revision that governed when the stage started, and a revision that did
	// not change the stage leaves its completed work, and its row, under the
	// older one. Looking only under the current revision missed the row
	// exactly in the multi-revision histories this guard exists for, found
	// nothing to compare, and let the re-performance freeze whatever the
	// registry resolves today - a different worker included, with no block.
	previous, found, err := r.Store.PreviousPlanAssignment(plan.ID, plan.Revision, generation, stage.ID)
	if err != nil {
		return &PlanStageBlock{StageID: stage.ID, Kind: "assignment", Reason: boundedDetail(err.Error())}
	}
	if !found {
		// FAIL CLOSED. This stage is being performed AGAIN, so a previous
		// performance exists by definition; not being able to read what it was
		// frozen under proves nothing about whether the obligation is the same,
		// and absence of proof was being read as proof of sameness.
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: "propose a revision: this stage is being performed again and the assignment its previous performance was frozen under cannot be read, so nothing proves the obligation is unchanged",
		}
	}
	// APPROVED-PLAN PROVENANCE MAY ADVANCE. It is not authority.
	//
	// A stage carried forward unchanged into a later approved revision is
	// performed again under THAT revision, so the assignment resolved for the
	// renewal necessarily carries the new revision number and the new plan
	// digest. Reading that as a changed obligation refused exactly the renewal
	// this boundary exists to permit.
	//
	// What may not move is the plan's durable IDENTITY, and the direction of
	// travel. The renewal runs under the revision the reconciler is executing -
	// which is the APPROVED one, because nothing else reaches here - and the
	// performance it renews is an earlier one under the same plan. Stating both
	// structurally is what keeps "the revision advanced" from becoming a way to
	// compare against something that was never approved.
	if previous.Plan.ID != next.Plan.ID {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: performing this stage again would change its plan, which is a different obligation than the one approved (%s becomes %s)",
				shortValue(previous.Plan.ID), shortValue(next.Plan.ID))),
		}
	}
	if next.Plan.Revision != plan.Revision {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: this stage's renewal was resolved under revision %d while revision %d is executing, so nothing proves it renews what was approved",
				next.Plan.Revision, plan.Revision)),
		}
	}
	// The DIGEST too, for the same reason and with no more trust. Equal by
	// construction on the production path - the resolver stamps it from the
	// plan being executed, and an approval verified it - but the assignment
	// this writes is a durable provenance claim, and a row recording a digest
	// the executing revision does not have is a false one whatever produced it.
	if next.Plan.Digest != plan.Digest {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: this stage's renewal names plan content %s and revision %d is %s, so it was not resolved from the approved document",
				shortValue(next.Plan.Digest), plan.Revision, shortValue(plan.Digest))),
		}
	}
	if previous.Plan.Revision > plan.Revision {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: this stage's previous performance is recorded under revision %d, which is later than the executing revision %d, so it is not a performance this one renews",
				previous.Plan.Revision, plan.Revision)),
		}
	}
	for _, difference := range []struct{ what, before, now string }{
		{"role", string(previous.Role), string(next.Role)},
		// Contract IDENTITY. The per-revision pointer beside it may advance
		// with the approved plan and is compared by CONTENT below; which
		// contract this stage answers may not change at all.
		{"contract", previous.Contract.ID, next.Contract.ID},
		{"profile", fmt.Sprintf("%s v%d %s", previous.Profile.ID, previous.Profile.Version, previous.Profile.Digest), fmt.Sprintf("%s v%d %s", next.Profile.ID, next.Profile.Version, next.Profile.Digest)},
		// The profile DOCUMENT names its instruction packs and context policy
		// by id; the registry resolves and freezes their content digests
		// separately. Editing a pack in place leaves the profile id, version
		// and digest identical, so comparing those alone let a new generation
		// execute different operator instructions with no approval.
		{"instruction packs", packSummary(previous.Profile.Instructions), packSummary(next.Profile.Instructions)},
		{"context policy", packSummary(contextPolicyRefs(previous.Profile)), packSummary(contextPolicyRefs(next.Profile))},
		{"worker", previous.Agent.ID, next.Agent.ID},
		// The WHOLE binding, field by field. Two registry entries can share an
		// id and differ in what they actually are, and the independence
		// obligations downstream stages carry are stated in terms of provider
		// kind and vendor family - so an id-only comparison would call a
		// different provider the same obligation.
		{"provider kind", previous.Agent.ProviderKind, next.Agent.ProviderKind},
		{"vendor family", previous.Agent.VendorFamily, next.Agent.VendorFamily},
		{"model", previous.Agent.Model, next.Agent.Model},
		{"trust mode", string(previous.Agent.TrustMode), string(next.Agent.TrustMode)},
		{"trust requirement", string(previous.TrustRequirement), string(next.TrustRequirement)},
		{"invocation mode", string(previous.InvocationMode), string(next.InvocationMode)},
		{"independence", independenceSummary(previous), independenceSummary(next)},
	} {
		if difference.before == difference.now {
			continue
		}
		// The REMEDY leads, and the values are shortened rather than the
		// sentence: two profile digests are 128 characters, and a bound that
		// cuts the end would take the actionable half of the message with it.
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: performing this stage again would change its %s, which is a different obligation than the one approved (%s becomes %s)",
				difference.what, shortValue(difference.before), shortValue(difference.now))),
		}
	}
	// The OBLIGATIONS, by content. The pointer to them advances with the
	// approved plan; what they say may not change without approval.
	if block := r.refuseContractChange(plan, stage, previous, next); block != nil {
		return block
	}
	// The stage's own bounds may NARROW - a profile that tightens is
	// customization doing what it is allowed to do - and may not be raised.
	if what := budgetEscalation(previous.Budget, next.Budget); what != "" {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: performing this stage again would raise its %s, which is more authority than the one approved", what)),
		}
	}
	// EVERYTHING ELSE, structurally. Capabilities, contract identity, required
	// capabilities, the context the worker is shown, the independence bindings
	// - and whatever this record gains later.
	before, beforeErr := domain.CanonicalJSON(renewalScope(previous))
	now, nowErr := domain.CanonicalJSON(renewalScope(next))
	if beforeErr != nil || nowErr != nil {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: "propose a revision: this stage's previous and next assignments could not be compared, and an unprovable obligation is not a renewed one",
		}
	}
	if !bytes.Equal(before, now) {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: "propose a revision: performing this stage again would change what it was frozen under - the profile, context, contract or obligations it carries - which is a different obligation than the one approved",
		}
	}
	return nil
}

// renewalScope is an assignment reduced to what a new execution generation may
// NOT change.
//
// What it erases is the whole permitted difference between two performances of
// the same approved obligation: which performance this is, the run it became,
// the resolver's explanation of how it chose, the upstream candidate whose
// replacement is the reason for the generation, the budget - which is compared
// separately because it may narrow - and the two POINTERS that name where the
// approved obligation is written down rather than what it says.
//
// The pointers are the plan's revision and digest, and the contract's revision.
// Both advance when a stage is carried forward unchanged into a later approved
// revision, and both are checked by the comparisons above this: the plan by
// identity and direction of travel, the contract by identity and by the content
// of the compiled document itself. Erasing them here without those checks would
// move an authority boundary, not relax a false one.
//
// Everything left is durable configuration or authority, and it has to be
// identical. The candidate a stage consumes moves; what an operator approved
// about how it is worked does not.
func renewalScope(assignment domain.AgentAssignment) domain.AgentAssignment {
	assignment.ID = ""
	assignment.RunID = ""
	assignment.Selection = domain.ResolutionExplanation{}
	assignment.Context.UpstreamOutputs = nil
	assignment.Budget = domain.StageBudget{}
	assignment.Plan.Revision, assignment.Plan.Digest = 0, ""
	assignment.Contract.Revision = ""
	return assignment
}

// refusePrivilegeChange's boundary, applied to the OBLIGATIONS a stage is
// performed under.
//
// Compiled contracts are stored per plan revision, so a stage carried forward
// unchanged into a later approved revision references a different contract
// revision while the document says exactly the same thing. The revision is a
// POINTER: comparing it refused renewals that changed nothing. Comparing
// nothing at all would be worse - a materially different compiled contract is a
// different obligation and goes through the approval boundary like any other.
//
// So the CONTENT is what is compared, and it is read from the durable contracts
// the two revisions were actually planned against rather than from the pointer
// the assignment carries.
func (r PlanReconciler) refuseContractChange(plan domain.EngineeringPlan, stage domain.PlanStage, previous, next domain.AgentAssignment) *PlanStageBlock {
	if previous.Plan.Revision == next.Plan.Revision {
		// One contract governs one revision and is immutable under it, so both
		// performances were resolved against the same stored document.
		return nil
	}
	before, block := r.obligationDigest(plan.ID, previous.Plan.Revision, stage)
	if block != nil {
		return block
	}
	now, block := r.obligationDigest(plan.ID, next.Plan.Revision, stage)
	if block != nil {
		return block
	}
	if before == now {
		return nil
	}
	return &PlanStageBlock{
		StageID: stage.ID, Kind: "authority",
		Reason: boundedDetail(fmt.Sprintf(
			"propose a revision: performing this stage again would perform it under different compiled obligations, which is a different obligation than the one approved (%s becomes %s)",
			shortValue(before), shortValue(now))),
	}
}

// obligationDigest is what one revision's compiled contract OBLIGES, as one
// comparable value.
//
// The contract's own revision string and the predecessor it names are erased:
// they are where the document sits in a chain, not what it requires. Everything
// else - objective, acceptance intent, subject, scope, facts, invariants,
// obligations, required claims, permissions, prohibitions, authority
// conditions, plan requirements, and the project model and policy it was
// compiled from - is content, and content is the invariant.
//
// A contract that cannot be read FAILS CLOSED. A stage is being performed
// again, so both revisions were planned against something; not being able to
// read one proves nothing about whether the obligations are the same.
func (r PlanReconciler) obligationDigest(planID string, revision int, stage domain.PlanStage) (string, *PlanStageBlock) {
	contract, found, err := r.Store.PlanContract(planID, revision)
	if err != nil {
		return "", &PlanStageBlock{StageID: stage.ID, Kind: "assignment", Reason: boundedDetail(err.Error())}
	}
	if !found {
		return "", &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: boundedDetail(fmt.Sprintf(
				"propose a revision: this stage is being performed again and the contract revision %d was planned against cannot be read, so nothing proves its obligations are unchanged", revision)),
		}
	}
	contract.Revision = ""
	contract.Provenance.PreviousContractRevision = nil
	digest, err := domain.Digest(contract)
	if err != nil {
		return "", &PlanStageBlock{
			StageID: stage.ID, Kind: "authority",
			Reason: "propose a revision: this stage's previous and next obligations could not be compared, and an unprovable obligation is not a renewed one",
		}
	}
	return digest, nil
}

// budgetEscalation names the first bound a re-performance would RAISE, or the
// empty string. An absent bound is unbounded, so dropping one is an escalation
// and adding one is a narrowing.
func budgetEscalation(previous, next domain.StageBudget) string {
	for _, bound := range []struct {
		what        string
		before, now int
	}{
		{"execution attempt ceiling", previous.MaxExecutionAttempts, next.MaxExecutionAttempts},
		{"provider invocation ceiling", previous.MaxProviderInvocations, next.MaxProviderInvocations},
		{"wall clock ceiling", previous.MaxWallSeconds, next.MaxWallSeconds},
	} {
		if bound.before <= 0 {
			continue
		}
		if bound.now <= 0 || bound.now > bound.before {
			return bound.what
		}
	}
	return ""
}

// packSummary is a set of pack references as one comparable string, digests
// included: the digest is the content, and the id alone is a name for whatever
// the file says today.
func packSummary(refs []domain.PackRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf("%s@%s/%s", ref.ID, ref.Revision, ref.Digest))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// contextPolicyRefs is the profile's context policy as a slice, so the same
// comparison covers "there is one" and "there is none".
func contextPolicyRefs(profile domain.ProfileBinding) []domain.PackRef {
	if profile.ContextPolicy == nil {
		return nil
	}
	return []domain.PackRef{*profile.ContextPolicy}
}

// shortValue keeps a comparison readable inside a bounded field. Identity is
// established by the comparison itself; the message only has to say WHICH thing
// differs, not carry two full digests.
func shortValue(value string) string {
	const enough = 24
	if len(value) <= enough {
		return value
	}
	return boundedTo(value, enough) + "..."
}

// independenceSummary is the independence obligation as one comparable string.
func independenceSummary(assignment domain.AgentAssignment) string {
	parts := make([]string, 0, len(assignment.Independence))
	for _, binding := range assignment.Independence {
		parts = append(parts, fmt.Sprintf("%s/%s/%s/%s", binding.Dimension,
			strings.Join(binding.DifferentFrom, "+"), binding.Class, binding.SatisfiedBy))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// startAgentStage creates or associates the ordinary EngineeringRun for one
// dependency-ready agent stage, within the plan's aggregate envelope.
func (r PlanReconciler) startAgentStage(ctx context.Context, plan domain.EngineeringPlan, stage domain.PlanStage, snapshot PlanSnapshot, resolution planningResolution) (*PlanStageRun, *PlanStageBlock, error) {
	assignment, ok := resolution.Assignment(stage.ID)
	if !ok {
		// The resolver already recorded why, and that block is in the report.
		return nil, nil, nil
	}
	// WHICH EXECUTION of this stage this is. Zero is the first, and a later one
	// exists only because the upstream candidate this stage consumed was
	// replaced. The approved obligation is unchanged, so it is performed again
	// under the same revision - but as its own assignment and its own run,
	// because reusing either would hand the new performance the input whose
	// replacement is the reason for it.
	generation := snapshot.Stages[stage.ID].Generation
	// THE ASSIGNMENT THAT WILL ACTUALLY EXECUTE, before anything is checked
	// against it.
	//
	// PutPlanAssignment keeps the first row written for a (revision, stage,
	// generation), so a row frozen by an earlier start attempt is what a retry
	// executes - not the resolution this pass just produced. Every guard below
	// therefore has to see that row. Checking the fresh resolution instead
	// checked an assignment that was about to be discarded on conflict, and the
	// one that ran was never checked at all: a start that failed after the
	// freeze, followed by the producer settling a different candidate, left the
	// stale frozen assignment to be loaded and executed while the guard passed
	// on the replacement it would never use.
	durable, frozenAlready, err := r.Store.PlanAssignment(plan.ID, plan.Revision, generation, stage.ID)
	if err != nil {
		return nil, nil, err
	}
	if frozenAlready {
		// A frozen assignment NOTHING EVER RAN is intent, not history. The
		// stage_assigned projection is written only after a run was created
		// under it, so an assignment the projection does not name is one whose
		// start never completed - the engine refused, the process died, the
		// tick errored.
		//
		// If its input has since been replaced, the performance it was frozen
		// for will never happen. Blocking would be safe and permanent: the row
		// is immutable, so every later tick would read the same stale
		// assignment and refuse again. So the stage advances an execution
		// GENERATION instead, exactly as it would if that performance had run
		// and been invalidated - the abandoned row stays where it is, the next
		// performance is frozen against what the producer actually settled, and
		// the privilege boundary compares the two.
		if snapshot.Stages[stage.ID].AssignmentID == "" {
			moved, err := r.movedUpstream(durable)
			if err != nil {
				return nil, nil, err
			}
			// Unless a RUN was created under it after all. The association
			// event is appended after the run exists, so a crash between the
			// two leaves a live run the projection does not name; discarding
			// that freeze would advance the generation and leave the run
			// executing, unstopped and attributed to nothing. Its stage is
			// associated instead, and the ordinary sweep invalidates and
			// retires it the way it does any run whose input moved under it.
			started := false
			if moved != "" {
				if started, err = r.startedRun(plan, stage.ID, generation); err != nil {
					return nil, nil, err
				}
			}
			if moved != "" && !started {
				if err := r.appendPlan(plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
					StageID: stage.ID, Outcome: planStageInvalidated, Revision: plan.Revision,
					Reason: boundedDetail("an assignment was frozen for this stage and no run was created under it, and " + moved),
				}); err != nil {
					return nil, nil, err
				}
				// The next tick performs the new generation. Nothing started
				// here, and nothing is blocked: the stage is simply not yet at
				// the assignment it will run.
				return nil, nil, nil
			}
		}
		assignment = durable
	}
	// The upstream heads this assignment FREEZES have to be heads their
	// producers are FINISHED WITH. A stage stays completed in plan state while
	// its run is re-activated by reviewer feedback, and the run row's candidate
	// moves on the first checkpoint after that - so a stage starting in the
	// window between "settled" and "back at work" would freeze itself against a
	// head that is being changed while it reads it. The sweep applies the same
	// rule to deciding that work is stale; this applies it to what a new
	// performance is bound to.
	for _, upstream := range assignment.Context.UpstreamOutputs {
		if upstream.RunID == "" {
			continue
		}
		run, found, err := r.Store.Run(upstream.RunID)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}
		if _, settled := stageOutcome(run); !settled {
			return nil, &PlanStageBlock{
				StageID: stage.ID, Kind: "upstream",
				Reason: boundedDetail(fmt.Sprintf("stage %s is producing a different candidate; this stage starts against the head it settles on", upstream.StageID)),
			}, nil
		}
	}
	// BUDGET, before anything durable happens. The aggregate envelope is what
	// an operator approved, and a plan that has spent it stops rather than
	// quietly exceeding it.
	if snapshot.Consumed.ChildRuns >= plan.BudgetEnvelope.MaxChildRuns {
		return nil, &PlanStageBlock{
			StageID: stage.ID, Kind: "budget",
			Reason: fmt.Sprintf("the plan's aggregate envelope allows %d child runs and %d have been created", plan.BudgetEnvelope.MaxChildRuns, snapshot.Consumed.ChildRuns),
		}, nil
	}
	if block := invocationCeilingReached(plan, snapshot, stage.ID); block != nil {
		return nil, block, nil
	}
	if active := activeStages(snapshot); active >= plan.BudgetEnvelope.MaxConcurrency {
		return nil, &PlanStageBlock{
			StageID: stage.ID, Kind: "concurrency",
			Reason: fmt.Sprintf("the plan allows %d concurrent stages and %d are running", plan.BudgetEnvelope.MaxConcurrency, active),
		}, nil
	}
	if r.Engine == nil {
		return nil, nil, &PlanRefusedError{PlanID: plan.ID, Detail: "no engine factory is configured, so no run can be created"}
	}
	if generation > 0 && !frozenAlready {
		if block := r.refusePrivilegeChange(plan, stage, generation, assignment); block != nil {
			return nil, block, nil
		}
		assignment.ID = fmt.Sprintf("%s-g%d", assignment.ID, generation)
	}
	if err := r.Store.PutPlanAssignment(plan.ID, plan.Revision, generation, assignment); err != nil {
		return nil, nil, err
	}
	// The STORED assignment is the one that runs. A row already frozen for this
	// (plan, revision, stage, generation) is kept, so a re-resolution that
	// picked a different worker must not be what the journal, the run binding
	// and the report describe: that is a tamper-evident journal naming worker B
	// while worker A does the work.
	frozen, found, err := r.Store.PlanAssignment(plan.ID, plan.Revision, generation, stage.ID)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, fmt.Errorf("assignment for plan %s revision %d stage %s was stored and could not be read back", plan.ID, plan.Revision, stage.ID)
	}
	assignment = frozen
	engine, err := r.Engine(r.Repository, assignment.Agent.ID)
	if err != nil {
		return nil, &PlanStageBlock{StageID: stage.ID, Kind: "agent", Reason: boundedDetail(err.Error())}, nil
	}
	upstreamBaseRevision, err := r.upstreamBase(assignment)
	if err != nil {
		return nil, &PlanStageBlock{StageID: stage.ID, Kind: "upstream", Reason: boundedDetail(err.Error())}, nil
	}
	binding := RunPlanBinding{
		PlanID: plan.ID, Revision: plan.Revision, PlanDigest: plan.Digest,
		StageID: stage.ID, AssignmentID: assignment.ID, Generation: generation,
		BaseRevision: upstreamBaseRevision,
		// The assignment's budget is the stage's, already narrowed by the
		// assigned profile's constraints, and narrowed AGAIN by what the plan
		// has left. A ceiling that only refuses the NEXT stage after an
		// overspend is a report, not a ceiling: the run has to be bounded by
		// the remainder when it starts, because one run makes several execution
		// bindings and can outspend the remainder before anything settles.
		StageBudget: remainingHeadroom(plan, snapshot).tighten(assignment.Budget),
	}
	outcome, err := engine.StartPlanStageRun(ctx, r.Issue, binding)
	if err != nil {
		return nil, &PlanStageBlock{StageID: stage.ID, Kind: "run", Reason: boundedDetail(err.Error())}, nil
	}
	if err := r.appendPlan(plan.ID, EventPlanStageAssigned, PlanStageAssignedPayload{
		StageID: stage.ID, AssignmentID: assignment.ID, Role: string(assignment.Role),
		ProfileID: assignment.Profile.ID, ProfileVersion: assignment.Profile.Version, ProfileDigest: assignment.Profile.Digest,
		AgentID: assignment.Agent.ID, ProviderKind: assignment.Agent.ProviderKind, VendorFamily: assignment.Agent.VendorFamily,
		TrustMode: string(assignment.Agent.TrustMode), InvocationMode: string(assignment.InvocationMode),
		RunID: outcome.RunID,
	}); err != nil {
		return nil, nil, err
	}
	// One child run consumed. Consumption is a delta against an immutable
	// record, so nothing later - a revision, a restart, a reassignment - can
	// take it back. It is recorded BEFORE the run is announced and keyed by the
	// run, so a crash between the two appends over-counts nothing on replay and
	// loses nothing either: the next tick re-derives the same run and the same
	// key, and the key is counted once.
	if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		Key: "child_run:" + outcome.RunID, StageID: stage.ID, RunID: outcome.RunID, ChildRuns: 1,
	}); err != nil {
		return nil, nil, err
	}
	if err := r.appendPlan(plan.ID, EventPlanRunStarted, PlanRunStartedPayload{StageID: stage.ID, RunID: outcome.RunID}); err != nil {
		return nil, nil, err
	}
	return &PlanStageRun{StageID: stage.ID, RunID: outcome.RunID, AgentID: assignment.Agent.ID}, nil, nil
}

// upstreamBase is the published upstream candidate this stage should build on.
//
// It is deliberately narrow. Only a PUBLISHED candidate qualifies: the run's
// workspace is cloned from the governed remote, so a commit that exists only in
// another run's local workspace cannot be its base. Where several upstream
// stages published, the last one in the assignment's own order wins - a plan
// with two independent producers feeding one stage is an integration the
// reconciler cannot invent, and one of them being the base is the honest
// approximation until an integration stage materializes both.
// A read failure here is NOT "not published". Deciding that from an error
// bases a reviewing stage on the trusted base and hands it nothing to review -
// which is the exact blocking finding the first live dogfood review reported
// about its own workspace, reached through a different door. The error is
// returned and the stage waits.
func (r PlanReconciler) upstreamBase(assignment domain.AgentAssignment) (string, error) {
	base := ""
	for _, upstream := range assignment.Context.UpstreamOutputs {
		if upstream.RunID == "" || upstream.Candidate == "" {
			continue
		}
		events, err := r.Store.Events(upstream.RunID)
		if err != nil {
			return "", fmt.Errorf("upstream stage %q run %s could not be read: %w", upstream.StageID, upstream.RunID, err)
		}
		projection, err := Project(events)
		if err != nil {
			return "", fmt.Errorf("upstream stage %q run %s could not be projected: %w", upstream.StageID, upstream.RunID, err)
		}
		if projection.PullRequest == nil {
			// Not published. Its commit is not on the remote, so it cannot be
			// cloned; the stage stays based on the trusted base and receives
			// the diff as context.
			continue
		}
		base = upstream.Candidate
	}
	return base, nil
}

// attributeRunSpend records what one child run has spent SO FAR, as the delta
// since the last time this plan attributed it.
//
// It is called every tick, not only at settlement. A stage settles when its
// work reaches goal state, and a goal-state run is not terminal: admitted
// reviewer feedback re-activates it and it spends more invocations and more
// active time. Attributing once, at settlement, made every one of those later
// invocations invisible to the plan's ceilings.
//
// The key carries the running TOTAL, so each increment is its own fact and the
// count-once fold still refuses a replayed one.
func (r PlanReconciler) attributeRunSpend(plan domain.EngineeringPlan, snapshot PlanSnapshot, stageID, runID string) (bool, error) {
	invocations, err := r.providerInvocations(runID)
	if err != nil {
		return false, err
	}
	active, err := r.activeSeconds(runID)
	if err != nil {
		return false, err
	}
	recorded := false
	attributed := snapshot.RunConsumed[runID]
	if delta := invocations - attributed.ProviderInvocations; delta > 0 {
		if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			Key: fmt.Sprintf("invocations:%s:%d", runID, invocations), StageID: stageID, RunID: runID,
			ProviderInvocations: delta,
		}); err != nil {
			return false, err
		}
		recorded = true
	}
	// ACTIVE wall time, by the same definition the run's own wall budget uses:
	// elapsed less what the run spent waiting on something external. A plan
	// whose stages wait days for a reviewer has not spent days of execution,
	// and a ceiling that counted them would stop work nobody was doing.
	if delta := active - attributed.WallSeconds; delta > 0 {
		if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			Key: fmt.Sprintf("wall:%s:%d", runID, active), StageID: stageID, RunID: runID,
			WallSeconds: delta,
		}); err != nil {
			return false, err
		}
		recorded = true
	}
	return recorded, nil
}

// settleFinishedStages records how each running stage's child run ended, and
// what it spent.
func (r PlanReconciler) settleFinishedStages(plan domain.EngineeringPlan, snapshot PlanSnapshot) ([]string, error) {
	var settled []string
	for _, stage := range plan.Stages {
		projection, ok := snapshot.Stages[stage.ID]
		if !ok || projection.State != PlanStageRunning || projection.RunID == "" {
			continue
		}
		run, found, err := r.Store.Run(projection.RunID)
		if err != nil {
			return settled, err
		}
		if !found {
			continue
		}
		// A stage is done when its WORK is done, which is not the same as its
		// run being terminal. A published run waits for a person in the forge
		// and stays non-terminal for as long as that takes; a plan that treated
		// that as "not finished" would never start the review of the change
		// that run just produced.
		outcome, done := stageOutcome(run)
		if !done {
			continue
		}
		if _, err := r.attributeRunSpend(plan, snapshot, stage.ID, projection.RunID); err != nil {
			return settled, err
		}
		if err := r.appendPlan(plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
			StageID: stage.ID, Outcome: outcome, Reason: boundedDetail(run.Reason),
		}); err != nil {
			return settled, err
		}
		settled = append(settled, stage.ID)
	}
	return settled, nil
}

// stageOutcome maps a child run's state onto what it means for the plan stage.
//
// Completed and goal-state-reached are both COMPLETE: the first is a run that
// finished, the second is a run that produced its candidate, passed assurance
// and is waiting for a person - which is exactly the point at which the next
// stage has something to work with. Every other non-terminal state is still in
// progress, and cancellation or failure is a failed stage.
func stageOutcome(run EngineeringRun) (string, bool) {
	switch {
	case run.Disposition == Completed:
		return "completed", true
	case run.Disposition == Waiting && run.Reason == ReasonGoalStateReached:
		return "completed", true
	case terminalDisposition(run.Disposition):
		return "failed", true
	default:
		return "", false
	}
}

// providerInvocations counts the execution attempts one run actually made.
func (r PlanReconciler) providerInvocations(runID string) (int, error) {
	operations, err := r.Store.Operations(runID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, operation := range operations {
		if operation.Kind == OpExecutionInvoke {
			total += operation.Attempt
		}
	}
	return total, nil
}

// gateSatisfaction answers whether a typed gate's EXISTING durable references
// prove it, and returns the references that did.
//
// Nothing here produces evidence, and nothing here decides authority. It reads
// what the kernel already recorded about the runs this gate depends on.
func (r PlanReconciler) gateSatisfaction(stage domain.PlanStage, plan domain.EngineeringPlan, snapshot PlanSnapshot) (PlanGateSatisfiedPayload, bool, error) {
	payload := PlanGateSatisfiedPayload{StageID: stage.ID, Kind: string(stage.Kind), Claims: stage.RequiredClaims}
	runs := upstreamRuns(stage, plan, snapshot)
	if len(runs) == 0 {
		// A gate with no upstream run has nothing to be satisfied BY. It waits
		// rather than being treated as vacuously true: a gate that passes
		// because nothing happened is the fake evidence #64 refuses.
		return payload, false, nil
	}
	// PROVING runs are the ones that produced something to judge. A reviewing
	// stage produces no candidate and therefore has no assurance observation of
	// its own; requiring one of every upstream run would make a gate after a
	// review permanently unsatisfiable, and treating a run with nothing to judge
	// as proof would make the gate vacuous. So: every upstream run that HAS a
	// verdict must pass, and at least one must exist.
	proving := 0
	for _, runID := range runs {
		events, err := r.Store.Events(runID)
		if err != nil {
			return payload, false, err
		}
		projected, err := Project(events)
		if err != nil {
			return payload, false, err
		}
		switch stage.Kind {
		case domain.StageAssuranceGate:
			if projected.Assurance == nil {
				continue
			}
			proving++
			if projected.Assurance.Stale || !projected.Assurance.Passed {
				return payload, false, nil
			}
			// The independent semantic verdict, where one exists, has to agree.
			// Letting an automated test pass stand in for it would be exactly
			// the aliasing the evidence model forbids.
			if projected.SemanticAssurance != nil && (!projected.SemanticAssurance.Passed || projected.SemanticAssurance.Stale) {
				return payload, false, nil
			}
			payload.Evidence = projected.Assurance.Bundle
			payload.ProvingRuns = append(payload.ProvingRuns, runID)
			payload.ProvenHeads = append(payload.ProvenHeads, runID+"@"+projected.Head())
		case domain.StageHumanDecisionGate:
			decision, satisfied := humanDecision(events, stage.Action, projected.Head())
			if !satisfied {
				continue
			}
			// The gate's CLAIMS have to be what the person answered. A gate
			// that states claims and accepts any human decision on the run -
			// including a routine publication authorization - records claims it
			// never checked, which is a gate that reads as proof of something
			// nobody was asked.
			if !answersClaims(decision, stage.RequiredClaims) {
				continue
			}
			proving++
			payload.Decision = decision.decision
			payload.HumanEvidenceID = decision.humanEvidenceID
			payload.ProvingRuns = append(payload.ProvingRuns, runID)
			payload.ProvenHeads = append(payload.ProvenHeads, runID+"@"+projected.Head())
		}
	}
	if proving == 0 {
		// Nothing upstream has been judged yet. A gate satisfied by an absence
		// would be the fake evidence #64 refuses.
		return payload, false, nil
	}
	return payload, true, nil
}

type humanDecisionReference struct {
	decision        Ref
	humanEvidenceID string
	// claims are what the authority request the person answered was ABOUT.
	claims []string
}

// answersClaims reports whether a human decision answered the claims the gate
// states. A gate that names no claims is answered by the decision itself; a
// gate that names them requires the request to have carried them, because a
// person authorizing a publication has not thereby answered "was this
// independently reviewed".
func answersClaims(decision humanDecisionReference, required []string) bool {
	if len(required) == 0 {
		return true
	}
	// A decision recorded before this evidence carried its claims answers by
	// its existence, as it always did. Refusing it would invalidate approvals
	// an operator already gave against a record that could not have carried
	// what is now asked of it - the same grandfathering a gate without proven
	// heads gets, for the same reason.
	if len(decision.claims) == 0 {
		return true
	}
	for _, claim := range required {
		found := false
		for _, answered := range decision.claims {
			found = found || answered == claim
		}
		if !found {
			return false
		}
	}
	return true
}

// humanDecision reports whether A PERSON decided this gate for this run.
//
// It reads the run's recorded human authority evidence and nothing else. An
// authorized #7 decision is NOT accepted in its place: authority is authorized
// whenever nothing is missing, which ordinary machine evidence can reach on its
// own, so a producing run's auto-authorized publication could discharge the
// very gate that stood in for a blocked independence obligation. A gate that
// says a person decided has to be able to name the person's evidence.
//
// The evidence is the kernel's existing HumanAuthorityRecorded record - pinned
// to a request, a candidate, a contract and a state digest - so the plan
// invents no second approval system.
func humanDecision(events []EngineeringEvent, action *domain.Action, head string) (humanDecisionReference, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		var payload HumanAuthorityRecordedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			// An unreadable decision is not an absent one. Walking past it
			// reaches an OLDER approval of the same head and satisfies the
			// gate - an error answering the question "did a person approve
			// this", which is the class this file swept elsewhere.
			return humanDecisionReference{}, false
		}
		if action != nil && payload.Action != *action {
			continue
		}
		// The decision has to be about THIS candidate. A person approves a
		// change, not a run: carrying an approval of head A onto head B is the
		// rule the run-level authority binding exists to prevent, and reading
		// raw events here bypassed it - the gate re-opened when the head moved
		// and then re-satisfied itself from the same stale approval, stamping
		// it onto work nobody had seen.
		if head != "" && payload.Candidate.Revision != head {
			continue
		}
		// The NEWEST matching decision governs. Skipping a rejection to keep
		// looking would let a gate be satisfied by an approval the person has
		// since reversed, which is the opposite of what a human decision gate
		// is for.
		if payload.Decision != "approve" {
			return humanDecisionReference{}, false
		}
		return humanDecisionReference{
			decision:        Ref{ID: payload.Request.ID, Revision: payload.Request.Revision},
			humanEvidenceID: payload.EvidenceID,
			claims:          payload.Requires,
		}, true
	}
	return humanDecisionReference{}, false
}

func (r PlanReconciler) runProjection(runID string) (RunProjection, error) {
	events, err := r.Store.Events(runID)
	if err != nil {
		return RunProjection{}, err
	}
	return Project(events)
}

// upstreamRuns are the child runs a gate judges: the runs of the agent stages
// it transitively depends on. Transitive, because a gate placed after another
// gate still judges the work, and the work is upstream of both.
func upstreamRuns(stage domain.PlanStage, plan domain.EngineeringPlan, snapshot PlanSnapshot) []string {
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		current, ok := plan.Stage(id)
		if !ok {
			return
		}
		for _, dependency := range current.DependsOn {
			walk(dependency)
		}
	}
	for _, dependency := range stage.DependsOn {
		walk(dependency)
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		if projection, ok := snapshot.Stages[id]; ok && projection.RunID != "" {
			ids = append(ids, projection.RunID)
		}
	}
	sort.Strings(ids)
	return ids
}

// dependenciesSatisfied is the whole of dependency gating: a stage is ready
// when every stage it depends on has COMPLETED (an agent stage) or been
// SATISFIED (a gate). A failed or invalidated dependency blocks with its own
// reason rather than being retried here; retries belong to the scheduler.
func planDependenciesSatisfied(stage domain.PlanStage, snapshot PlanSnapshot) (bool, string) {
	for _, dependency := range stage.DependsOn {
		projection, ok := snapshot.Stages[dependency]
		if !ok {
			return false, "waiting for " + dependency
		}
		switch projection.State {
		case PlanStageCompleted, PlanStageSatisfied:
		case PlanStageFailed:
			return false, "stage " + dependency + " failed"
		case PlanStageInvalidated:
			return false, "stage " + dependency + " was invalidated by a revision"
		default:
			return false, "waiting for " + dependency
		}
	}
	return true, ""
}

// terminalStageState is a stage this plan will not touch again.
//
// INVALIDATED is deliberately not one of them. A stage whose assumptions an
// approved revision invalidated has to be done again under that revision - its
// run identity carries the revision, so redoing it is a new run rather than a
// continuation - and treating it as terminal wedged the plan: the stage never
// re-ran and everything downstream blocked forever on a dependency that could
// not progress.
func terminalStageState(state PlanStageState) bool {
	switch state {
	case PlanStageCompleted, PlanStageFailed, PlanStageSatisfied:
		return true
	default:
		return false
	}
}

// headroom is what the plan has LEFT of each attributable dimension. A zero
// value means the envelope states no ceiling in that dimension, which is not
// the same as no headroom.
type headroom struct{ invocations, wallSeconds int }

// approvedRevisionMoved reports the approved revision when it is no longer the
// one this pass loaded, and zero when it still is.
//
// It exists because one step of a pass gives the plan lock up: a planning
// invocation runs unlocked, for minutes, and an operator decision in that
// window changes which document is the plan. Re-reading the snapshot is not
// enough - the pass holds a decoded plan document, and that is what its
// remaining steps act on.
func (r PlanReconciler) approvedRevisionMoved(plan domain.EngineeringPlan) (int, error) {
	snapshot, err := r.Store.ReplayPlan(plan.ID)
	if err != nil {
		return 0, err
	}
	approved, ok := snapshot.ApprovedRevision()
	if !ok || approved == plan.Revision {
		return 0, nil
	}
	return approved, nil
}

func remainingHeadroom(plan domain.EngineeringPlan, snapshot PlanSnapshot) headroom {
	left := headroom{}
	if ceiling := plan.BudgetEnvelope.MaxProviderInvocations; ceiling > 0 {
		if left.invocations = ceiling - snapshot.Consumed.ProviderInvocations; left.invocations < 0 {
			left.invocations = 0
		}
	}
	if ceiling := plan.BudgetEnvelope.MaxWallSeconds; ceiling > 0 {
		if left.wallSeconds = ceiling - snapshot.Consumed.WallSeconds; left.wallSeconds < 0 {
			left.wallSeconds = 0
		}
	}
	return left
}

// tighten narrows a stage budget to the remainder. It only ever narrows: a
// remainder larger than the stage's own bound changes nothing.
func (h headroom) tighten(budget domain.StageBudget) domain.StageBudget {
	// The plan's remaining invocations are a TOTAL for the run, and they are
	// carried as one. They used to be written into MaxExecutionAttempts, which
	// is the retry allowance of a single execution binding: a run with two
	// invocations left could spend two on its initial binding and then a fresh
	// two on a continuation, because a continuation is a new binding with its
	// own allowance. The aggregate ceiling was enforced per binding, which is
	// not enforcement of an aggregate at all.
	if h.invocations > 0 && (budget.MaxProviderInvocations <= 0 || h.invocations < budget.MaxProviderInvocations) {
		budget.MaxProviderInvocations = h.invocations
	}
	if h.wallSeconds > 0 && (budget.MaxWallSeconds <= 0 || h.wallSeconds < budget.MaxWallSeconds) {
		budget.MaxWallSeconds = h.wallSeconds
	}
	return budget
}

// invocationCeilingReached is the aggregate provider-invocation gate, applied
// before anything that will spend one. The envelope states a total, and a
// ceiling that is only compared after the spending has happened is not a
// ceiling - it is a report.
func invocationCeilingReached(plan domain.EngineeringPlan, snapshot PlanSnapshot, stageID string) *PlanStageBlock {
	if ceiling := plan.BudgetEnvelope.MaxProviderInvocations; ceiling > 0 && snapshot.Consumed.ProviderInvocations >= ceiling {
		return &PlanStageBlock{
			StageID: stageID, Kind: "budget",
			Reason: fmt.Sprintf("the plan allows %d provider invocations and %d have been spent",
				ceiling, snapshot.Consumed.ProviderInvocations),
		}
	}
	// The aggregate ACTIVE wall ceiling, enforced by the same rule: a plan that
	// has spent its execution time starts nothing further. It is attributed
	// when a stage settles, so it bounds the NEXT stage rather than
	// interrupting one - a plan ceiling is not a per-run timeout, which each
	// run already has.
	if ceiling := plan.BudgetEnvelope.MaxWallSeconds; ceiling > 0 && snapshot.Consumed.WallSeconds >= ceiling {
		return &PlanStageBlock{
			StageID: stageID, Kind: "budget",
			Reason: fmt.Sprintf("the plan allows %d active wall seconds and %d have been spent",
				ceiling, snapshot.Consumed.WallSeconds),
		}
	}
	return nil
}

// activeSeconds is one child run's active execution time, in whole seconds.
//
// A run that has ENDED is measured to its terminal event, never to now.
// Measuring a finished run against the current clock charges the plan for every
// hour between the run ending and the tick that settled it - which after a
// restart is the whole downtime, and would spend a plan's wall ceiling on time
// nothing was running.
func (r PlanReconciler) activeSeconds(runID string) (int, error) {
	run, found, err := r.Store.Run(runID)
	if err != nil || !found {
		return 0, err
	}
	events, err := r.Store.Events(runID)
	if err != nil {
		return 0, err
	}
	active := ActiveElapsed(run, events, terminalOrNow(events, r.now()))
	if active <= 0 {
		return 0, nil
	}
	return int(active / time.Second), nil
}

// terminalOrNow is when the run stopped, or now if it has not.
func terminalOrNow(events []EngineeringEvent, now time.Time) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case EventRunCompleted, EventRunFailed, EventRunCancelled:
			return events[i].OccurredAt
		}
	}
	return now
}

func activeStages(snapshot PlanSnapshot) int {
	active := 0
	for _, projection := range snapshot.Stages {
		if projection.State == PlanStageRunning {
			active++
		}
	}
	return active
}

func (r PlanReconciler) appendPlan(planID, eventType string, payload any) error {
	return appendPlanEvent(r.Store, r.now(), planID, eventType, payload)
}

// planningResolution is the small part of a resolution this file consumes. It
// is an interface so the reconciler depends on the ANSWER rather than on the
// resolver's whole shape.
type planningResolution interface {
	Assignment(stageID string) (domain.AgentAssignment, bool)
}

// ---------------------------------------------------------------------------
// Assignment persistence
// ---------------------------------------------------------------------------

// PutPlanAssignment stores the canonical AgentAssignment for one stage of one
// revision.
//
// It is immutable per (plan, revision, stage): the assignment is what the run
// was created under, and a later resolution producing a different worker must
// be a new revision rather than a rewrite of work already in flight.
func (s *SQLiteOperationStore) PutPlanAssignment(planID string, revision, generation int, assignment domain.AgentAssignment) error {
	if _, err := domain.Encode(assignment); err != nil {
		return fmt.Errorf("assignment for stage %q is invalid: %w", assignment.StageID, err)
	}
	document, err := CanonicalJSON(assignment)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`INSERT INTO plan_assignments (plan_id, revision, stage_id, generation, assignment_id, document)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(plan_id, revision, stage_id, generation) DO NOTHING`,
		planID, revision, assignment.StageID, generation, assignment.ID, string(document))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 1 {
		return err
	}
	// A stored assignment stays. Refusing to overwrite it is what makes the
	// freeze real: the run is executing under the configuration this row
	// records, whatever the registry says now.
	return nil
}

// PreviousPlanAssignment returns the assignment the PREVIOUS performance of a
// stage was frozen under: the row with the greatest (revision, generation)
// ordered strictly before the one asked about.
//
// It is not (revision, generation-1). A stage completed under one revision and
// carried unchanged into the next keeps its row under the revision it executed
// under, so the performance before generation N of revision R legitimately
// lives at an earlier revision. That is the case the authority boundary has to
// see: an execution generation renews an obligation, and the obligation it
// renews is whatever was last actually performed.
func (s *SQLiteOperationStore) PreviousPlanAssignment(planID string, revision, generation int, stageID string) (domain.AgentAssignment, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_assignments
		WHERE plan_id = ? AND stage_id = ? AND (revision < ? OR (revision = ? AND generation < ?))
		ORDER BY revision DESC, generation DESC LIMIT 1`,
		planID, stageID, revision, revision, generation).Scan(&document)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentAssignment{}, false, nil
		}
		return domain.AgentAssignment{}, false, err
	}
	assignment, err := domain.Decode[domain.AgentAssignment]([]byte(document))
	if err != nil {
		return domain.AgentAssignment{}, false, fmt.Errorf("decode durable assignment: %w", err)
	}
	return assignment, true, nil
}

// PlanAssignment returns the frozen assignment one run is executing under.
func (s *SQLiteOperationStore) PlanAssignment(planID string, revision, generation int, stageID string) (domain.AgentAssignment, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_assignments WHERE plan_id = ? AND revision = ? AND stage_id = ? AND generation = ?`,
		planID, revision, stageID, generation).Scan(&document)
	if err != nil {
		// The typed sentinel, not the driver's message. A message check is a
		// dependency on prose that no compiler enforces and no driver promises.
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AgentAssignment{}, false, nil
		}
		return domain.AgentAssignment{}, false, err
	}
	// Decoded through the SCHEMA, like every other durable artifact this
	// package reads back. A frozen assignment drives the agent binding, the
	// instruction packs and the upstream context, so a row that no longer
	// satisfies its own schema - corruption, or a schema tightened after it was
	// written - is refused rather than used.
	assignment, err := domain.Decode[domain.AgentAssignment]([]byte(document))
	if err != nil {
		return domain.AgentAssignment{}, false, fmt.Errorf("decode durable assignment: %w", err)
	}
	return assignment, true, nil
}
