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
	// Evidence is the durable transcripts of the reasoning invocation. It is
	// carried here so a REFUSED proposal can reference the same evidence a
	// successful one is explained by: a refusal whose transcript an operator has
	// to find by guessing an artifact path is a refusal nobody diagnoses.
	Evidence []Artifact
	// References is what referenced-issue hydration produced for this proposal.
	// It is recorded on a refused attempt so "the planner was working from an
	// abbreviated cohort description" is a durable fact rather than something
	// the model happens to mention in prose.
	References []PlanSourceReferencePayload
	// Origin states where the proposal came from, for the durable record.
	Origin string
}

// PlanAttemptRefusedError is the typed refusal for a proposal that deterministic
// compilation turned down.
//
// It names the durable attempt so the operator's next step is a command rather
// than a search. The underlying error is preserved, so every caller that was
// matching on planning.ValidationError still matches.
type PlanAttemptRefusedError struct {
	PlanID    string
	AttemptID string
	cause     error
}

func (e *PlanAttemptRefusedError) Error() string {
	return fmt.Sprintf("%s\nthe proposal and its evidence are preserved as plan attempt %s: read it with `autonomy plan show %s --text`",
		e.cause.Error(), e.AttemptID, e.PlanID)
}

func (e *PlanAttemptRefusedError) Unwrap() error { return e.cause }

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
		// The refusal is durable in BOTH shapes it can take.
		//
		// When a plan already exists, the verdict is recorded against the
		// revision the proposal would have replaced, exactly as before. When
		// this is a FIRST proposal there is no revision to be about - nothing
		// compiled, so no document exists to digest - and the attempt itself
		// becomes the durable record instead. The earlier code wrote nothing at
		// all in that case, on the reasoning that a plan row would be a plan
		// that never existed. That reasoning holds for a plan DOCUMENT and not
		// for the attempt: the operator asked for this work, a registered agent
		// reasoned about it, and an invocation was spent. Leaving them with "no
		// such plan" described none of that.
		if found {
			if err := s.recordValidation(input.PlanID, revision, "", domain.ProposalRefused, compileErr); err != nil {
				return domain.EngineeringPlan{}, err
			}
		}
		attemptID, err := s.recordRefusedAttempt(input, revision, origin, compileErr)
		if err != nil {
			// Recording the evidence failed, which is a worse thing to hide
			// than the refusal it was about. Both are reported: the compile
			// refusal is the answer, and the write failure is why it was not
			// preserved.
			return domain.EngineeringPlan{}, fmt.Errorf("%w (and the refused attempt could not be recorded: %v)", compileErr, err)
		}
		return domain.EngineeringPlan{}, &PlanAttemptRefusedError{PlanID: input.PlanID, AttemptID: attemptID, cause: compileErr}
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
		ProposalID: plan.Provenance.ProposalID, References: input.References,
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

// unstarted is the assignments an approval can still bind.
//
// A stage that is still executing under the row it froze is not bound again:
// that row is what every later look reads, and recording it here would file the
// revision it STARTED under as this revision's approval, which is a claim about
// the wrong performance. What governs it being performed again under the SAME
// revision is the privilege comparison against its previous performance.
//
// The snapshot handed in decides which stages those are, and it must be the
// state approving would leave. A stage this revision invalidates has started
// under the old one and not under this one, so the prospective state reads it
// as pending and it IS bound here.
//
// A stage that resolved to nothing is not bound either: the operator was shown
// a blocker rather than an assignment, and there is nothing to hold execution
// to.
func unstarted(assignments []domain.AgentAssignment, snapshot PlanSnapshot) []domain.AgentAssignment {
	bound := make([]domain.AgentAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		if projection, ok := snapshot.Stages[assignment.StageID]; ok && projection.AssignmentID != "" {
			continue
		}
		bound = append(bound, assignment)
	}
	return bound
}

// Approve records the operator's decision on ONE exact revision.
//
// The digest is checked, not merely the number: approving a revision number
// whose content could since have changed would be approving something nobody
// looked at.
// The assignments digest is REQUIRED: it is what `plan show` printed beside the
// revision, and an approval that named only the revision authorized whichever
// assignments the registry happened to resolve at decide time. Naming it is
// what makes the approval a decision about work the operator actually read.
func (s PlanService) Approve(planID string, revision int, digest, assignments, operator, note string) (PlanSnapshot, error) {
	return s.decide(planID, revision, digest, assignments, operator, note, EventPlanApproved)
}

// Reject records a refusal. It is durable for the same reason an approval is:
// "we looked at this and said no" is a fact about the work.
func (s PlanService) Reject(planID string, revision int, digest, assignments, operator, note string) (PlanSnapshot, error) {
	return s.decide(planID, revision, digest, assignments, operator, note, EventPlanRejected)
}

func (s PlanService) decide(planID string, revision int, digest, assignments, operator, note, eventType string) (PlanSnapshot, error) {
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
	// A REJECTION binds nothing, so there is no set for it to name. Accepting
	// the argument and ignoring it would tell an operator their decision had
	// been checked against something when nothing had been checked at all.
	if eventType != EventPlanApproved && strings.TrimSpace(assignments) != "" {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: "a rejection binds no assignments, so it cannot name a set: drop --assignments, or approve the revision that set belongs to",
		}
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
	// WHAT THE OPERATOR IS APPROVING, durably, before the approval exists.
	//
	// An approval is a decision about a document AND about the assignments
	// shown beside it. Resolution is otherwise recomputed from the live
	// registry on every look, so an unstarted stage was re-resolved when it
	// finally became dependency-ready: an edited profile, an edited
	// instruction pack or a changed default worker between the approval and
	// the first run meant execution froze a configuration nobody had seen.
	//
	// Written FIRST, so a crash before the event leaves rows nothing points at
	// - which the retried approval re-writes identically - rather than an
	// approval whose binding was lost.
	// AN APPROVAL NAMES THE SET IT APPROVES.
	//
	// The revision digest binds the plan DOCUMENT, and operator configuration
	// is not in that document: a profile, an instruction pack or the workforce
	// edited between reading a proposal and deciding on it leaves the plan
	// digest identical while changing who would perform the work. An approval
	// that names only the document therefore authorizes assignments nobody
	// read, which is the whole of what this boundary exists to stop - so it is
	// REQUIRED rather than checked-if-offered.
	//
	// It is required of new approvals only. An approval already durable in a
	// journal was written before this existed and stays readable and operable;
	// what cannot happen is a NEW one being written without it.
	if eventType == EventPlanApproved && strings.TrimSpace(assignments) == "" {
		return PlanSnapshot{}, &PlanRefusedError{
			PlanID: planID,
			Detail: fmt.Sprintf("an approval of revision %d must name the assignments it approves as well as the revision: run `autonomy plan show %s` and use the command it prints", revision, planID),
		}
	}
	assignmentsDigest := ""
	if eventType == EventPlanApproved {
		// Against the state approving WOULD leave, not the state it replaces.
		//
		// This is the same computation `plan show --revision N` renders, and
		// using anything else binds a different document than the operator was
		// shown. A stage this revision materially changes is reset by the
		// supersession below - lifecycle cleared, generation back to zero - so
		// reading the pre-approval snapshot saw it as "already started", left
		// it unbound, and then nothing governed it: the privilege comparison
		// engages only above generation zero, and the supersession had just
		// taken it back to zero. The most ordinary flow there is, revising a
		// plan mid-execution, therefore live-resolved the changed stage from
		// whatever the registry said at first run.
		_, prospective, err := s.previewSnapshot(plan, snapshot)
		if err != nil {
			return PlanSnapshot{}, err
		}
		resolution, err := s.Resolve(plan, prospective)
		if err != nil {
			return PlanSnapshot{}, err
		}
		bindable := unstarted(resolution.Assignments, prospective)
		// The decision decides the set it named, or nothing.
		shown, err := assignmentSetDigest(bindable)
		if err != nil {
			return PlanSnapshot{}, err
		}
		if named := strings.TrimSpace(assignments); named != shown {
			return PlanSnapshot{}, &PlanRefusedError{
				PlanID: planID,
				Detail: fmt.Sprintf("revision %d now resolves assignments %s and the decision names %s: read it again, because who would perform this work has changed since you looked",
					revision, short12(shown), short12(named)),
			}
		}
		if assignmentsDigest, err = s.Store.PutApprovedAssignments(planID, revision, bindable); err != nil {
			return PlanSnapshot{}, err
		}
	}
	if err := s.appendPlanEvent(planID, eventType, PlanDecisionPayload{
		Revision: revision, Digest: plan.Digest, Operator: operator, Note: boundedDetail(note),
		AssignmentsDigest: assignmentsDigest,
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
	// Preview is present when the revision shown is NOT the one governing the
	// work. The state beside it is then prospective - what approving this
	// revision would leave - rather than a report of what is happening.
	Preview *PlanPreview `json:"preview,omitempty"`
	// AssignmentsDigest is the digest of the assignments approving THIS view
	// would bind: who performs each stage that has not started, under which
	// profile, packs, context and worker.
	//
	// It exists so a decision can name it. The plan digest binds the document,
	// and registry and workforce edits do not change that document - so an edit
	// landing between reading a proposal and deciding on it would be bound as
	// "what the operator saw". Naming this closes that the same way naming the
	// revision digest closed deciding an unread document.
	AssignmentsDigest string `json:"assignments_digest,omitempty"`
	// Unbound is the stages this revision's approval bound NOTHING for, in
	// stage order: they showed a blocker rather than an assignment, so there
	// was no identity to hold execution to and they resolve live when they
	// become performable.
	Unbound []string `json:"unbound,omitempty"`
}

// PlanPreview says that a view is an answer to "what would approving this do",
// and names what approving it would throw away.
type PlanPreview struct {
	// GoverningRevision is the revision actually executing.
	GoverningRevision int `json:"governing_revision"`
	// Invalidated is the stages whose completed work approving this revision
	// would discard, in stage order.
	Invalidated []string `json:"invalidated,omitempty"`
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
		// An exact revision that is not the governing one is a PREVIEW. It was
		// rendered against live state, so a stage this revision materially
		// changes still showed as "completed by" the worker that performed the
		// PREVIOUS revision's version of it - work approving this revision
		// would immediately invalidate and redo. That is not an approval
		// preview; it is one document decorated with another's execution.
		preview, prospective, err := s.previewSnapshot(exact, snapshot)
		if err != nil {
			return PlanView{}, err
		}
		view, err := s.viewOf(exact, prospective)
		if err != nil {
			return PlanView{}, err
		}
		view.Preview = preview
		return view, nil
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
// previewSnapshot answers what durable state WOULD look like if this revision
// were approved now.
//
// It applies exactly the rule approval applies - InvalidatedStages, the one
// computation that decides what a revision keeps and what it redoes - so the
// preview cannot drift from the thing it previews. A stage whose content and
// dependencies are unchanged keeps its completed work, because approving would
// keep it; every other stage reads as pending, because approving would redo it.
// Stages that exist only in the governing revision are absent, because this
// document does not contain them.
//
// Nothing is written. It returns the governing revision unchanged when the
// requested one IS governing, and when the governing document cannot be read it
// refuses rather than falling back to live state, which is the thing that was
// wrong.
func (s PlanService) previewSnapshot(plan domain.EngineeringPlan, snapshot PlanSnapshot) (*PlanPreview, PlanSnapshot, error) {
	governing, ok := snapshot.ApprovedRevision()
	if !ok || governing == plan.Revision {
		return nil, snapshot, nil
	}
	previous, found, err := s.Store.PlanRevision(plan.ID, governing)
	if err != nil {
		return nil, PlanSnapshot{}, err
	}
	if !found {
		return nil, PlanSnapshot{}, &PlanRefusedError{
			PlanID: plan.ID,
			Detail: fmt.Sprintf("revision %d governs the work and is not stored, so revision %d cannot be previewed against it", governing, plan.Revision),
		}
	}
	invalid := map[string]bool{}
	for _, id := range InvalidatedStages(previous, plan, snapshot) {
		invalid[id] = true
	}
	prospective := snapshot
	prospective.Stages = make(map[string]PlanStageProjection, len(plan.Stages))
	preview := &PlanPreview{GoverningRevision: governing}
	for _, stage := range plan.Stages {
		projection, executed := snapshot.Stages[stage.ID]
		if executed && !invalid[stage.ID] {
			prospective.Stages[stage.ID] = projection
			continue
		}
		if executed && projection.State != PlanStagePending {
			preview.Invalidated = append(preview.Invalidated, stage.ID)
		}
		prospective.Stages[stage.ID] = PlanStageProjection{StageID: stage.ID, State: PlanStagePending}
	}
	return preview, prospective, nil
}

func (s PlanService) viewOf(plan domain.EngineeringPlan, snapshot PlanSnapshot) (PlanView, error) {
	view := PlanView{Plan: plan, Snapshot: snapshot, Envelope: plan.BudgetEnvelope, Consumed: snapshot.Consumed}
	resolution, err := s.Resolve(plan, snapshot)
	if err != nil {
		return PlanView{}, err
	}
	view.Assigned, view.Blocked = resolution.Assignments, resolution.Blocked
	bindable := unstarted(resolution.Assignments, snapshot)
	if view.AssignmentsDigest, err = assignmentSetDigest(bindable); err != nil {
		return PlanView{}, err
	}
	// What approving would NOT bind, said out loud. A stage that showed a
	// blocker has no approval-visible assignment, so it resolves live when the
	// blocker clears - and rendering it later beside bound stages, identically,
	// is what would let an operator believe it was approved.
	assigned := make(map[string]bool, len(bindable))
	for _, assignment := range bindable {
		assigned[assignment.StageID] = true
	}
	for _, stage := range plan.Stages {
		if stage.Kind != domain.StageAgent {
			continue
		}
		if projection, started := snapshot.Stages[stage.ID]; started && projection.AssignmentID != "" {
			continue
		}
		if !assigned[stage.ID] {
			view.Unbound = append(view.Unbound, stage.ID)
		}
	}
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
	// And the assignments this revision's APPROVAL bound, for the stages that
	// have not started. A stage that has started is read through what it
	// froze; a stage that has not is resolved to what the operator approved
	// rather than to whatever the registry would choose today.
	authorized, err := s.authorizedAssignments(plan, snapshot)
	if err != nil {
		return planning.Resolution{}, err
	}
	return planning.Resolve(planning.ResolveInput{
		Plan: plan, Registry: s.Registry, Agents: s.Agents,
		Contract: contract, Upstream: upstream, DefaultAgent: s.DefaultAgent,
		Frozen: frozen, Authorized: authorized,
	})
}

// authorizedAssignments is what this revision's approval bound, and only what
// its approval bound.
//
// The rows live in a table beside the journal, and a table is editable while a
// hash-chained event is not. So the digest the approval RECORDED is what
// decides whether those rows may authorize anything: they are read, digested
// with the same definition the approval used, and refused unless they are the
// set the operator approved. Two durable sources that can disagree are one
// durable source and one opportunity, and the side table must not be the one
// that wins.
//
// Three shapes, deliberately distinguished:
//
//   - The revision is not the one an operator approved. Nothing it may have
//     rows for is authority here; a preview resolves live, which is what a
//     preview is for.
//   - The approval recorded NO digest. It was written before this boundary
//     existed, so it bound nothing and there is nothing to check rows against -
//     and rows existing anyway is a disagreement in its starkest form.
//   - The approval recorded one. The rows have to be it.
func (s PlanService) authorizedAssignments(plan domain.EngineeringPlan, snapshot PlanSnapshot) (map[string]domain.AgentAssignment, error) {
	rows, err := s.Store.ApprovedAssignments(plan.ID, plan.Revision)
	if err != nil {
		return nil, err
	}
	approved := snapshot.Approved
	if approved.Status != domain.ApprovalApproved || approved.Revision != plan.Revision {
		return nil, nil
	}
	if approved.AssignmentsDigest == "" {
		if len(rows) > 0 {
			return nil, &PlanRefusedError{
				PlanID: plan.ID,
				Detail: fmt.Sprintf("revision %d has %d stored approved assignments and its approval names none, so nothing proves an operator approved them",
					plan.Revision, len(rows)),
			}
		}
		return nil, nil
	}
	stored, err := assignmentSetDigest(assignmentsOf(rows))
	if err != nil {
		return nil, err
	}
	if stored != approved.AssignmentsDigest {
		return nil, &PlanRefusedError{
			PlanID: plan.ID,
			Detail: fmt.Sprintf("revision %d was approved with assignments %s and the stored assignments are %s: the approval and what it authorized disagree, so nothing executes",
				plan.Revision, short12(approved.AssignmentsDigest), short12(stored)),
		}
	}
	return rows, nil
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
	// The generation the stage is CURRENTLY performing, so a re-performed stage
	// is read through the assignment it is actually executing under rather than
	// through the one whose work was invalidated.
	assignment, found, err := s.Store.PlanAssignment(plan.ID, plan.Revision, projection.Generation, stageID)
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
	// The run records which generation it was created for, so an older
	// revision's stage is read through the exact performance it executed.
	return s.Store.PlanAssignment(plan.ID, run.Plan.Revision, run.Plan.Generation, stageID)
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
// The assignments an approval bound
// ---------------------------------------------------------------------------

// PutApprovedAssignments records the assignments an operator saw when they
// approved one revision, and returns the canonical digest of the bound set.
//
// It is immutable per (plan, revision, stage). Re-recording the same document
// is a no-op, so a retry after a crash between this and the approval event
// ordinarily completes; a DIFFERENT document is refused, because what an
// approval bound cannot be changed under the approval.
//
// "Ordinarily", because the retry re-resolves: a producer that settled inside
// that window changes the upstream a dependent assignment carries, and the
// retry then refuses. That is fail-closed and rare, and the remedy is a new
// proposal rather than anything this function can do - so the refusal says
// which stage disagreed.
func (s *SQLiteOperationStore) PutApprovedAssignments(planID string, revision int, assignments []domain.AgentAssignment) (string, error) {
	for _, assignment := range assignments {
		if _, err := domain.Encode(assignment); err != nil {
			return "", fmt.Errorf("approved assignment for stage %q is invalid: %w", assignment.StageID, err)
		}
		document, err := CanonicalJSON(assignment)
		if err != nil {
			return "", err
		}
		var stored string
		switch err := s.db.QueryRow(`SELECT document FROM plan_approved_assignments WHERE plan_id = ? AND revision = ? AND stage_id = ?`,
			planID, revision, assignment.StageID).Scan(&stored); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := s.db.Exec(`INSERT INTO plan_approved_assignments (plan_id, revision, stage_id, document) VALUES (?, ?, ?, ?)`,
				planID, revision, assignment.StageID, string(document)); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case stored != string(document):
			return "", fmt.Errorf("plan %s revision %d already bound stage %s to a different assignment; an approval's assignments are immutable",
				planID, revision, assignment.StageID)
		}
	}
	return s.ApprovedAssignmentsDigest(planID, revision)
}

// assignmentSetDigest is the canonical digest of one bound set, in stage order.
//
// It is ONE function because two callers have to agree exactly: the approval
// surface computes it over what it is about to show, and the approval computes
// it over what it is about to bind. A second implementation would be a second
// answer to "is this the set the operator read".
func assignmentSetDigest(assignments []domain.AgentAssignment) (string, error) {
	ordered := make([]domain.AgentAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		// The UPSTREAM CANDIDATE is erased. It is the one execution fact an
		// approval cannot bind and does not try to - it is rebound from the
		// settled producer when the stage starts - so leaving it in would make
		// the digest move when a producer settles between reading a proposal
		// and deciding on it, and refuse the decision saying that who would
		// perform the work had changed. It had not. What this names is who
		// performs each stage and under what configuration, which is what the
		// refusal claims and what the approval actually binds.
		assignment.Context.UpstreamOutputs = nil
		ordered = append(ordered, assignment)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StageID < ordered[j].StageID })
	// The EMPTY set digests too, rather than to the empty string. "Nothing is
	// bindable here" is a fact an operator can read and name, and collapsing it
	// into "no digest given" left the one direction unpinnable: a set that
	// APPEARED between reading and deciding would have been bound unchecked.
	return domain.Digest(ordered)
}

// ApprovedAssignments returns what one revision's approval bound, keyed by
// stage. An empty map means the revision has not been approved, or was
// approved before this boundary existed.
func (s *SQLiteOperationStore) ApprovedAssignments(planID string, revision int) (map[string]domain.AgentAssignment, error) {
	rows, err := s.db.Query(`SELECT stage_id, document FROM plan_approved_assignments WHERE plan_id = ? AND revision = ? ORDER BY stage_id`, planID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bound := map[string]domain.AgentAssignment{}
	for rows.Next() {
		var stageID, document string
		if err := rows.Scan(&stageID, &document); err != nil {
			return nil, err
		}
		// Decoded through the SCHEMA, like every other durable artifact this
		// package reads back. A row that no longer satisfies it authorizes
		// nothing.
		assignment, err := domain.Decode[domain.AgentAssignment]([]byte(document))
		if err != nil {
			return nil, fmt.Errorf("decode approved assignment for stage %q: %w", stageID, err)
		}
		bound[stageID] = assignment
	}
	return bound, rows.Err()
}

// ApprovedAssignmentsDigest is the canonical digest of the rows AS THEY STAND
// for one revision. It is what the approval event carries, so the binding lives
// in the hash-chained journal and not only in a table beside it - and it is
// what those rows are checked against before they authorize anything.
//
// Zero rows digest to the canonical digest of the empty set rather than to the
// empty string. "This approval bound nothing" is a fact the journal can record
// and this can confirm; "no binding was ever recorded" is the ABSENCE of a
// digest in the approval event, and the two are not the same.
func (s *SQLiteOperationStore) ApprovedAssignmentsDigest(planID string, revision int) (string, error) {
	bound, err := s.ApprovedAssignments(planID, revision)
	if err != nil {
		return "", err
	}
	return assignmentSetDigest(assignmentsOf(bound))
}

// assignmentsOf is a bound set as a slice, in no particular order:
// assignmentSetDigest imposes the canonical one.
func assignmentsOf(bound map[string]domain.AgentAssignment) []domain.AgentAssignment {
	assignments := make([]domain.AgentAssignment, 0, len(bound))
	for _, assignment := range bound {
		assignments = append(assignments, assignment)
	}
	return assignments
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
	return appendPlanEventWithArtifacts(store, at, planID, eventType, payload, nil)
}

// appendPlanEventWithArtifacts is the same append with durable evidence
// attached. The artifacts go through the journal's own ValidateArtifact
// discipline, so a plan event cannot reference evidence that would not be
// accepted anywhere else in this runtime.
func appendPlanEventWithArtifacts(store *SQLiteOperationStore, at time.Time, planID, eventType string, payload any, artifacts []Artifact) error {
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
		Artifacts:     artifacts,
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

// ---------------------------------------------------------------------------
// Refused planning attempts
// ---------------------------------------------------------------------------

// recordRefusedAttempt preserves one refused proposal as durable, inspectable,
// non-executable planning evidence, and returns its identity.
//
// What it deliberately does NOT do is persist an invalid EngineeringPlan. There
// is no revision document, no digest and no assignment set, because none of
// those exist: compilation is what produces them and compilation refused. What
// is written is a plan IDENTITY with no revision - which Plans() and Plan()
// both decline to return, so nothing executable can see it - and one
// plan.attempt_refused event in the plan's own journal stream.
//
// The journal is the durability mechanism on purpose. An attempt read before a
// restart and an attempt read after one are the same replayed events, so the
// record cannot mean one thing in a live process and another thing in a fresh
// one.
func (s PlanService) recordRefusedAttempt(input ProposeInput, revision int, origin string, cause error) (string, error) {
	repository := strings.TrimSpace(input.Repository)
	if repository == "" {
		repository = input.Subject.Repository
	}
	if repository == "" {
		return "", &PlanRefusedError{PlanID: input.PlanID, Detail: "a plan attempt is bound to a repository"}
	}
	if _, err := s.Store.ClaimPlanAttempt(input.PlanID, repository, s.now()); err != nil {
		return "", err
	}
	// The identity answers the SOURCE, whether or not it ever reached a plan.
	// Binding it here is what lets `plan show` say which issue the operator was
	// planning; without it the record described an attempt at nothing in
	// particular.
	if input.Issue > 0 {
		if err := s.Store.BindPlanSource(input.PlanID, input.Issue); err != nil {
			return "", err
		}
	}
	// WHICH try this is, counted from the durable journal rather than from a
	// field somebody increments. Two refusals at the same revision are two
	// attempts, and a restart between them does not restart the count.
	snapshot, err := s.Store.ReplayPlan(input.PlanID)
	if err != nil {
		return "", err
	}
	attempt := 1
	for _, previous := range snapshot.Attempts {
		if previous.Revision == revision {
			attempt++
		}
	}
	attemptID := fmt.Sprintf("attempt-%s-r%d-%d", input.PlanID, revision, attempt)
	payload := PlanAttemptRefusedPayload{
		AttemptID: attemptID, Revision: revision, Origin: origin, Issue: input.Issue,
		Stages: attemptStages(input.Reasoned), Errors: boundedReasons(refusalReasons(cause)),
		Evidence: attemptEvidence(input.Evidence), References: input.References,
	}
	if input.Reasoning != nil {
		payload.Reasoning = reasoningPayload(*input.Reasoning)
	}
	// The transcripts are attached as event ARTIFACTS as well as referenced in
	// the payload, so they pass the same ValidateArtifact discipline every other
	// piece of durable evidence passes on the way in and on every replay.
	return attemptID, appendPlanEventWithArtifacts(s.Store, s.now(), input.PlanID,
		EventPlanAttemptRefused, payload, input.Evidence)
}

// attemptStages reduces the proposal to the members that decide whether it is
// executable. Objectives and rationales are provider prose and stay in the
// transcript; the journal carries no provider text.
func attemptStages(stages []domain.PlanStage) []PlanAttemptStagePayload {
	proposed := make([]PlanAttemptStagePayload, 0, len(stages))
	for _, stage := range stages {
		item := PlanAttemptStagePayload{
			ID: stage.ID, Kind: string(stage.Kind), Role: string(stage.Role),
			DependsOn: stage.DependsOn,
		}
		if stage.Independence != nil {
			// DifferentFrom is copied EXACTLY, empty included. An empty list is
			// the fact that explains the refusal in the case this record was
			// built for, and normalizing it to absent would erase the
			// difference between "asked for no independence" and "asked for
			// independence over nothing".
			item.Independence = &PlanAttemptIndependencePayload{
				Dimension:     string(stage.Independence.Dimension),
				DifferentFrom: append([]string{}, stage.Independence.DifferentFrom...),
			}
		}
		proposed = append(proposed, item)
	}
	return proposed
}

func attemptEvidence(artifacts []Artifact) []PlanAttemptEvidenceRef {
	refs := make([]PlanAttemptEvidenceRef, 0, len(artifacts))
	for _, artifact := range artifacts {
		refs = append(refs, PlanAttemptEvidenceRef{
			Path: artifact.Path, SHA256: artifact.SHA256,
			MediaType: artifact.MediaType, LocalOnly: artifact.LocalOnly,
		})
	}
	return refs
}

// PlanAttemptsView is what `plan show` renders for a plan identity that has no
// executable revision: every refused attempt, oldest first.
//
// Executable is stated rather than implied. "This plan has no EngineeringPlan"
// is the single most important thing the view says, and an operator should not
// have to infer it from an absent field.
type PlanAttemptsView struct {
	PlanID     string        `json:"plan_id"`
	Repository string        `json:"repository"`
	Issue      int           `json:"source_issue,omitempty"`
	Executable bool          `json:"executable_plan_exists"`
	Attempts   []PlanAttempt `json:"attempts"`
}

// AttemptsView reads the refused planning attempts of one plan identity.
//
// It refuses an identity that does not exist at all, and it is readable whether
// or not the plan later reached a revision: an attempt that failed before a
// successful retry is still a fact about what happened.
func (s PlanService) AttemptsView(planID string) (PlanAttemptsView, error) {
	identities, err := s.Store.PlanIdentities()
	if err != nil {
		return PlanAttemptsView{}, err
	}
	for _, identity := range identities {
		if identity.PlanID != planID {
			continue
		}
		snapshot, err := s.Store.ReplayPlan(planID)
		if err != nil {
			return PlanAttemptsView{}, err
		}
		return PlanAttemptsView{
			PlanID: planID, Repository: identity.Repository, Issue: identity.Issue,
			Executable: identity.Revision > 0, Attempts: snapshot.Attempts,
		}, nil
	}
	return PlanAttemptsView{}, &PlanRefusedError{PlanID: planID, Detail: "no such plan"}
}
