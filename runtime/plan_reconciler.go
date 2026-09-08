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
	if active := activeStages(snapshot); active >= plan.BudgetEnvelope.MaxConcurrency {
		return nil, &PlanStageBlock{
			StageID: stage.ID, Kind: "concurrency",
			Reason: fmt.Sprintf("the plan allows %d concurrent stages and %d are running", plan.BudgetEnvelope.MaxConcurrency, active),
		}, nil
	}
	if r.Engine == nil {
		return nil, nil, &PlanRefusedError{PlanID: plan.ID, Detail: "no engine factory is configured, so no run can be created"}
	}
	engine, err := r.Engine(r.Repository, assignment.Agent.ID)
	if err != nil {
		return nil, &PlanStageBlock{StageID: stage.ID, Kind: "agent", Reason: boundedDetail(err.Error())}, nil
	}
	if err := r.Store.PutPlanAssignment(plan.ID, plan.Revision, assignment); err != nil {
		return nil, nil, err
	}
	binding := RunPlanBinding{
		PlanID: plan.ID, Revision: plan.Revision, PlanDigest: plan.Digest,
		StageID: stage.ID, AssignmentID: assignment.ID,
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
	if err := r.appendPlan(plan.ID, EventPlanRunStarted, PlanRunStartedPayload{StageID: stage.ID, RunID: outcome.RunID}); err != nil {
		return nil, nil, err
	}
	// One child run consumed. Consumption is a delta against an immutable
	// record, so nothing later - a revision, a restart, a reassignment - can
	// take it back.
	if !outcome.Adopted {
		if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
			StageID: stage.ID, RunID: outcome.RunID, ChildRuns: 1,
		}); err != nil {
			return nil, nil, err
		}
	}
	return &PlanStageRun{StageID: stage.ID, RunID: outcome.RunID, AgentID: assignment.Agent.ID}, nil, nil
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
		if !found || !terminalDisposition(run.Disposition) {
			continue
		}
		outcome := "completed"
		if run.Disposition != Completed {
			outcome = "failed"
		}
		// What the run actually spent, read from ITS journal rather than
		// assumed: provider invocations are attempts of execution operations,
		// which is a fact the durable operations already carry.
		invocations, err := r.providerInvocations(projection.RunID)
		if err != nil {
			return settled, err
		}
		if invocations > 0 {
			if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
				StageID: stage.ID, RunID: projection.RunID, ProviderInvocations: invocations,
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
	for _, runID := range runs {
		projected, err := r.runProjection(runID)
		if err != nil {
			return payload, false, err
		}
		switch stage.Kind {
		case domain.StageAssuranceGate:
			if projected.Assurance == nil || projected.Assurance.Stale || !projected.Assurance.Passed {
				return payload, false, nil
			}
			// The independent semantic verdict, where one exists, has to agree.
			// Letting an automated test pass stand in for it would be exactly
			// the aliasing the evidence model forbids.
			if projected.SemanticAssurance != nil && (!projected.SemanticAssurance.Passed || projected.SemanticAssurance.Stale) {
				return payload, false, nil
			}
			payload.Evidence = projected.Assurance.Bundle
		case domain.StageHumanDecisionGate:
			decision, satisfied := humanDecision(projected, stage.Action)
			if !satisfied {
				return payload, false, nil
			}
			payload.Decision = decision.decision
			payload.HumanEvidenceID = decision.humanEvidenceID
		}
	}
	return payload, true, nil
}

type humanDecisionReference struct {
	decision        Ref
	humanEvidenceID string
}

// humanDecision reports whether a person decided the gate's action for this run.
//
// It accepts either an authorized #7 decision for the exact action - which
// already required the human evidence a policy asked for - or a recorded human
// authority answer of "approve" for it. Both are existing durable records; the
// plan invents no second approval system.
func humanDecision(projected RunProjection, action *domain.Action) (humanDecisionReference, bool) {
	for _, evaluation := range projected.AuthorityDecisions {
		if action != nil && evaluation.Action != *action {
			continue
		}
		if evaluation.Status == domain.AuthorityAuthorized {
			return humanDecisionReference{decision: evaluation.Decision}, true
		}
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

func terminalStageState(state PlanStageState) bool {
	switch state {
	case PlanStageCompleted, PlanStageFailed, PlanStageInvalidated, PlanStageSatisfied:
		return true
	default:
		return false
	}
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
	var assignment domain.AgentAssignment
	if err := json.Unmarshal([]byte(document), &assignment); err != nil {
		return domain.AgentAssignment{}, false, fmt.Errorf("decode durable assignment: %w", err)
	}
	return assignment, true, nil
}
