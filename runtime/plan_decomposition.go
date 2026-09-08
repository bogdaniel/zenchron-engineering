package runtime

// Decomposition: a planner-role stage of an APPROVED plan reasoning about what
// the plan should become.
//
// The frozen law it implements:
//
//	current approved revision
//	  -> planner-role decomposition stage (non-mutating)
//	  -> PlanRevisionProposal
//	  -> deterministic validation
//	  -> normal operator approval boundary
//	  -> new immutable approved revision
//
// Two things it deliberately does not do. It does not create a nested plan:
// there is no sub-plan, no sub-scheduler, and a plan cannot contain a plan. And
// it does not apply anything: a material proposal blocks the stages it would
// change until an operator approves it, which is what "no hidden autonomous
// replacement of the approved plan" means in running code.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// decomposeStage performs one planner-role stage.
//
// It is the ONE agent stage that does not become an EngineeringRun. A run
// exists to produce a candidate change through the governed candidate
// lifecycle; a planner produces a proposal and must not be able to write at
// all, so it runs through the same verified non-mutating boundary the initial
// planner uses and its output crosses the approval boundary instead of the
// candidate one.
func (r PlanReconciler) decomposeStage(ctx context.Context, plan domain.EngineeringPlan, stage domain.PlanStage, assignment domain.AgentAssignment) (*PlanStageBlock, error) {
	if r.Planner == nil {
		return &PlanStageBlock{
			StageID: stage.ID, Kind: "planner",
			Reason: "no planning invoker is configured, so a decomposition stage cannot run",
		}, nil
	}
	contract, found, err := r.Store.PlanContract(plan.ID, plan.Revision)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &PlanRefusedError{PlanID: plan.ID, Detail: "no contract is stored for the approved revision"}
	}
	// The ceiling is checked BEFORE the provider runs. A planner whose answer
	// never parses is re-entered on every tick, and without this the plan spent
	// a real invocation each time against a ceiling nothing consulted.
	snapshot, err := r.Store.ReplayPlan(plan.ID)
	if err != nil {
		return nil, err
	}
	if block := invocationCeilingReached(plan, snapshot, stage.ID); block != nil {
		return block, nil
	}
	output, plannerErr := r.Planner(ctx, PlanDecompositionRequest{
		Plan: plan, Stage: stage, Assignment: assignment, Contract: contract,
	})
	// One provider invocation was spent whether or not the proposal is
	// accepted, AND whether or not the invocation failed - the provider ran.
	// Recording it only on success made every failure invisible to the
	// aggregate.
	if err := r.appendPlan(plan.ID, EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		StageID: stage.ID, ProviderInvocations: 1,
	}); err != nil {
		return nil, err
	}
	if plannerErr != nil {
		// A refused planning invocation is a BLOCK on that stage, not a failed
		// plan: the reason is typed, the operator can act on it, and the
		// approved revision keeps executing whatever it can.
		return &PlanStageBlock{StageID: stage.ID, Kind: "planner", Reason: boundedDetail(plannerErr.Error())}, nil
	}
	proposal, err := r.recordProposal(plan, stage, contract, output)
	if err != nil {
		return &PlanStageBlock{StageID: stage.ID, Kind: "planner", Reason: boundedDetail(err.Error())}, nil
	}
	if err := r.appendPlan(plan.ID, EventPlanStageSettled, PlanStageSettledPayload{
		StageID: stage.ID, Outcome: "completed",
		Reason: fmt.Sprintf("emitted plan revision proposal %s", proposal.ID),
	}); err != nil {
		return nil, err
	}
	return nil, nil
}

// PlanDecompositionRequest is what a decomposition invocation needs. It is a
// seam rather than a direct call so the reconciler stays free of provider
// construction: the composition root supplies the invoker, exactly as it
// supplies the engine factory.
type PlanDecompositionRequest struct {
	Plan       domain.EngineeringPlan
	Stage      domain.PlanStage
	Assignment domain.AgentAssignment
	Contract   domain.EngineeringWorkContract
}

// recordProposal compiles, validates and stores the proposed revision.
//
// The proposal is durable even when validation REFUSES it. A refusal is the
// answer to "what did the planner ask for and why can it not happen", and
// discarding it would leave an operator with a stage that completed and nothing
// to read.
func (r PlanReconciler) recordProposal(plan domain.EngineeringPlan, stage domain.PlanStage, contract domain.EngineeringWorkContract, output PlannerOutput) (domain.PlanRevisionProposal, error) {
	snapshot, err := r.Store.ReplayPlan(plan.ID)
	if err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	next := plan.Revision + 1
	proposalID := fmt.Sprintf("proposal-%s-r%d", plan.ID, next)
	reasoning := output.Reasoning

	// The ProjectModel identity comes from the revision being revised. A
	// decomposition reasons about the SAME project state the approved plan was
	// compiled against; re-observing it here would let a revision silently move
	// onto a different model.
	model := domain.ProjectModel{
		SchemaVersion: domain.SchemaVersion,
		ID:            plan.Provenance.ProjectModel.ID,
		Revision:      plan.Provenance.ProjectModel.Revision,
		Subject:       plan.Subject,
	}
	proposed, compileErr := planning.Compile(planning.CompileInput{
		PlanID: plan.ID, Revision: next, Objective: plan.Objective, Subject: plan.Subject,
		Contract: contract, Model: model, Envelope: plan.BudgetEnvelope, Proposed: output.Stages,
		Reasoning: &reasoning, Previous: &plan, ProposalID: proposalID,
	})
	proposal := domain.PlanRevisionProposal{
		SchemaVersion: domain.SchemaVersion,
		ID:            proposalID,
		Source:        domain.PlanRef{ID: plan.ID, Revision: plan.Revision, Digest: plan.Digest},
		Reason:        proposalReason(output.Notes),
		Provenance: domain.ProposalProvenance{
			Origin: domain.ProposalOriginDecomposition, StageID: stage.ID, Reasoning: &reasoning,
		},
		Budget: domain.BudgetDelta{
			Current: plan.BudgetEnvelope, Proposed: plan.BudgetEnvelope, Consumed: snapshot.Consumed,
		},
		Validation: domain.ProposalValidation{Status: domain.ProposalValid},
		Approval:   domain.ProposalApproval{Status: domain.ApprovalPending},
	}
	if compileErr != nil {
		proposal.Proposed = refusedRevisionPlaceholder(plan, next, proposalID)
		proposal.Validation = domain.ProposalValidation{
			Status: domain.ProposalRefused, Errors: refusalReasons(compileErr),
		}
		proposal.Changes = domain.PlanChangeSummary{Material: false}
		if err := r.storeProposal(plan, proposal); err != nil {
			return domain.PlanRevisionProposal{}, err
		}
		if err := r.appendPlan(plan.ID, EventPlanValidated, PlanValidatedPayload{
			Revision: next, Status: string(domain.ProposalRefused), Errors: boundedReasons(refusalReasons(compileErr)),
		}); err != nil {
			return domain.PlanRevisionProposal{}, err
		}
		return proposal, nil
	}

	proposal.Proposed = proposed
	proposal.Budget.Proposed = proposed.BudgetEnvelope
	proposal.Budget.Widened = widensEnvelope(plan.BudgetEnvelope, proposed.BudgetEnvelope)
	proposal.Changes = changeSummary(plan, proposed, snapshot)
	if err := r.storeProposal(plan, proposal); err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	// The proposed revision is stored as an UNAPPROVED revision, which is what
	// makes it readable, diffable and approvable through exactly the same path
	// an operator edit takes. Storing it grants nothing: the reconciler
	// executes the approved revision, and this one is not it.
	if _, err := r.Store.PutPlanRevision(proposed); err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	if err := r.Store.PutPlanContract(plan.ID, proposed.Revision, contract); err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	objective, err := Digest(proposed.Objective)
	if err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	if err := r.appendPlan(plan.ID, EventPlanProposed, PlanProposedPayload{
		Revision: proposed.Revision, Digest: proposed.Digest, ObjectiveDigest: objective,
		StageCount: len(proposed.Stages), AgentStageCount: agentStageCount(proposed),
		Budget: budgetPayload(proposed.BudgetEnvelope), Reasoning: reasoningPayload(reasoning),
		ProposalID: proposal.ID, Origin: domain.ProposalOriginDecomposition,
	}); err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	if err := r.appendPlan(plan.ID, EventPlanValidated, PlanValidatedPayload{
		Revision: proposed.Revision, Digest: proposed.Digest, Status: string(domain.ProposalValid),
	}); err != nil {
		return domain.PlanRevisionProposal{}, err
	}
	return proposal, nil
}

// refusedRevisionPlaceholder is the minimum schema-valid document a refused
// proposal carries. A refusal still has to say what plan and revision it was
// about; it must not pretend to carry a plan that never compiled.
func refusedRevisionPlaceholder(plan domain.EngineeringPlan, revision int, proposalID string) domain.EngineeringPlan {
	placeholder := plan
	placeholder.Revision = revision
	previous := plan.Revision
	placeholder.Provenance.PreviousRevision = &previous
	placeholder.Provenance.ProposalID = proposalID
	if digest, err := placeholder.ContentDigest(); err == nil {
		placeholder.Digest = digest
	}
	return placeholder
}

func proposalReason(notes string) string {
	if notes == "" {
		return "a planner-role stage proposed a revised decomposition"
	}
	return boundedDetail(notes)
}

func refusalReasons(err error) []string {
	var invalid *planning.ValidationError
	if errors.As(err, &invalid) {
		return invalid.Reasons
	}
	return []string{boundedDetail(err.Error())}
}

// widensEnvelope reports whether a proposal asks for MORE than the current
// ceiling. Widening requires the same explicit operator authority that could
// have granted the ceiling originally, so it is stated on the proposal rather
// than discovered at approval time.
func widensEnvelope(current, proposed domain.PlanBudgetEnvelope) bool {
	if proposed.MaxChildRuns > current.MaxChildRuns ||
		proposed.MaxConcurrency > current.MaxConcurrency ||
		proposed.MaxProviderInvocations > current.MaxProviderInvocations ||
		proposed.MaxWallSeconds > current.MaxWallSeconds {
		return true
	}
	if proposed.MaxCostMicros == nil {
		return false
	}
	return current.MaxCostMicros == nil || *proposed.MaxCostMicros > *current.MaxCostMicros
}

// changeSummary is the approval-visible difference. Material is what decides
// whether affected execution pauses: a change to stages, dependencies,
// assignments, budgets, trust or independence is approval-visible, and a
// proposal that changes none of those is not.
func changeSummary(current, proposed domain.EngineeringPlan, snapshot PlanSnapshot) domain.PlanChangeSummary {
	summary := domain.PlanChangeSummary{}
	for _, stage := range proposed.Stages {
		before, existed := current.Stage(stage.ID)
		switch {
		case !existed:
			summary.AddedStages = append(summary.AddedStages, stage.ID)
		case !sameStageContent(before, stage):
			summary.ChangedStages = append(summary.ChangedStages, stage.ID)
		}
	}
	for _, stage := range current.Stages {
		if _, still := proposed.Stage(stage.ID); !still {
			summary.RemovedStages = append(summary.RemovedStages, stage.ID)
		}
	}
	summary.InvalidatedStages = InvalidatedStages(current, proposed, snapshot)
	summary.Material = len(summary.AddedStages) > 0 || len(summary.RemovedStages) > 0 ||
		len(summary.ChangedStages) > 0 || !sameEnvelope(current.BudgetEnvelope, proposed.BudgetEnvelope)
	return summary
}

// sameEnvelope compares envelopes by VALUE. A struct comparison looks right
// and is not: PlanBudgetEnvelope carries MaxCostMicros as a pointer, so two
// envelopes holding the same ceiling would report a budget change that never
// happened - pausing the plan for an operator decision about nothing - and two
// aliased pointers holding different ceilings would report no change at all.
// Which one you got depended on whether the documents had been round-tripped.
func sameEnvelope(left, right domain.PlanBudgetEnvelope) bool {
	if left.MaxChildRuns != right.MaxChildRuns || left.MaxConcurrency != right.MaxConcurrency ||
		left.MaxProviderInvocations != right.MaxProviderInvocations || left.MaxWallSeconds != right.MaxWallSeconds {
		return false
	}
	switch {
	case left.MaxCostMicros == nil && right.MaxCostMicros == nil:
		return true
	case left.MaxCostMicros == nil || right.MaxCostMicros == nil:
		return false
	default:
		return *left.MaxCostMicros == *right.MaxCostMicros
	}
}

// ---------------------------------------------------------------------------
// Proposal persistence
// ---------------------------------------------------------------------------

func (r PlanReconciler) storeProposal(plan domain.EngineeringPlan, proposal domain.PlanRevisionProposal) error {
	return r.Store.PutPlanProposal(plan.ID, proposal)
}

// PutPlanProposal stores one PlanRevisionProposal. It is validated against its
// schema on the way in - including the plan it proposes, through the plan
// schema - so nothing unvalidated becomes an approvable artifact.
func (s *SQLiteOperationStore) PutPlanProposal(planID string, proposal domain.PlanRevisionProposal) error {
	if _, err := domain.Encode(proposal.Proposed); err != nil {
		return fmt.Errorf("proposed revision is not a valid plan: %w", err)
	}
	if _, err := domain.Encode(proposal); err != nil {
		return fmt.Errorf("proposal %q is invalid: %w", proposal.ID, err)
	}
	document, err := CanonicalJSON(proposal)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO plan_proposals (id, plan_id, from_revision, to_revision, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		proposal.ID, planID, proposal.Source.Revision, proposal.Proposed.Revision, string(document))
	return err
}

// PlanProposals lists a plan's proposals, oldest first.
func (s *SQLiteOperationStore) PlanProposals(planID string) ([]domain.PlanRevisionProposal, error) {
	rows, err := s.db.Query(`SELECT document FROM plan_proposals WHERE plan_id = ? ORDER BY to_revision ASC, id ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proposals []domain.PlanRevisionProposal
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		proposal, err := domain.Decode[domain.PlanRevisionProposal]([]byte(document))
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, rows.Err()
}

// PlanProposal returns one proposal by id.
func (s *SQLiteOperationStore) PlanProposal(id string) (domain.PlanRevisionProposal, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_proposals WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PlanRevisionProposal{}, false, nil
	}
	if err != nil {
		return domain.PlanRevisionProposal{}, false, err
	}
	proposal, err := domain.Decode[domain.PlanRevisionProposal]([]byte(document))
	return proposal, err == nil, err
}

// PendingMaterialProposal is the proposal, if any, that a plan is waiting on.
//
// It is what makes "affected execution pauses until approval" checkable: a
// material proposal against the APPROVED revision means the plan an operator
// approved is no longer the plan the planner believes in, and starting new work
// under the old one would be executing a decomposition nobody accepted.
func (s *SQLiteOperationStore) PendingMaterialProposal(planID string, approvedRevision int) (domain.PlanRevisionProposal, bool, error) {
	proposals, err := s.PlanProposals(planID)
	if err != nil {
		return domain.PlanRevisionProposal{}, false, err
	}
	for _, proposal := range proposals {
		if proposal.Source.Revision != approvedRevision {
			continue
		}
		if proposal.Validation.Status != domain.ProposalValid || !proposal.Changes.Material {
			continue
		}
		if proposal.Approval.Status != domain.ApprovalPending {
			continue
		}
		// A proposal whose revision has SINCE been approved is no longer
		// pending: the approval is recorded in the journal, not on the stored
		// document, so it is the journal that answers.
		snapshot, err := s.ReplayPlan(planID)
		if err != nil {
			return domain.PlanRevisionProposal{}, false, err
		}
		if approved, ok := snapshot.ApprovedRevision(); ok && approved >= proposal.Proposed.Revision {
			continue
		}
		// And a proposal an operator REJECTED is answered too. "Keep the
		// current plan" is the ordinary answer to a decomposition, and reading
		// only approvals left the plan paused on a question that had been
		// decided - blocking every new stage forever.
		if snapshot.Rejected[proposal.Proposed.Revision] {
			continue
		}
		return proposal, true, nil
	}
	return domain.PlanRevisionProposal{}, false, nil
}
