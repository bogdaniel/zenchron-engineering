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
	"context"
	"encoding/json"
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
	previousRevision := plan.Provenance.PreviousRevision
	if previousRevision == nil || *previousRevision >= plan.Revision {
		return snapshot, nil
	}
	for _, recorded := range snapshot.Superseded {
		if recorded.ToRevision == plan.Revision {
			return snapshot, nil
		}
	}
	previous, found, err := r.Store.PlanRevision(planID, *previousRevision)
	if err != nil {
		return snapshot, err
	}
	if !found {
		return snapshot, nil
	}
	if err := r.appendPlan(planID, EventPlanRevisionSuperseded, PlanRevisionSupersededPayload{
		FromRevision: *previousRevision, ToRevision: plan.Revision,
		ProposalID:        plan.Provenance.ProposalID,
		InvalidatedStages: InvalidatedStages(previous, plan, snapshot),
	}); err != nil {
		return snapshot, err
	}
	return r.Store.ReplayPlan(planID)
}

// startAgentStage creates or associates the ordinary EngineeringRun for one
// dependency-ready agent stage, within the plan's aggregate envelope.
func (r PlanReconciler) startAgentStage(ctx context.Context, plan domain.EngineeringPlan, stage domain.PlanStage, snapshot PlanSnapshot, resolution planningResolution) (*PlanStageRun, *PlanStageBlock, error) {
	assignment, ok := resolution.Assignment(stage.ID)
	if !ok {
		// The resolver already recorded why, and that block is in the report.
		return nil, nil, nil
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
	if err := r.Store.PutPlanAssignment(plan.ID, plan.Revision, assignment); err != nil {
		return nil, nil, err
	}
	// The STORED assignment is the one that runs. A row already frozen for this
	// (plan, revision, stage) is kept, so a re-resolution that picked a
	// different worker must not be what the journal, the run binding and the
	// report describe: that is a tamper-evident journal naming worker B while
	// worker A does the work.
	frozen, found, err := r.Store.PlanAssignment(plan.ID, plan.Revision, stage.ID)
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
	binding := RunPlanBinding{
		PlanID: plan.ID, Revision: plan.Revision, PlanDigest: plan.Digest,
		StageID: stage.ID, AssignmentID: assignment.ID,
		BaseRevision: r.upstreamBase(assignment),
		// The assignment's budget is the stage's, already narrowed by the
		// assigned profile's constraints. The run is created bounded by it.
		StageBudget: assignment.Budget,
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
func (r PlanReconciler) upstreamBase(assignment domain.AgentAssignment) string {
	base := ""
	for _, upstream := range assignment.Context.UpstreamOutputs {
		if upstream.RunID == "" || upstream.Candidate == "" {
			continue
		}
		events, err := r.Store.Events(upstream.RunID)
		if err != nil {
			continue
		}
		projection, err := Project(events)
		if err != nil || projection.PullRequest == nil {
			// Not published. Its commit is not on the remote, so it cannot be
			// cloned; the stage stays based on the trusted base and receives
			// the diff as context.
			continue
		}
		base = upstream.Candidate
	}
	return base
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
		// What the run actually spent, read from ITS journal rather than
		// assumed: provider invocations are attempts of execution operations,
		// which is a fact the durable operations already carry.
		invocations, err := r.providerInvocations(projection.RunID)
		if err != nil {
			return settled, err
		}
		if invocations > 0 {
			// Keyed by the run: settling is retried after a crash between this
			// append and the settled event, and the invocations of one run are
			// one fact however many times that retry happens.
			if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
				Key: "invocations:" + projection.RunID, StageID: stage.ID, RunID: projection.RunID,
				ProviderInvocations: invocations,
			}); err != nil {
				return settled, err
			}
		}
		// ACTIVE wall time, by the same definition the run's own wall budget
		// uses: elapsed less what the run spent waiting on something external.
		// A plan whose stages wait days for a reviewer has not spent days of
		// execution, and a ceiling that counted them would stop work nobody
		// was doing.
		active, err := r.activeSeconds(projection.RunID)
		if err != nil {
			return settled, err
		}
		if active > 0 {
			if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
				Key: "wall:" + projection.RunID, StageID: stage.ID, RunID: projection.RunID,
				WallSeconds: active,
			}); err != nil {
				return settled, err
			}
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
		case domain.StageHumanDecisionGate:
			decision, satisfied := humanDecision(events, stage.Action)
			if !satisfied {
				continue
			}
			proving++
			payload.Decision = decision.decision
			payload.HumanEvidenceID = decision.humanEvidenceID
			payload.ProvingRuns = append(payload.ProvingRuns, runID)
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
func humanDecision(events []EngineeringEvent, action *domain.Action) (humanDecisionReference, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		var payload HumanAuthorityRecordedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if action != nil && payload.Action != *action {
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
func (s *SQLiteOperationStore) PutPlanAssignment(planID string, revision int, assignment domain.AgentAssignment) error {
	if _, err := domain.Encode(assignment); err != nil {
		return fmt.Errorf("assignment for stage %q is invalid: %w", assignment.StageID, err)
	}
	document, err := CanonicalJSON(assignment)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`INSERT INTO plan_assignments (plan_id, revision, stage_id, assignment_id, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(plan_id, revision, stage_id) DO NOTHING`,
		planID, revision, assignment.StageID, assignment.ID, string(document))
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

// PlanAssignment returns the frozen assignment one run is executing under.
func (s *SQLiteOperationStore) PlanAssignment(planID string, revision int, stageID string) (domain.AgentAssignment, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_assignments WHERE plan_id = ? AND revision = ? AND stage_id = ?`,
		planID, revision, stageID).Scan(&document)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
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
