// Package runtime implements the local, deterministic operational layer above
// the authorization kernel. It intentionally contains no provider authority.
package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const SchemaVersion = "0.1"

// Run-mode exits are stable before the live GitHub runner is introduced.
const (
	ExitCompleted = 0
	ExitWaiting   = 10
	ExitFailed    = 11
	ExitCancelled = 12
	ExitInvalid   = 64
)

type Disposition string

const (
	Active    Disposition = "active"
	Waiting   Disposition = "waiting"
	Completed Disposition = "completed"
	Failed    Disposition = "failed"
	Cancelled Disposition = "cancelled"
)

type Phase string

const (
	Contract  Phase = "contract"
	Execute   Phase = "execute"
	Observe   Phase = "observe"
	Assure    Phase = "assure"
	Authorize Phase = "authorize"
	Remediate Phase = "remediate"
	Publish   Phase = "publish"
)

type OperationState string

const (
	Pending            OperationState = "pending"
	Leased             OperationState = "leased"
	Running            OperationState = "running"
	Succeeded          OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationCancelled OperationState = "cancelled"
	Unknown            OperationState = "unknown"
)

const (
	EventRunCreated = "run.created"
	// EventRunAgentAssigned is the durable, immutable record of WHICH named
	// execution agent a run was created to be worked by. The run row also
	// carries the id, but the row is mutable and this is not: the journal is
	// where "this run has always been a codex run" is answered, including
	// after the operator's default agent changes underneath it.
	EventRunAgentAssigned = "run.agent_assigned"
	// EventRunAgentHandoffRefused records an attempt to move an existing run
	// to a different execution agent, and the typed refusal it produced. It is
	// journalled rather than merely returned because a refusal is an
	// engineering fact about the run: it names the exact candidate, the two
	// agents, the two trust modes, the reason and the budgets that were NOT
	// reset, so an operator can explain later why the work continued where it
	// did.
	EventRunAgentHandoffRefused = "run.agent_handoff_refused"
	// EventFeedbackObserved is one ADMISSION decision about one piece of
	// GitHub feedback: who wrote it, what permission they hold, whether it
	// applies to the current head, and whether it may reach a worker. It is
	// recorded for refused items too, because "why was my comment ignored" is
	// an operator question the runtime has to be able to answer.
	EventFeedbackObserved = "feedback.observed"
	// EventFeedbackConsumed is DELIVERY: admitted items were actually given to
	// an exact invocation. It is a separate event from admission so a crash
	// between observing a review and acting on it can neither lose it nor
	// deliver it twice.
	EventFeedbackConsumed = "feedback.consumed"
	// EventFeedbackPublicationIdentity binds the account the runtime publishes
	// as. The self-loop guard refuses feedback authored by that identity, so
	// the binding has to be durable: a credential rotated while `serve` is
	// alive would otherwise leave the guard recognizing an account the runtime
	// no longer is, and admitting its own comments as somebody else's.
	EventFeedbackPublicationIdentity = "feedback.publication_identity"
	EventRunWaiting                  = "run.waiting"
	EventRunCompleted                = "run.completed"
	EventRunFailed                   = "run.failed"
	EventRunCancelled                = "run.cancelled"
	EventSourceIntentChanged         = "source.intent_changed"
	EventSourceOptInRemoved          = "source.opt_in_removed"
	EventSourceOptInRestored         = "source.opt_in_restored"
	EventOperationPlanned            = "operation.planned"
	EventOperationBefore             = "operation.before"
	EventOperationAfter              = "operation.after"
	EventCandidateChanged            = "candidate.changed"
	EventCandidateCommitted          = "candidate.committed"
	// EventCandidateCheckpointed is a runtime-owned commit of work an
	// interrupted producer left behind. It is deliberately NOT
	// candidate.committed: every reader of that event treats it as an
	// execution-complete, assurance-eligible candidate, and a checkpoint is
	// neither. See RunProjection.CandidateComplete.
	EventCandidateCheckpointed = "candidate.checkpointed"
	// EventExecutionCompleted records that a producer invocation ended by
	// finishing, rather than by running out of a bound. It is a producer
	// COMPLETION OBSERVATION and nothing more: it asserts no acceptance, no
	// evidence and no authority.
	EventExecutionCompleted       = "execution.completed"
	EventCandidateBaseIntegrated  = "candidate.base_integrated"
	EventCandidateExternalChanged = "candidate.external_changed"
	EventContractCompiled         = "contract.compiled"
	EventReassessmentCompleted    = "reassessment.completed"
	EventAssuranceObserved        = "assurance.observed"
	// EventSemanticAssuranceObserved is the INDEPENDENT semantic verifier's own
	// observation. It is a distinct event because it is a distinct producer
	// answering a distinct question: every reader that means "the automated
	// verifier judged this tree" keeps meaning exactly that.
	EventSemanticAssuranceObserved = "assurance.semantic_observed"
	EventAuthorityEvaluated        = "authority.evaluated"
	EventGitHubCIObserved          = "github.ci_observed"
	EventGitHubReviewObserved      = "github.review_observed"
	EventGitHubPRObserved          = "github.pr_observed"
	EventHumanAuthorityRecorded    = "human.authority_recorded"

	// The plan lifecycle. These events belong to a PLAN stream rather than a
	// run stream, and they live in the same append-only journal, in the same
	// table, under the same hash chain: #64 forbids a second event journal, and
	// two journals would be two answers to "what happened to this work".
	//
	// A plan event never carries a run id and a run event never carries a plan
	// id. The association between them is stated explicitly by
	// plan.run_started, so "which run belongs to which stage" is a recorded
	// fact rather than something a reader infers from two streams.

	// EventPlanProposed is a plan revision offered for approval. It is the only
	// event that introduces a revision, and it never implies approval: a
	// proposal that executed because it parsed is exactly what the approval
	// boundary exists to prevent.
	EventPlanProposed = "plan.proposed"
	// EventPlanValidated is the deterministic validator's verdict on a
	// revision. A refusal is journalled rather than only returned: refusing
	// quietly would lose the reason a reasoning agent asked for something it
	// may not have.
	EventPlanValidated = "plan.validated"
	// EventPlanApproved and EventPlanRejected are the operator's answer. The
	// approval is authority to EXECUTE within existing policy and permission
	// ceilings, and is never merge, release or acceptance authority.
	EventPlanApproved = "plan.approved"
	EventPlanRejected = "plan.rejected"
	// EventPlanStageAssigned freezes which profile, which underlying agent and
	// which invocation mode an executable stage resolved to. Editing the
	// profile afterwards cannot rewrite this: the digests are recorded here.
	EventPlanStageAssigned = "plan.stage_assigned"
	// EventPlanRunStarted associates one agent stage with one ordinary #63
	// EngineeringRun. Only agent stages ever produce one of these.
	EventPlanRunStarted = "plan.run_started"
	// EventPlanStageSettled records how a stage ended: completed, failed, or
	// invalidated by an upstream change.
	EventPlanStageSettled = "plan.stage_settled"
	// EventPlanGateSatisfied records that a typed gate's existing durable
	// references - evidence, authority decision, human decision - now prove it.
	// A gate is never satisfied by a worker run, because it never creates one.
	EventPlanGateSatisfied = "plan.gate_satisfied"
	// EventPlanBudgetConsumed is a DELTA against the plan's aggregate envelope.
	// Consumption is a projection summed from these events rather than a stored
	// counter, which is what makes "a revision, restart or reassignment cannot
	// reset consumed budget" a property of the representation instead of a rule
	// somebody has to remember.
	EventPlanBudgetConsumed = "plan.budget_consumed"
	// EventPlanRevisionSuperseded records one approved revision replacing
	// another, and exactly which downstream stages that invalidated.
	EventPlanRevisionSuperseded = "plan.revision_superseded"
)

var eventTypes = map[string]bool{EventPlanProposed: true, EventPlanValidated: true, EventPlanApproved: true, EventPlanRejected: true, EventPlanStageAssigned: true, EventPlanRunStarted: true, EventPlanStageSettled: true, EventPlanGateSatisfied: true, EventPlanBudgetConsumed: true, EventPlanRevisionSuperseded: true, EventRunCreated: true, EventRunAgentAssigned: true, EventRunAgentHandoffRefused: true, EventFeedbackObserved: true, EventFeedbackConsumed: true, EventFeedbackPublicationIdentity: true, EventRunWaiting: true, EventRunCompleted: true, EventRunFailed: true, EventRunCancelled: true, EventSourceIntentChanged: true, EventSourceOptInRemoved: true, EventSourceOptInRestored: true, EventOperationPlanned: true, EventOperationBefore: true, EventOperationAfter: true, EventCandidateChanged: true, EventCandidateCommitted: true, EventCandidateCheckpointed: true, EventExecutionCompleted: true, EventCandidateBaseIntegrated: true, EventCandidateExternalChanged: true, EventContractCompiled: true, EventReassessmentCompleted: true, EventAssuranceObserved: true, EventSemanticAssuranceObserved: true, EventAuthorityEvaluated: true, EventGitHubCIObserved: true, EventGitHubReviewObserved: true, EventGitHubPRObserved: true, EventHumanAuthorityRecorded: true}

// planEventTypes is the plan stream's own vocabulary. It exists so an event
// cannot be appended to the wrong stream: a plan event in a run's hash chain
// would put a plan's history inside a run's identity, and a run event in a
// plan's chain would make a plan's state digest depend on work it does not own.
var planEventTypes = map[string]bool{
	EventPlanProposed: true, EventPlanValidated: true, EventPlanApproved: true,
	EventPlanRejected: true, EventPlanStageAssigned: true, EventPlanRunStarted: true,
	EventPlanStageSettled: true, EventPlanGateSatisfied: true, EventPlanBudgetConsumed: true,
	EventPlanRevisionSuperseded: true,
}

type Ref struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type Candidate struct {
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}
type Cursor struct {
	LastSequence  int64  `json:"last_sequence"`
	LastEventID   string `json:"last_event_id"`
	LastEventHash string `json:"last_event_hash,omitempty"`
}
type Artifact struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	MediaType   string `json:"media_type"`
	LocalOnly   bool   `json:"local_only"`
	Sanitized   bool   `json:"sanitized"`
	Publishable bool   `json:"publishable"`
}
type EngineeringRun struct {
	SchemaVersion    string      `json:"schema_version"`
	ID               string      `json:"id"`
	Repository       string      `json:"repository"`
	Goal             string      `json:"goal"`
	Phase            Phase       `json:"phase"`
	Disposition      Disposition `json:"disposition"`
	Reason           string      `json:"reason,omitempty"`
	Base             Ref         `json:"base"`
	Candidate        Candidate   `json:"candidate"`
	Contract         Ref         `json:"contract"`
	ControllerSHA256 string      `json:"controller_sha256"`
	// Budgets are the bounds this run was created under. It is a POINTER
	// because encoding/json does not honour omitempty on a struct: a value
	// field would emit "budgets":{...zeroes...} for every run that predates it,
	// changing the canonical document - and therefore the state digest - of
	// runs already on disk. Absent must stay absent.
	//
	// Nil is a run created before this field existed, and
	// runState.continuationLimit reads that absence as the legacy rule rather
	// than as today's configuration.
	Budgets *RunBudgets `json:"budgets,omitempty"`
	// AgentID is the named execution agent this run is worked by. It is a
	// PROJECTION of the run.agent_assigned event, kept on the row so the
	// all-runs operator view can be answered without replaying every journal;
	// the journal remains the authority, exactly as it is for the candidate
	// revision this row also carries.
	//
	// It is omitempty because a run created before the agent registry existed
	// has none, and that absence has a documented legacy meaning: the run was
	// worked by whichever single provider the operator configuration named at
	// the time. No identity is backfilled for it.
	AgentID string `json:"agent_id,omitempty"`
	// Plan binds this run to the plan stage that created it, when a plan did.
	// It is a POINTER with omitempty for the same reason Budgets is: a run that
	// no plan created must canonicalize exactly as it did before plans existed.
	//
	// It carries IDENTITIES and nothing else. The stage objective, the context
	// pack and the frozen profile configuration live in the durable
	// AgentAssignment this points at, so the run row cannot drift from the
	// assignment an operator approved.
	Plan      *RunPlanBinding `json:"plan,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Cursor    Cursor          `json:"journal_cursor"`
}

// RunPlanBinding is the durable link from an ordinary EngineeringRun back to
// the plan stage that created it.
//
// Only `agent` stages ever produce one. A gate creates no run at all, so no
// gate can appear here - which is how "typed gates create no worker runs" stays
// answerable from the run row as well as from the plan journal.
type RunPlanBinding struct {
	PlanID       string `json:"plan_id"`
	Revision     int    `json:"revision"`
	PlanDigest   string `json:"plan_digest"`
	StageID      string `json:"stage_id"`
	AssignmentID string `json:"assignment_id"`
	// BaseRevision is the UPSTREAM candidate this stage builds on, when its
	// dependencies produced and published one. A reviewing or integrating stage
	// based on the trusted branch would have nothing to review or integrate;
	// this is what makes its workspace contain the work.
	//
	// It is only ever set to a published commit, because the governed remote is
	// the only thing a candidate is cloned from. Where the upstream work is not
	// published, the field is empty and the stage is based on the trusted base
	// with the upstream diff supplied as context instead.
	BaseRevision string `json:"base_revision,omitempty"`
	// Generation is which EXECUTION of this stage the run performs. It advances
	// when an already-approved stage is performed again because the upstream
	// candidate it consumed was replaced - an execution fact, not a change to
	// the approved plan - and it is part of the run identity, so each
	// generation is its own run rather than an adoption of the previous one.
	//
	// omitempty and zero for every first performance, so no existing run is
	// re-identified.
	Generation int `json:"generation,omitempty"`
	// StageBudget is the plan stage's own bound, already narrowed by the
	// assigned profile's constraints. The run persists its budgets narrowed by
	// this, which is what makes a profile's `max_wall_seconds` and
	// `max_execution_attempts` bind the work instead of being recorded and
	// ignored.
	//
	// omitempty, because a run created before it existed must canonicalize
	// exactly as it did: the genesis event is hashed against the run document.
	StageBudget domain.StageBudget `json:"stage_budget,omitzero"`
}

type RunOperation struct {
	SchemaVersion    string          `json:"schema_version"`
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	Kind             string          `json:"kind"`
	IdempotencyKey   string          `json:"idempotency_key"`
	State            OperationState  `json:"state"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"max_attempts"`
	DependsOn        []string        `json:"depends_on,omitempty"`
	InputStateSHA256 string          `json:"input_state_sha256"`
	Lease            *Lease          `json:"lease,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	CreatedAt        time.Time       `json:"created_at,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	LastProgressAt   *time.Time      `json:"last_progress_at,omitempty"`
	WallBudget       time.Duration   `json:"wall_budget,omitempty"`
	NoProgressBudget time.Duration   `json:"no_progress_budget,omitempty"`
	NoProgressKey    string          `json:"no_progress_key,omitempty"`
	CancelRequested  bool            `json:"cancel_requested,omitempty"`
}
type Lease struct {
	Owner       string    `json:"owner"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type EngineeringEvent struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	RunID         string `json:"run_id"`
	// PlanID names the plan stream this event belongs to, and is empty for
	// every run event.
	//
	// omitempty is load-bearing rather than tidy: a run event's canonical
	// document is what its hash chain, its state digests and - through them -
	// its run identity are computed over. A member that appeared as an empty
	// string would re-identify every event ever written.
	PlanID            string          `json:"plan_id,omitempty"`
	Sequence          int64           `json:"sequence"`
	Type              string          `json:"type"`
	OccurredAt        time.Time       `json:"occurred_at"`
	OperationID       string          `json:"operation_id,omitempty"`
	PreviousEventID   string          `json:"previous_event_id,omitempty"`
	PreviousEventHash string          `json:"previous_event_hash,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Artifacts         []Artifact      `json:"artifacts,omitempty"`
	StateBefore       string          `json:"state_before,omitempty"`
	StateAfter        string          `json:"state_after,omitempty"`
	EventHash         string          `json:"event_hash,omitempty"`
}
type RunSnapshot struct {
	EngineeringRun
	Operations  map[string]RunOperation `json:"operations"`
	Artifacts   []Artifact              `json:"artifacts,omitempty"`
	StateSHA256 string                  `json:"state_sha256"`
}

// CanonicalJSON serializes a typed runtime value to JSON, then applies RFC 8785
// JSON Canonicalization Scheme (JCS).
//
// It delegates to domain.CanonicalJSON so the runtime journal, the kernel
// contracts and the M2 planning artifacts are canonicalized by exactly ONE
// implementation. Two canonicalizers would be two definitions of "the same
// document", which is the one thing a hash chain cannot tolerate.
func CanonicalJSON(v any) ([]byte, error) { return domain.CanonicalJSON(v) }

// Digest is the SHA-256 of the canonical document.
func Digest(v any) (string, error) { return domain.Digest(v) }

func StateDigest(s RunSnapshot) (string, error) {
	s.StateSHA256 = ""
	s.Cursor = Cursor{}
	return Digest(s)
}
func EventDigest(e EngineeringEvent) (string, error) { e.EventHash = ""; return Digest(e) }
func ValidateArtifact(a Artifact) error {
	if !a.Sanitized && !a.LocalOnly {
		return fmt.Errorf("raw artifact %q must be local-only", a.Path)
	}
	if a.Publishable && !a.Sanitized {
		return fmt.Errorf("raw artifact %q is not publishable", a.Path)
	}
	return nil
}
func Reduce(run EngineeringRun, events []EngineeringEvent) (RunSnapshot, error) {
	s := RunSnapshot{EngineeringRun: run, Operations: map[string]RunOperation{}}
	var prev EngineeringEvent
	for i, e := range events {
		if !eventTypes[e.Type] {
			return s, fmt.Errorf("unknown event type %q", e.Type)
		}
		if e.RunID != run.ID || e.Sequence != int64(i+1) {
			return s, fmt.Errorf("invalid event sequence")
		}
		if i > 0 && (e.PreviousEventID != prev.ID || e.PreviousEventHash != prev.EventHash) {
			return s, fmt.Errorf("broken event chain")
		}
		h, err := EventDigest(e)
		if err != nil || (e.EventHash != "" && e.EventHash != h) {
			return s, fmt.Errorf("invalid event hash")
		}
		e.EventHash = h
		for _, a := range e.Artifacts {
			if err := ValidateArtifact(a); err != nil {
				return s, err
			}
			s.Artifacts = append(s.Artifacts, a)
		}
		if e.Type == EventOperationPlanned || e.Type == EventOperationBefore || e.Type == EventOperationAfter {
			var operation RunOperation
			if err := json.Unmarshal(e.Payload, &operation); err != nil || operation.ID == "" || operation.RunID != run.ID {
				return s, fmt.Errorf("invalid operation lifecycle payload")
			}
			if e.OperationID != "" && e.OperationID != operation.ID {
				return s, fmt.Errorf("operation event identity mismatch")
			}
			s.Operations[operation.ID] = operation
		}
		if e.Type == EventRunWaiting {
			s.Disposition = Waiting
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunCompleted {
			s.Disposition = Completed
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunFailed {
			s.Disposition = Failed
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunCancelled {
			s.Disposition = Cancelled
			s.Reason = payloadReason(e.Payload)
		}
		s.Cursor = Cursor{e.Sequence, e.ID, e.EventHash}
		prev = e
	}
	d, err := StateDigest(s)
	s.StateSHA256 = d
	return s, err
}
func payloadReason(raw json.RawMessage) string {
	var v struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Reason
}
func StableOperationKey(runID, kind string, bindings ...string) string {
	sort.Strings(bindings)
	return strings.Join(append([]string{runID, kind}, bindings...), ":")
}
func CanAcquire(op RunOperation, now time.Time, ownerAlive bool) bool {
	if op.State == Succeeded || op.State == OperationCancelled {
		return false
	}
	return op.Lease == nil || (!ownerAlive && !now.Before(op.Lease.ExpiresAt))
}

// OperationElapsed reports elapsed time only for an actively started operation.
func OperationElapsed(op RunOperation, now time.Time) time.Duration {
	if op.StartedAt == nil || now.Before(*op.StartedAt) {
		return 0
	}
	return now.Sub(*op.StartedAt)
}

// NoProgressExceeded is deliberately separate from wall time: a heartbeat can
// prove liveness while its unchanged progress fingerprint still exhausts the
// operation's forward-progress budget.
func NoProgressExceeded(op RunOperation, now time.Time) bool {
	return op.NoProgressBudget > 0 && op.LastProgressAt != nil && !now.Before(*op.LastProgressAt) && now.Sub(*op.LastProgressAt) > op.NoProgressBudget
}
