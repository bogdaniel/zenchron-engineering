package runtime

// The plan lifecycle: propose, validate, approve, revise.
//
// This file is the OPERATOR boundary of #64. It owns nothing about scheduling
// and nothing about execution: it compiles a plan, records the deterministic
// verdict on it, and waits. A proposed plan does not execute because it parsed,
// and there is no automatic approval anywhere in this milestone.
//
// Everything durable here goes through the existing store and the existing
// append-only journal. The plan reconciler reads what this file wrote; the #63
// scheduler and leases remain the only thing that decides when work runs.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// PlanRefusedError is the typed refusal for a plan lifecycle request that the
// durable state does not permit.
type PlanRefusedError struct {
	PlanID string
	Detail string
}

func (e *PlanRefusedError) Error() string {
	if e.PlanID == "" {
		return "plan request refused: " + e.Detail
	}
	return "plan " + e.PlanID + ": " + e.Detail
}

// PlanService is the lifecycle owner for one operator state directory.
//
// It is deliberately small and dependency-explicit: a store, a clock, the
// operator's customization registry, and the workforce as descriptors. It
// constructs no provider and contacts nothing.
type PlanService struct {
	Store    *SQLiteOperationStore
	Clock    Clock
	Registry planning.Registry
	// Agents is the registered workforce, projected for the planner.
	Agents []domain.ExecutionAgentDescriptor
	// DefaultAgent breaks resolution ties. It is a preference, never an
	// eligibility rule.
	DefaultAgent string
	// Envelope is the OPERATOR's aggregate ceiling. A plan may be tighter and
	// can never be wider, and only this path can raise it.
	Envelope domain.PlanBudgetEnvelope
}

func (s PlanService) now() time.Time {
	if s.Clock == nil {
		return RealClock{}.Now()
	}
	return s.Clock.Now()
}

// ProposeInput is one plan proposal.
type ProposeInput struct {
	PlanID     string
	Objective  string
	Subject    domain.Subject
	Repository string
	Contract   domain.EngineeringWorkContract
	Model      domain.ProjectModel
	Facts      []domain.EngineeringFact
	// Issue is the source the plan answers. Every stage run it creates answers
	// the same source; the stage objective is what differs.
	Issue int
	// Template is the operator's chosen reusable process, or empty.
	Template string
	// Reasoned is a reasoning agent's proposed stages, and Reasoning its
	// provenance. Both are absent for a purely deterministic compilation, which
	// is a first-class mode rather than a fallback: the deterministic skeleton
	// exists so policy and validation are testable without a live model.
	Reasoned  []domain.PlanStage
	Reasoning *domain.PlanReasoningProvenance
	// Origin states where the proposal came from, for the durable record.
	Origin string
}

// Propose compiles, validates and records a plan revision awaiting approval.
//
// A REFUSED plan is recorded too. Refusing quietly would lose the reason a
// reasoning agent asked for something it may not have, and that reason is
// exactly what an operator needs to decide what to do next.
func (s PlanService) Propose(ctx context.Context, input ProposeInput) (domain.EngineeringPlan, error) {
	if s.Store == nil {
		return domain.EngineeringPlan{}, &PlanRefusedError{Detail: "a durable store is required"}
	}
	existing, found, err := s.Store.Plan(input.PlanID)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	revision := 1
	var previous *domain.EngineeringPlan
	if found {
		revision = existing.Revision + 1
		previous = &existing
	}
	var template *domain.EngineeringPlanTemplate
	if strings.TrimSpace(input.Template) != "" {
		resolved, err := s.Registry.Template(input.Template)
		if err != nil {
			return domain.EngineeringPlan{}, err
		}
		template = &resolved
	}
	origin := input.Origin
	if origin == "" {
		origin = domain.ProposalOriginInitial
		if found {
			origin = domain.ProposalOriginOperatorEdit
		}
	}

	// What this plan has already spent. A revision that tightens the envelope
	// below it is refused at compile time rather than approved and then blocked
	// at the first stage that tries to run under it.
	var consumed domain.PlanConsumption
	if found {
		snapshot, err := s.Store.ReplayPlan(input.PlanID)
		if err != nil {
			return domain.EngineeringPlan{}, err
		}
		consumed = snapshot.Consumed
	}
	plan, compileErr := planning.Compile(planning.CompileInput{
		PlanID: input.PlanID, Revision: revision, Objective: input.Objective,
		Subject: input.Subject, Contract: input.Contract, Model: input.Model, Facts: input.Facts,
		Template: template, Envelope: s.Envelope, Proposed: input.Reasoned,
		Reasoning: input.Reasoning, Previous: previous, Consumed: consumed,
	})
	if compileErr != nil {
		// The refusal is durable when a plan already exists to record it
		// against. For a first proposal there is no plan row yet, and creating
		// one to hold a refusal would create a plan that never existed.
		if found {
			if err := s.recordValidation(input.PlanID, revision, "", domain.ProposalRefused, compileErr); err != nil {
				return domain.EngineeringPlan{}, err
			}
		}
		return domain.EngineeringPlan{}, compileErr
	}

	claimedNow := false
	if !found {
		claimed, err := s.Store.ClaimPlan(plan, s.now())
		if err != nil {
			return domain.EngineeringPlan{}, err
		}
		claimedNow = claimed
		// The claim is a conditional insert, so `false` means another proposer
		// created this plan between the read above and here. Continuing would
		// append a second proposed event for a revision that already exists -
		// and, when the two proposals differ, would write a contract and a
		// source binding the winner never agreed to. The loser stops.
		if !claimed {
			return domain.EngineeringPlan{}, &PlanRefusedError{
				PlanID: plan.ID,
				Detail: "the plan was created concurrently by another proposal; read it and propose again from what is now stored",
			}
		}
	}
	// ClaimPlan writes the first revision with the plan row, in one
	// transaction, so a first proposal's PutPlanRevision is a no-op by
	// construction: the claim IS the creation.
	created, err := s.Store.PutPlanRevision(plan)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	created = created || claimedNow
	objectiveDigest, err := Digest(plan.Objective)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	payload := PlanProposedPayload{
		Revision: plan.Revision, Digest: plan.Digest, ObjectiveDigest: objectiveDigest,
		StageCount: len(plan.Stages), AgentStageCount: agentStageCount(plan),
		Budget: budgetPayload(plan.BudgetEnvelope), Origin: origin,
		ProposalID: plan.Provenance.ProposalID,
	}
	if plan.Provenance.Template != nil {
		payload.Template = &PlanTemplateRef{
			ID: plan.Provenance.Template.ID, Version: plan.Provenance.Template.Version, Digest: plan.Provenance.Template.Digest,
		}
	}
	if plan.Provenance.Reasoning != nil {
		payload.Reasoning = reasoningPayload(*plan.Provenance.Reasoning)
	}
	// Only the caller that CREATED the revision announces it. A concurrent
	// proposer that computed the same next revision from the same stored state
	// wrote nothing, and announcing it anyway proposed one revision twice. The
	// writes below are idempotent, so a retry that crashed after storing the
	// revision still completes them.
	if created {
		if err := s.appendPlanEvent(plan.ID, EventPlanProposed, payload); err != nil {
			return domain.EngineeringPlan{}, err
		}
	}
	if err := s.Store.PutPlanContract(plan.ID, plan.Revision, input.Contract); err != nil {
		return domain.EngineeringPlan{}, err
	}
	if input.Issue > 0 {
		if err := s.Store.BindPlanSource(plan.ID, input.Issue); err != nil {
			return domain.EngineeringPlan{}, err
		}
	}
	if err := s.recordValidation(plan.ID, plan.Revision, plan.Digest, domain.ProposalValid, nil); err != nil {
		return domain.EngineeringPlan{}, err
	}
	return plan, nil
}

// Approve records the operator's decision on ONE exact revision.
//
// The digest is checked, not merely the number: approving a revision number
// whose content could since have changed would be approving something nobody
// looked at.
func (s PlanService) Approve(planID string, revision int, digest, operator, note string) (PlanSnapshot, error) {
	return s.decide(planID, revision, digest, operator, note, EventPlanApproved)
}

// Reject records a refusal. It is durable for the same reason an approval is:
// "we looked at this and said no" is a fact about the work.
func (s PlanService) Reject(planID string, revision int, digest, operator, note string) (PlanSnapshot, error) {
	return s.decide(planID, revision, digest, operator, note, EventPlanRejected)
}

func (s PlanService) decide(planID string, revision int, digest, operator, note, eventType string) (PlanSnapshot, error) {
	plan, found, err := s.Store.PlanRevision(planID, revision)
	if err != nil {
		return PlanSnapshot{}, err
	}
	if !found {
		return PlanSnapshot{}, &PlanRefusedError{PlanID: planID, Detail: fmt.Sprintf("revision %d does not exist", revision)}
	}
	// A decision NAMES the content it decides. The digest is required here, at
	// the service boundary, rather than only in the CLI: the invariant belongs
	// where every caller passes through, and the control endpoint is a second
	// caller. Deciding by revision number alone is deciding something nobody
	// has necessarily read.
	if strings.TrimSpace(digest) == "" {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: fmt.Sprintf("a decision on revision %d must name its digest: it is what binds the decision to the content that was read", revision),
		}
	}
	if digest != plan.Digest {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: fmt.Sprintf("revision %d now digests to %s and the decision names %s: approve what you read, or read it again", revision, short12(plan.Digest), short12(digest)),
		}
	}
	snapshot, err := s.Store.ReplayPlan(planID)
	if err != nil {
		return PlanSnapshot{}, err
	}
	// The verdict for THIS revision, not the latest verdict recorded. A single
	// slot meant a later revision's validation replaced an earlier refusal, so
	// the refused revision would pass this check by having been superseded in a
	// field rather than by having been fixed.
	if verdict, ok := snapshot.Validations[revision]; ok && verdict.Status == domain.ProposalRefused {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: fmt.Sprintf("revision %d failed deterministic validation and cannot be approved: %s", revision, strings.Join(verdict.Errors, "; ")),
		}
	}
	if strings.TrimSpace(operator) == "" {
		return PlanSnapshot{}, &PlanRefusedError{PlanID: planID, Detail: "an approval records who made it"}
	}
	// Approval moves FORWARD. Re-approving a revision older than the one
	// already governing would leave the durable approval record describing
	// something other than the work being executed, and the supersession below
	// would then measure the next revision against the wrong predecessor.
	if governing, ok := snapshot.ApprovedRevision(); ok && revision < governing {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: fmt.Sprintf("revision %d is older than the approved revision %d: a plan is not un-revised by approving what it superseded", revision, governing),
		}
	}
	if err := s.appendPlanEvent(planID, eventType, PlanDecisionPayload{
		Revision: revision, Digest: plan.Digest, Operator: operator, Note: boundedDetail(note),
	}); err != nil {
		return PlanSnapshot{}, err
	}
	// An approval of a LATER revision supersedes the one before it, and records
	// exactly which downstream stages that invalidated. Only affected stages
	// appear: invalidating unrelated work to be safe would discard valid work.
	//
	// The predecessor is the APPROVED revision, which is sticky, rather than the
	// latest decision: proposing a new revision resets the pending decision, so
	// reading that field here meant the supersession was never recorded at all
	// in the ordinary propose-approve-propose-approve order.
	if governing, ok := snapshot.ApprovedRevision(); eventType == EventPlanApproved && ok && governing < revision {
		previous, previousFound, err := s.Store.PlanRevision(planID, governing)
		if err != nil {
			return PlanSnapshot{}, err
		}
		invalidated := []string{}
		if previousFound {
			invalidated = InvalidatedStages(previous, plan, snapshot)
		}
		if err := s.appendPlanEvent(planID, EventPlanRevisionSuperseded, PlanRevisionSupersededPayload{
			FromRevision: governing, ToRevision: revision,
			ProposalID: plan.Provenance.ProposalID, InvalidatedStages: invalidated,
		}); err != nil {
			return PlanSnapshot{}, err
		}
	}
	return s.Store.ReplayPlan(planID)
}

// InvalidatedStages is which downstream stages a revision invalidates.
//
// A stage is invalidated when the revision CHANGED it, or when something it
// depends on changed. A stage whose content and dependencies are unchanged
// keeps its completed work: reusing it is legitimate exactly while its exact
// output identity and obligations remain valid, and refusing to reuse it would
// throw away work an operator paid for.
func InvalidatedStages(previous, next domain.EngineeringPlan, snapshot PlanSnapshot) []string {
	changed := map[string]bool{}
	for _, stage := range next.Stages {
		before, existed := previous.Stage(stage.ID)
		if !existed {
			changed[stage.ID] = true
			continue
		}
		if !sameStageContent(before, stage) {
			changed[stage.ID] = true
		}
	}
	for _, stage := range previous.Stages {
		if _, still := next.Stage(stage.ID); !still {
			changed[stage.ID] = true
		}
	}
	// Propagate downstream: a stage whose input changed cannot keep an output
	// derived from the old one.
	for progressed := true; progressed; {
		progressed = false
		for _, stage := range next.Stages {
			if changed[stage.ID] {
				continue
			}
			for _, dependency := range stage.DependsOn {
				if changed[dependency] {
					changed[stage.ID] = true
					progressed = true
					break
				}
			}
		}
	}
	invalidated := make([]string, 0, len(changed))
	for id := range changed {
		// Only stages that had DONE something are invalidated: a pending stage
		// that changed has nothing to invalidate, it simply becomes the new
		// stage.
		if projection, ok := snapshot.Stages[id]; ok && projection.State != PlanStagePending {
			invalidated = append(invalidated, id)
		}
	}
	sort.Strings(invalidated)
	return invalidated
}

func sameStageContent(left, right domain.PlanStage) bool {
	leftJSON, leftErr := domain.CanonicalJSON(left)
	rightJSON, rightErr := domain.CanonicalJSON(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

// PlanView is the operator's whole answer about one plan: the exact revision,
// its replayed state, the assignments resolution would produce, and every stage
// that cannot be assigned with the reason.
type PlanView struct {
	Plan     domain.EngineeringPlan   `json:"plan"`
	Snapshot PlanSnapshot             `json:"state"`
	Assigned []domain.AgentAssignment `json:"assignments,omitempty"`
	Blocked  []planning.Blocked       `json:"blocked,omitempty"`
	// Consumed and Envelope are shown together, and unknown cost stays unknown:
	// an approval view that rendered "not reported" as zero would be telling an
	// operator something nobody measured.
	Envelope domain.PlanBudgetEnvelope `json:"budget_envelope"`
	Consumed domain.PlanConsumption    `json:"consumed"`
}

// View is the read behind `plan show` and `plan status`.
func (s PlanService) View(planID string) (PlanView, error) { return s.ViewRevision(planID, 0) }

// ViewRevision is the same view of ONE exact revision. Zero means the governing
// one, which is what `plan show` renders by default; naming a revision is how
// an operator reads the proposal they are being asked about while a different
// revision is executing.
func (s PlanService) ViewRevision(planID string, revision int) (PlanView, error) {
	plan, found, err := s.Store.Plan(planID)
	if err != nil {
		return PlanView{}, err
	}
	if !found {
		return PlanView{}, &PlanRefusedError{PlanID: planID, Detail: "no such plan"}
	}
	snapshot, err := s.Store.ReplayPlan(planID)
	if err != nil {
		return PlanView{}, err
	}
	if revision > 0 {
		exact, exactFound, err := s.Store.PlanRevision(planID, revision)
		if err != nil {
			return PlanView{}, err
		}
		if !exactFound {
			return PlanView{}, &PlanRefusedError{
				PlanID: planID, Detail: fmt.Sprintf("revision %d does not exist", revision),
			}
		}
		return s.viewOf(exact, snapshot)
	}
	// The revision shown is the one that GOVERNS: the approved revision when
	// there is one, and the latest proposal otherwise. Showing the newest
	// document while an older one is executing would misdescribe the work.
	if approved, ok := snapshot.ApprovedRevision(); ok && approved != plan.Revision {
		governing, governingFound, err := s.Store.PlanRevision(planID, approved)
		if err != nil {
			return PlanView{}, err
		}
		if governingFound {
			plan = governing
		}
	}
	return s.viewOf(plan, snapshot)
}

// viewOf renders one exact plan document beside the plan's replayed state.
func (s PlanService) viewOf(plan domain.EngineeringPlan, snapshot PlanSnapshot) (PlanView, error) {
	view := PlanView{Plan: plan, Snapshot: snapshot, Envelope: plan.BudgetEnvelope, Consumed: snapshot.Consumed}
	resolution, err := s.Resolve(plan, snapshot)
	if err != nil {
		return PlanView{}, err
	}
	view.Assigned, view.Blocked = resolution.Assignments, resolution.Blocked
	return view, nil
}

// Resolve maps the plan's agent stages onto profiles and workers.
//
// It is a pure read: resolution is recomputed from durable state rather than
// stored, so an agent that became available since the last look is visible
// immediately, and a stage already bound to a run keeps the frozen identity its
// assignment recorded.
func (s PlanService) Resolve(plan domain.EngineeringPlan, snapshot PlanSnapshot) (planning.Resolution, error) {
	upstream := map[string][]domain.UpstreamOutput{}
	for _, stage := range plan.Stages {
		var outputs []domain.UpstreamOutput
		for _, dependency := range stage.DependsOn {
			projection, ok := snapshot.Stages[dependency]
			if !ok || projection.RunID == "" {
				continue
			}
			output := domain.UpstreamOutput{StageID: dependency, RunID: projection.RunID}
			// A read failure is NOT "this stage published nothing". The
			// assignment resolved here is frozen immutably when the stage
			// starts, so swallowing the error would permanently base an
			// independent review on the trusted base and hand it no diff -
			// the exact defect the first live dogfood review reported.
			run, runFound, err := s.Store.Run(projection.RunID)
			if err != nil {
				return planning.Resolution{}, fmt.Errorf("stage %q depends on run %s and it could not be read: %w", stage.ID, projection.RunID, err)
			}
			if runFound {
				output.Candidate, output.Tree = run.Candidate.Revision, run.Candidate.Tree
			}
			outputs = append(outputs, output)
		}
		if len(outputs) > 0 {
			upstream[stage.ID] = outputs
		}
	}
	// The contract a plan was PLANNED AGAINST, not a fresh compilation: the
	// obligations a stage is resolved under have to be the ones the operator
	// approved the plan with.
	contract, found, err := s.Store.PlanContract(plan.ID, plan.Revision)
	if err != nil {
		return planning.Resolution{}, err
	}
	if !found {
		return planning.Resolution{}, &PlanRefusedError{
			PlanID: plan.ID,
			Detail: fmt.Sprintf("no work contract is stored for revision %d, so its stages cannot be resolved against the obligations it was planned under", plan.Revision),
		}
	}
	// The assignments already executing are read back rather than recomputed.
	// A stage that has run is executing under what an operator approved, and a
	// downstream independence obligation is about the worker that actually
	// produced the change - not about whichever worker would be chosen today.
	frozen := map[string]domain.AgentAssignment{}
	for stageID, projection := range snapshot.Stages {
		if projection.AssignmentID == "" {
			continue
		}
		assignment, found, err := s.frozenAssignment(plan, projection, stageID)
		if err != nil {
			return planning.Resolution{}, err
		}
		if found {
			frozen[stageID] = assignment
		}
	}
	return planning.Resolve(planning.ResolveInput{
		Plan: plan, Registry: s.Registry, Agents: s.Agents,
		Contract: contract, Upstream: upstream, DefaultAgent: s.DefaultAgent, Frozen: frozen,
	})
}

// frozenAssignment is the assignment a stage ACTUALLY executed under, which is
// not always stored under the revision governing now.
//
// Assignment rows are written under the revision that governed when the stage
// started, and a later revision that did not invalidate that stage leaves its
// completed work in place - so after an ordinary approve, revise, approve the
// row lives under the older revision. Looking only under the current one made
// the stage vanish from the frozen set, and a downstream independence
// obligation was then judged against a fresh re-resolution: whichever worker
// would be picked today rather than the one that produced the change.
//
// The stage's own RUN carries the revision it was created under, so that is
// what the search follows.
func (s PlanService) frozenAssignment(plan domain.EngineeringPlan, projection PlanStageProjection, stageID string) (domain.AgentAssignment, bool, error) {
	assignment, found, err := s.Store.PlanAssignment(plan.ID, plan.Revision, stageID)
	if err != nil || found {
		return assignment, found, err
	}
	if projection.RunID == "" {
		return domain.AgentAssignment{}, false, nil
	}
	run, runFound, err := s.Store.Run(projection.RunID)
	if err != nil {
		return domain.AgentAssignment{}, false, err
	}
	if !runFound || run.Plan == nil || run.Plan.Revision == plan.Revision {
		return domain.AgentAssignment{}, false, nil
	}
	return s.Store.PlanAssignment(plan.ID, run.Plan.Revision, stageID)
}

// ---------------------------------------------------------------------------
// The contract a plan revision was planned against
// ---------------------------------------------------------------------------

// PutPlanContract stores the compiled work contract one plan revision was
// planned against. Re-storing the same document is a no-op; a different one is
// refused, because the obligations a plan was approved under cannot change
// under the plan.
func (s *SQLiteOperationStore) PutPlanContract(planID string, revision int, contract domain.EngineeringWorkContract) error {
	if _, err := domain.Encode(contract); err != nil {
		return fmt.Errorf("contract for plan %q is invalid: %w", planID, err)
	}
	document, err := CanonicalJSON(contract)
	if err != nil {
		return err
	}
	var stored string
	switch err := s.db.QueryRow(`SELECT document FROM plan_contracts WHERE plan_id = ? AND revision = ?`, planID, revision).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case stored == string(document):
		return nil
	default:
		return fmt.Errorf("plan %s revision %d was planned against a different contract; a revision's obligations are immutable", planID, revision)
	}
	_, err = s.db.Exec(`INSERT INTO plan_contracts (plan_id, revision, document) VALUES (?, ?, ?)`, planID, revision, string(document))
	return err
}

// PlanContract returns the contract one revision was planned against.
func (s *SQLiteOperationStore) PlanContract(planID string, revision int) (domain.EngineeringWorkContract, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_contracts WHERE plan_id = ? AND revision = ?`, planID, revision).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EngineeringWorkContract{}, false, nil
	}
	if err != nil {
		return domain.EngineeringWorkContract{}, false, err
	}
	contract, err := domain.Decode[domain.EngineeringWorkContract]([]byte(document))
	return contract, err == nil, err
}

// ---------------------------------------------------------------------------
// The source a plan answers
// ---------------------------------------------------------------------------

// BindPlanSource records which issue a plan answers. It is idempotent and
// never changes an existing binding: a plan answers one source, and moving it
// to another would silently redirect every stage run it creates.
func (s *SQLiteOperationStore) BindPlanSource(planID string, issue int) error {
	if issue <= 0 {
		return fmt.Errorf("a plan answers a source issue, and %d is not one", issue)
	}
	result, err := s.db.Exec(`UPDATE plans SET source_issue = ? WHERE id = ? AND source_issue = 0`, issue, planID)
	if err != nil {
		return err
	}
	bound, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if bound == 1 {
		return nil
	}
	// Nothing was updated, which is either the idempotent case - already bound
	// to this same issue - or a conflict. They are different facts, and
	// answering both with silence let a caller believe it had redirected a plan
	// that goes on answering its original issue.
	_, existing, found, err := s.PlanSource(planID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("plan %s does not exist, so it cannot be bound to issue #%d", planID, issue)
	}
	if existing != issue {
		return fmt.Errorf("plan %s already answers issue #%d and cannot be rebound to #%d: every stage run it creates answers its source", planID, existing, issue)
	}
	return nil
}

// PlanSource is the repository and issue one plan answers.
func (s *SQLiteOperationStore) PlanSource(planID string) (string, int, bool, error) {
	var repository string
	var issue int
	err := s.db.QueryRow(`SELECT repository, source_issue FROM plans WHERE id = ?`, planID).Scan(&repository, &issue)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	return repository, issue, true, nil
}

// ---------------------------------------------------------------------------
// Journal helpers
// ---------------------------------------------------------------------------

func (s PlanService) appendPlanEvent(planID, eventType string, payload any) error {
	return appendPlanEvent(s.Store, s.now(), planID, eventType, payload)
}

// appendPlanEvent is shared with the plan reconciler so both write through one
// path: the journal allocates the sequence and links the chain, and no caller
// chooses either.
func appendPlanEvent(store *SQLiteOperationStore, at time.Time, planID, eventType string, payload any) error {
	encoded, err := marshalPayloadJSON(payload)
	if err != nil {
		return err
	}
	_, err = store.AppendPlanEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            newEventID(planID),
		PlanID:        planID,
		Type:          eventType,
		OccurredAt:    at,
		Payload:       encoded,
	})
	return err
}

func (s PlanService) recordValidation(planID string, revision int, digest string, status domain.ProposalValidationStatus, cause error) error {
	payload := PlanValidatedPayload{Revision: revision, Digest: digest, Status: string(status)}
	if cause != nil {
		var invalid *planning.ValidationError
		if errors.As(cause, &invalid) {
			payload.Errors = boundedReasons(invalid.Reasons)
		} else {
			payload.Errors = []string{boundedDetail(cause.Error())}
		}
	}
	return s.appendPlanEvent(planID, EventPlanValidated, payload)
}

func agentStageCount(plan domain.EngineeringPlan) int {
	count := 0
	for _, stage := range plan.Stages {
		if stage.Kind == domain.StageAgent {
			count++
		}
	}
	return count
}

func budgetPayload(envelope domain.PlanBudgetEnvelope) PlanBudgetPayload {
	return PlanBudgetPayload{
		MaxChildRuns: envelope.MaxChildRuns, MaxConcurrency: envelope.MaxConcurrency,
		MaxProviderInvocations: envelope.MaxProviderInvocations, MaxWallSeconds: envelope.MaxWallSeconds,
		MaxCostMicros: envelope.MaxCostMicros,
	}
}

func reasoningPayload(reasoning domain.PlanReasoningProvenance) *PlanReasoningPayload {
	return &PlanReasoningPayload{
		AgentID: reasoning.AgentID, ProviderKind: reasoning.ProviderKind, VendorFamily: reasoning.VendorFamily,
		TrustMode: string(reasoning.TrustMode), Model: reasoning.Model, ProfileID: reasoning.ProfileID,
		InvocationMode: string(reasoning.InvocationMode), ProviderMode: reasoning.ProviderMode,
		WorkspaceDigestBefore: reasoning.WorkspaceDigestBefore, WorkspaceDigestAfter: reasoning.WorkspaceDigestAfter,
		WorkspaceUnchanged: reasoning.WorkspaceUnchanged,
	}
}

// boundedReasons applies the same detail bound to every element, so a durable
// payload cannot grow without limit however many reasons a validator produced.
func boundedReasons(values []string) []string {
	const maxReasons = 32
	if len(values) > maxReasons {
		values = values[:maxReasons]
	}
	bounded := make([]string, 0, len(values))
	for _, value := range values {
		bounded = append(bounded, boundedDetail(value))
	}
	return bounded
}
