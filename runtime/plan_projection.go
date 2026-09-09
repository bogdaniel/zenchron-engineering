package runtime

// The plan reducer.
//
// A plan's durable truth is its append-only event stream, exactly as a run's is.
// This file is the ONE reducer for that stream: approval state, per-stage state,
// assignments, child-run associations, gate satisfaction and consumed budget are
// all PROJECTIONS of events, never stored counters.
//
// That choice is what makes two of #64's frozen laws structural rather than
// remembered:
//
//   - a revision, reassignment, restart or decomposition cannot reset consumed
//     budget, because consumption is a sum over immutable events and nothing can
//     subtract from it;
//   - a restart reconstructs plan state without provider session memory,
//     because the only input is the journal.

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// PlanStageState is where one stage has got to.
//
// A gate reaches Satisfied and never Running or Completed: it performs no work,
// so the vocabulary that would describe work is deliberately not available to
// it. An agent stage reaches Completed or Failed and never Satisfied.
type PlanStageState string

const (
	// PlanStagePending is assigned or declared, not yet started.
	PlanStagePending PlanStageState = "pending"
	// PlanStageRunning is an agent stage whose EngineeringRun exists.
	PlanStageRunning PlanStageState = "running"
	// PlanStageCompleted and PlanStageFailed are how an agent stage ended.
	PlanStageCompleted PlanStageState = "completed"
	PlanStageFailed    PlanStageState = "failed"
	// PlanStageInvalidated is a stage whose assumptions an upstream revision
	// or failure invalidated. It is neither completed nor failed: nothing was
	// wrong with it, and it no longer applies.
	PlanStageInvalidated PlanStageState = "invalidated"
	// PlanStageSatisfied is a typed gate whose existing durable references
	// prove it.
	PlanStageSatisfied PlanStageState = "satisfied"
)

// PlanGateSatisfaction is the durable evidence, authority or human decision
// reference that satisfied a gate. The plan stores references only: duplicating
// an evidence bundle or an authority decision inside a plan would be a second
// evidence model, and #64 forbids one.
type PlanGateSatisfaction struct {
	Kind            domain.StageKind `json:"kind"`
	Claims          []string         `json:"claims,omitempty"`
	Evidence        Ref              `json:"evidence,omitzero"`
	Decision        Ref              `json:"decision,omitzero"`
	HumanEvidenceID string           `json:"human_evidence_id,omitempty"`
	// ProvenHeads is what each proving run carried when it proved this gate.
	ProvenHeads []string `json:"proven_heads,omitempty"`
}

// PlanStageProjection is one stage's replayed state, including the exact
// configuration that was frozen into its assignment.
type PlanStageProjection struct {
	StageID string         `json:"stage_id"`
	State   PlanStageState `json:"state"`
	Reason  string         `json:"reason,omitempty"`
	// The assignment freeze. These are the identities that will have performed
	// the work, and editing a profile afterwards cannot change them.
	AssignmentID   string                 `json:"assignment_id,omitempty"`
	Role           domain.EngineeringRole `json:"role,omitempty"`
	ProfileID      string                 `json:"profile_id,omitempty"`
	ProfileVersion int                    `json:"profile_version,omitempty"`
	ProfileDigest  string                 `json:"profile_digest,omitempty"`
	AgentID        string                 `json:"agent_id,omitempty"`
	ProviderKind   string                 `json:"provider_kind,omitempty"`
	VendorFamily   string                 `json:"vendor_family,omitempty"`
	TrustMode      string                 `json:"trust_mode,omitempty"`
	InvocationMode domain.InvocationMode  `json:"invocation_mode,omitempty"`
	// RunID is the ordinary #63 EngineeringRun this stage became. It is empty
	// for every gate, which is how "typed gates create no worker runs" is
	// answerable from replayed state alone.
	RunID string `json:"run_id,omitempty"`
	// InvalidatedUnder is the revision an invalidation happened under, when it
	// happened under the revision the stage was performed under.
	InvalidatedUnder int `json:"invalidated_under,omitempty"`
	// Generation is which EXECUTION of this stage, under this revision, is
	// current. It advances when an already-approved stage has to be performed
	// again because the upstream candidate it consumed was replaced: that is an
	// execution fact, not a change to the approved plan, so the obligation is
	// renewed rather than re-planned. It is part of the run identity, so each
	// generation is its own #63 run.
	Generation int                   `json:"generation,omitempty"`
	Gate       *PlanGateSatisfaction `json:"gate,omitempty"`
}

// PlanApproval is the operator decision on one exact revision. Both the number
// and the digest are recorded: approving a revision number whose content could
// since have changed would be approving something nobody looked at.
type PlanApproval struct {
	Status   domain.ApprovalStatus `json:"status"`
	Revision int                   `json:"revision"`
	Digest   string                `json:"digest"`
	Operator string                `json:"operator,omitempty"`
	Note     string                `json:"note,omitempty"`
}

// PlanValidation is the latest deterministic verdict on the current revision.
type PlanValidation struct {
	Revision int                             `json:"revision"`
	Digest   string                          `json:"digest"`
	Status   domain.ProposalValidationStatus `json:"status"`
	Errors   []string                        `json:"errors,omitempty"`
}

// PlanSupersession records one revision replacing another.
type PlanSupersession struct {
	FromRevision      int      `json:"from_revision"`
	ToRevision        int      `json:"to_revision"`
	ProposalID        string   `json:"proposal_id"`
	InvalidatedStages []string `json:"invalidated_stages,omitempty"`
}

// PlanSnapshot is the replayed state of one plan.
type PlanSnapshot struct {
	PlanID   string `json:"plan_id"`
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
	// ObjectiveDigest identifies the objective without carrying it. The
	// objective is derived from untrusted issue text, so the journal - and this
	// projection of it - hold its identity while the readable text stays in the
	// plan revision document.
	ObjectiveDigest string `json:"objective_digest,omitempty"`
	// Reasoning is the provenance of the reasoning invocation that produced the
	// current revision, when one contributed. Its presence is what answers
	// "which registered agent planned this, and was the workspace verified".
	Reasoning *PlanReasoningPayload `json:"reasoning,omitempty"`
	// Approval is the LATEST decision state of the latest revision: a newly
	// proposed revision is pending, and that is what an operator is asked
	// about.
	Approval PlanApproval `json:"approval"`
	// Approved is the highest revision an operator actually approved, and it is
	// STICKY. Proposing a new revision does not un-approve the one that is
	// executing: the approved revision keeps governing until an operator
	// approves the replacement, which is what lets unaffected stages continue
	// while a proposal waits.
	Approved PlanApproval `json:"approved,omitzero"`
	// Rejected is every revision an operator turned down. It is a set rather
	// than a latest-decision field because rejecting a proposal is the ordinary
	// answer "keep the current plan", and without a durable record of it the
	// proposal stayed pending forever and paused every new stage of the plan.
	Rejected map[int]bool `json:"rejected,omitempty"`
	// RetiredRuns are child runs whose stage a revision invalidated. The stage
	// no longer names them - it starts again under the new revision - but a run
	// does not stop existing because the plan stopped looking at it, and what
	// it spends is still the plan's.
	RetiredRuns []string `json:"retired_runs,omitempty"`
	// Governed is every revision an operator approved, in the order approved.
	// A supersession replaces the revision that was GOVERNING, and a proposal
	// that was never approved never governed anything.
	Governed []int `json:"governed,omitempty"`
	// Validation is the LATEST verdict, for display. Validations is every
	// verdict BY REVISION, because a single slot meant a later revision's
	// verdict replaced an earlier refusal - and a decision that consults only
	// the latest record cannot see that the revision it is about was refused.
	Validation  PlanValidation                 `json:"validation,omitzero"`
	Validations map[int]PlanValidation         `json:"validations,omitempty"`
	Stages      map[string]PlanStageProjection `json:"stages"`
	Superseded  []PlanSupersession             `json:"superseded,omitempty"`
	// Consumed is summed from plan.budget_consumed events. It is a SUM over
	// immutable records, so nothing - not a revision, not a restart, not a
	// reassignment - can lower it.
	Consumed domain.PlanConsumption `json:"consumed"`
	// StageConsumed is the same sum PER STAGE. A stage's own budget - the one a
	// profile narrows - is a fact about that stage, so enforcing it needs the
	// stage's share rather than the plan's total.
	StageConsumed map[string]domain.PlanConsumption `json:"stage_consumed,omitempty"`
	// RunConsumed is what has already been attributed FOR EACH RUN, so the next
	// attribution is a delta rather than a repeat. A run keeps spending after
	// the stage settles - feedback re-activates it - and this is how that later
	// spend reaches the plan without counting the earlier spend twice.
	RunConsumed map[string]domain.PlanConsumption `json:"run_consumed,omitempty"`
	Cursor      Cursor                            `json:"journal_cursor"`
	// consumedKeys is the set of consumption keys already counted. It is not
	// exported and not part of the state digest: it is how the fold stays
	// idempotent, not a fact about the plan.
	consumedKeys map[string]bool `json:"-"`
	StateSHA256  string          `json:"state_sha256"`
}

// GoverningHistory is every revision an operator approved, in order. The
// approval events are the record of which plan was executing when.
func (s PlanSnapshot) GoverningHistory() []int { return append([]int(nil), s.Governed...) }

// PreviousGoverning is the revision that governed BEFORE the given one, which
// is what a supersession replaces. A revision that was proposed and never
// approved never governed, so it is not a predecessor of anything.
func (s PlanSnapshot) PreviousGoverning(revision int) (int, bool) {
	previous, found := 0, false
	for _, governed := range s.Governed {
		if governed < revision && governed > previous {
			previous, found = governed, true
		}
	}
	return previous, found
}

// ApprovedRevision reports the revision an operator approved, and whether one
// exists. A plan reconciler asks this before it creates anything: an unapproved
// plan executes nothing.
func (s PlanSnapshot) ApprovedRevision() (int, bool) {
	if s.Approved.Status != domain.ApprovalApproved {
		return 0, false
	}
	return s.Approved.Revision, true
}

// ChildRuns lists the EngineeringRuns this plan created, in stage order.
func (s PlanSnapshot) ChildRuns() []string {
	stages := make([]string, 0, len(s.Stages))
	for id := range s.Stages {
		stages = append(stages, id)
	}
	sort.Strings(stages)
	runs := make([]string, 0, len(stages))
	for _, id := range stages {
		if run := s.Stages[id].RunID; run != "" {
			runs = append(runs, run)
		}
	}
	return runs
}

// ReducePlan rebuilds plan state from its events. It is the plan's counterpart
// to Reduce, and it applies the same chain discipline: an unknown type, a gap
// in the sequence, a broken link or a tampered hash is refused rather than
// projected.
func ReducePlan(planID string, events []EngineeringEvent) (PlanSnapshot, error) {
	snapshot := PlanSnapshot{
		PlanID: planID,
		Stages: map[string]PlanStageProjection{},
		// An unwritten plan is PENDING approval. There is no state in which a
		// plan is approved by default.
		Approval: PlanApproval{Status: domain.ApprovalPending},
	}
	var previous EngineeringEvent
	for i, e := range events {
		if !planEventTypes[e.Type] {
			return snapshot, fmt.Errorf("event type %q does not belong to a plan stream", e.Type)
		}
		if e.PlanID != planID || e.RunID != "" || e.Sequence != int64(i+1) {
			return snapshot, fmt.Errorf("invalid plan event sequence")
		}
		if i > 0 && (e.PreviousEventID != previous.ID || e.PreviousEventHash != previous.EventHash) {
			return snapshot, fmt.Errorf("broken plan event chain")
		}
		hash, err := EventDigest(e)
		if err != nil || (e.EventHash != "" && e.EventHash != hash) {
			return snapshot, fmt.Errorf("invalid plan event hash")
		}
		e.EventHash = hash
		for _, artifact := range e.Artifacts {
			if err := ValidateArtifact(artifact); err != nil {
				return snapshot, err
			}
		}
		if err := snapshot.apply(e); err != nil {
			return snapshot, err
		}
		snapshot.Cursor = Cursor{e.Sequence, e.ID, e.EventHash}
		previous = e
	}
	digest, err := PlanStateDigest(snapshot)
	snapshot.StateSHA256 = digest
	return snapshot, err
}

// PlanStateDigest is the plan's state digest, computed the same way a run's is:
// over the canonical snapshot with the cursor and the digest field cleared, so
// an event's state_after never feeds back into the digest it records.
func PlanStateDigest(s PlanSnapshot) (string, error) {
	s.StateSHA256 = ""
	s.Cursor = Cursor{}
	return Digest(s)
}

func (s *PlanSnapshot) apply(e EngineeringEvent) error {
	switch e.Type {
	case EventPlanProposed:
		var payload PlanProposedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		s.Revision, s.Digest, s.ObjectiveDigest = payload.Revision, payload.Digest, payload.ObjectiveDigest
		s.Reasoning = payload.Reasoning
		// A NEW revision is not approved. Carrying the previous revision's
		// approval forward is exactly the silent plan replacement the approval
		// boundary exists to prevent.
		s.Approval = PlanApproval{Status: domain.ApprovalPending, Revision: payload.Revision, Digest: payload.Digest}
		s.Validation = PlanValidation{}
	case EventPlanValidated:
		var payload PlanValidatedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		verdict := PlanValidation{
			Revision: payload.Revision, Digest: payload.Digest,
			Status: domain.ProposalValidationStatus(payload.Status), Errors: payload.Errors,
		}
		s.Validation = verdict
		if s.Validations == nil {
			s.Validations = map[int]PlanValidation{}
		}
		s.Validations[payload.Revision] = verdict
	case EventPlanApproved, EventPlanRejected:
		var payload PlanDecisionPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		status := domain.ApprovalApproved
		if e.Type == EventPlanRejected {
			status = domain.ApprovalRejected
		}
		decision := PlanApproval{
			Status: status, Revision: payload.Revision, Digest: payload.Digest,
			Operator: payload.Operator, Note: payload.Note,
		}
		s.Approval = decision
		// An approval is sticky and monotonic. A REJECTION of a later revision
		// leaves the approved one exactly where it was: rejecting a proposal is
		// not a withdrawal of the approval the plan is already executing under.
		if status == domain.ApprovalApproved && payload.Revision >= s.Approved.Revision {
			s.Approved = decision
		}
		if status == domain.ApprovalApproved {
			known := false
			for _, governed := range s.Governed {
				known = known || governed == payload.Revision
			}
			if !known {
				s.Governed = append(s.Governed, payload.Revision)
			}
		}
		if status == domain.ApprovalRejected {
			if s.Rejected == nil {
				s.Rejected = map[int]bool{}
			}
			s.Rejected[payload.Revision] = true
		}
	case EventPlanStageAssigned:
		var payload PlanStageAssignedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		stage := s.stage(payload.StageID)
		stage.State = PlanStagePending
		stage.AssignmentID = payload.AssignmentID
		stage.Role = domain.EngineeringRole(payload.Role)
		stage.ProfileID, stage.ProfileVersion, stage.ProfileDigest = payload.ProfileID, payload.ProfileVersion, payload.ProfileDigest
		stage.AgentID, stage.ProviderKind = payload.AgentID, payload.ProviderKind
		stage.VendorFamily, stage.TrustMode = payload.VendorFamily, payload.TrustMode
		stage.InvocationMode = domain.InvocationMode(payload.InvocationMode)
		if payload.RunID != "" {
			stage.RunID = payload.RunID
		}
		s.Stages[payload.StageID] = stage
	case EventPlanRunStarted:
		var payload PlanRunStartedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		stage := s.stage(payload.StageID)
		// A gate never gets a run. Refusing here rather than recording it is
		// what keeps "typed gates create no worker runs" true of replayed state
		// and not only of the code that writes it.
		if stage.Gate != nil {
			return fmt.Errorf("stage %q is a satisfied gate and cannot own EngineeringRun %q", payload.StageID, payload.RunID)
		}
		stage.RunID = payload.RunID
		stage.State = PlanStageRunning
		// A stage that is RUNNING is not invalidated and has no reason. Both
		// belong to the performance that ended, and leaving them on a healthy
		// new generation showed operators an `invalidated_under` beside work
		// that is under way.
		stage.InvalidatedUnder = 0
		stage.Reason = ""
		s.Stages[payload.StageID] = stage
	case EventPlanStageSettled:
		var payload PlanStageSettledPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		stage := s.stage(payload.StageID)
		switch payload.Outcome {
		case string(Completed):
			stage.State = PlanStageCompleted
		case string(Failed):
			stage.State = PlanStageFailed
		default:
			stage.State = PlanStageInvalidated
			if payload.Revision > 0 {
				// Invalidated UNDER the revision it was performed under. The
				// run it named is retired - it does not stop existing because
				// the plan stopped looking at it, and if it is still executing
				// it is executing work that has been discarded - and the stage
				// no longer names it, so nothing settles the stage from a run
				// that was stopped on purpose. The revision is remembered,
				// because a stage cannot be performed again under it.
				s.retire(stage.RunID)
				s.Stages[payload.StageID] = PlanStageProjection{
					StageID: payload.StageID, State: PlanStageInvalidated,
					Reason: payload.Reason, InvalidatedUnder: payload.Revision,
					// The next performance of this stage is a new execution
					// generation: a new assignment naming what it will actually
					// consume, and a new run, under the same approved plan.
					Generation: stage.Generation + 1,
				}
				return nil
			}
		}
		stage.Reason = payload.Reason
		s.Stages[payload.StageID] = stage
	case EventPlanGateSatisfied:
		var payload PlanGateSatisfiedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		stage := s.stage(payload.StageID)
		if stage.RunID != "" {
			return fmt.Errorf("stage %q owns EngineeringRun %q and cannot be satisfied as a gate", payload.StageID, stage.RunID)
		}
		stage.State = PlanStageSatisfied
		stage.Gate = &PlanGateSatisfaction{
			Kind: domain.StageKind(payload.Kind), Claims: payload.Claims,
			Evidence: payload.Evidence, Decision: payload.Decision, HumanEvidenceID: payload.HumanEvidenceID,
			ProvenHeads: payload.ProvenHeads,
		}
		s.Stages[payload.StageID] = stage
	case EventPlanBudgetConsumed:
		var payload PlanBudgetConsumedPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		if payload.Key != "" {
			if s.consumedKeys == nil {
				s.consumedKeys = map[string]bool{}
			}
			if s.consumedKeys[payload.Key] {
				// Already counted. The reconciler re-derives the same work
				// every tick, so a crash between two appends replays a delta
				// that was already recorded - and consumption that can only go
				// up must not go up twice for one thing.
				return nil
			}
			s.consumedKeys[payload.Key] = true
		}
		if payload.StageID != "" {
			if s.StageConsumed == nil {
				s.StageConsumed = map[string]domain.PlanConsumption{}
			}
			stage := s.StageConsumed[payload.StageID]
			stage.ChildRuns += payload.ChildRuns
			stage.ProviderInvocations += payload.ProviderInvocations
			stage.WallSeconds += payload.WallSeconds
			s.StageConsumed[payload.StageID] = stage
		}
		if payload.RunID != "" {
			if s.RunConsumed == nil {
				s.RunConsumed = map[string]domain.PlanConsumption{}
			}
			run := s.RunConsumed[payload.RunID]
			run.ChildRuns += payload.ChildRuns
			run.ProviderInvocations += payload.ProviderInvocations
			run.WallSeconds += payload.WallSeconds
			s.RunConsumed[payload.RunID] = run
		}
		s.Consumed.ChildRuns += payload.ChildRuns
		s.Consumed.ProviderInvocations += payload.ProviderInvocations
		s.Consumed.WallSeconds += payload.WallSeconds
		if payload.CostKnown && payload.CostMicros != nil {
			// Cost becomes KNOWN the first time anything reports it, and the
			// total is the sum of what was reported. An unreported invocation
			// contributes nothing rather than a zero, so "partly known" stays
			// distinguishable from "known to be free".
			total := *payload.CostMicros
			if s.Consumed.CostMicros != nil {
				total += *s.Consumed.CostMicros
			}
			s.Consumed.CostMicros = &total
			s.Consumed.CostKnown = true
		}
	case EventPlanRevisionSuperseded:
		var payload PlanRevisionSupersededPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return err
		}
		s.Superseded = append(s.Superseded, PlanSupersession{
			FromRevision: payload.FromRevision, ToRevision: payload.ToRevision,
			ProposalID: payload.ProposalID, InvalidatedStages: payload.InvalidatedStages,
		})
		// Only the stages the proposal named are invalidated. Invalidating
		// everything downstream "to be safe" would discard completed work whose
		// assumptions still hold, which is the opposite of what the revision
		// laws ask for.
		for _, id := range payload.InvalidatedStages {
			// The stage starts again under the new revision, so its lifecycle
			// state from the old one is cleared rather than carried: a
			// revision may legitimately change what a stage IS - policy
			// permits an agent stage to become a human decision gate - and a
			// preserved gate would refuse the replacement's run event, while a
			// preserved run id would refuse its gate event.
			//
			// The RUN, though, does not stop existing because the plan stopped
			// looking at it. An invalidated stage's child run can still be
			// live, and what it spends is still the plan's - so the run id is
			// retained here, apart from the stage, and the reconciler keeps
			// attributing it.
			if retired := s.Stages[id].RunID; retired != "" {
				s.retire(retired)
			}
			s.Stages[id] = PlanStageProjection{
				StageID: id,
				State:   PlanStageInvalidated,
				Reason:  "superseded by revision " + fmt.Sprint(payload.ToRevision),
			}
		}
	}
	return nil
}

// retire records a child run whose stage no longer names it. It is idempotent:
// the same run retired twice is one retirement.
func (s *PlanSnapshot) retire(runID string) {
	if runID == "" {
		return
	}
	for _, known := range s.RetiredRuns {
		if known == runID {
			return
		}
	}
	s.RetiredRuns = append(s.RetiredRuns, runID)
}

func (s *PlanSnapshot) stage(id string) PlanStageProjection {
	stage, ok := s.Stages[id]
	if !ok {
		return PlanStageProjection{StageID: id, State: PlanStagePending}
	}
	return stage
}
