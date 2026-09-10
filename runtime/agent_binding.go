package runtime

// Which execution agent works a run is durable run PROVENANCE, not a live
// setting. Two rules follow from that, and this file is where both are stated.
//
// A run is bound to its agent when it is created, and the binding is journalled
// rather than only written to the mutable run row. An operator who changes
// their default agent afterwards changes what NEW work uses; the runs already
// in flight keep being worked by the agent they were started with, and a replay
// of the journal alone says which one that was.
//
// Moving an existing run to a different agent is therefore a governed
// TRANSITION and never a registry swap. #63 implements the refusal side of that
// transition: the runtime records a typed handoff record carrying everything a
// successor would have needed, refuses, and tells the operator to start a new
// generation instead. That is a deliberate scope boundary rather than an
// oversight - see AgentHandoffRefusedError.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// AgentAssignedPayload is the immutable record of a run's execution agent.
type AgentAssignedPayload struct {
	AgentID      string    `json:"agent_id"`
	ProviderKind string    `json:"provider_kind"`
	TrustMode    TrustMode `json:"trust_mode"`
	// Model is the model the agent was configured with at assignment time. It
	// is provenance, not a binding: a CLI may resolve a different one, and the
	// invocation provenance on each attempt is what says what actually ran.
	Model string `json:"model,omitempty"`
}

// BudgetDimension is one remaining allowance, with KNOWN stated separately
// from the number.
//
// It exists because "not reported" and "zero" are opposite facts and a bare
// int cannot hold both. A subscription CLI reports no token count and no
// monetary cost; recording those as 0 would say the run has spent nothing and
// may spend nothing, neither of which is true. Known=false is the honest shape,
// and it survives into the handoff record rather than being flattened on the
// way there.
type BudgetDimension struct {
	Known     bool  `json:"known"`
	Remaining int64 `json:"remaining,omitempty"`
}

// UnknownBudget is the value for a dimension this configuration cannot observe.
func UnknownBudget() BudgetDimension { return BudgetDimension{} }

// KnownBudget is a dimension the runtime itself owns and can count.
func KnownBudget(remaining int64) BudgetDimension {
	return BudgetDimension{Known: true, Remaining: remaining}
}

// RemainingBudgets is the cumulative allowance a transition must carry
// forward unchanged. Budgets do not reset on a provider change; that is the
// whole point of recording them here.
type RemainingBudgets struct {
	WallSeconds       BudgetDimension `json:"wall_seconds"`
	ExecutionAttempts BudgetDimension `json:"execution_attempts"`
	// ProviderInvocations is the RUN TOTAL left - every execution invocation of
	// every binding, counted together - which is a different bound from the
	// per-binding retry allowance above it. A record that carried only the
	// retry allowance described itself as the complete cumulative allowance
	// while omitting the one ceiling a successor cannot recover by starting a
	// new binding.
	ProviderInvocations BudgetDimension `json:"provider_invocations"`
	RemediationAttempts BudgetDimension `json:"remediation_attempts"`
	AssuranceAttempts   BudgetDimension `json:"assurance_attempts"`
	Continuations       BudgetDimension `json:"continuations"`
	// Tokens and Cost are provider-reported and are frequently UNKNOWN. A
	// subscription CLI exposes neither, and the runtime never converts that
	// into zero.
	Tokens BudgetDimension `json:"tokens"`
	Cost   BudgetDimension `json:"cost_micros"`
}

// AgentHandoffRecord is the typed transition. Every member exists because a
// successor provider - or a human explaining what happened - would need it to
// reconstruct the state the transition was attempted from.
//
// It is deliberately identity and counts only: no untrusted text, no provider
// session material, no candidate content. It has to fit a canonical event
// payload, and it has to be safe to read back years later.
type AgentHandoffRecord struct {
	RunID string `json:"run_id"`
	// Predecessor names the exact invocation the run was last worked by.
	PredecessorOperationID string `json:"predecessor_operation_id,omitempty"`
	PredecessorAttempt     int    `json:"predecessor_attempt,omitempty"`
	// Candidate is the exact tree the successor would have inherited. A
	// handoff that could not name it would not be a handoff.
	CandidateRevision string `json:"candidate_revision,omitempty"`
	CandidateTree     string `json:"candidate_tree,omitempty"`
	// From and To are the two agents. From is empty for a run created before
	// the agent registry existed, which is the documented legacy meaning
	// rather than an invented identity.
	From AgentIdentity `json:"from,omitzero"`
	To   AgentIdentity `json:"to"`
	// Reason is the operator's stated cause. It is required: an unexplained
	// provider change is exactly the silent swap this type exists to prevent.
	Reason string `json:"reason"`
	// Contract and Config are the governance identity in force. A transition
	// may not change either, so recording them is what makes "unchanged"
	// checkable afterwards.
	Contract Ref          `json:"contract,omitzero"`
	Config   ConfigDigest `json:"config"`
	// Budgets are cumulative and do NOT reset.
	Budgets RemainingBudgets `json:"budgets"`
	// EvidenceBundles and Authority are the acceptance context the successor
	// would have inherited. A provider switch is never itself evidence.
	EvidenceBundles int                    `json:"evidence_bundles"`
	Authority       domain.AuthorityStatus `json:"authority_status,omitempty"`
	// FeedbackConsumed is how much admitted GitHub feedback the predecessor
	// has already been given. It is a COUNT plus the durable keys' digest
	// rather than the keys themselves, so the record stays bounded while still
	// binding the exact consumption state a successor must not replay.
	FeedbackConsumed       int    `json:"feedback_consumed"`
	FeedbackConsumedDigest string `json:"feedback_consumed_digest,omitempty"`
	// ProviderSessionRetired records that the predecessor's provider-native
	// session metadata, if any existed, is closed by this transition and is
	// never reinterpreted as the successor's live session. Continuity comes
	// from the candidate, the contract, the admitted feedback and the journal.
	ProviderSessionRetired bool `json:"provider_session_retired"`
	// Refused and RefusalCode state the outcome. #63 always refuses; the
	// members are explicit so a future in-run handoff records the same shape
	// with Refused false rather than needing a second event type.
	//
	// The code is a CLOSED vocabulary rather than a sentence. A durable payload
	// carries identity and classification; the operator-facing explanation is
	// derived from the code by AgentHandoffRefusedError, so improving the
	// wording later does not rewrite history.
	Refused     bool               `json:"refused"`
	RefusalCode HandoffRefusalCode `json:"refusal_code,omitempty"`
	RecordedAt  string             `json:"recorded_at"`
}

// HandoffRefusalCode is why a transition was refused.
type HandoffRefusalCode string

const (
	// HandoffNotImplemented is #63's scope boundary: the transition could be
	// described completely and still was not performed.
	HandoffNotImplemented HandoffRefusalCode = "in_run_handoff_not_implemented"
	// HandoffTrustDowngrade is the privilege law: a run governed under
	// protected execution is not continued by an operator_trusted worker.
	HandoffTrustDowngrade HandoffRefusalCode = "protected_trust_required"
)

// AgentHandoffRefusedError is the typed refusal for moving a live run to a
// different execution agent.
//
// #63 implements the refusal, not the transition, and that is a decision rather
// than a gap. A safe in-run handoff has to re-resolve the successor's effective
// grant against the new provider's capabilities, carry cumulative budgets
// forward, retire the predecessor's provider session without letting the
// successor inherit it, and present already-admitted feedback as history rather
// than as new input. Building all of that to serve a case an operator can
// already express - start a new generation with the agent they want - would be
// speculative machinery around a rare event. Refusing instead keeps the
// invariant that matters: an EngineeringRun's provider never changes silently,
// and never widens privilege by changing.
type AgentHandoffRefusedError struct {
	Record AgentHandoffRecord
}

func (e *AgentHandoffRefusedError) Error() string {
	from := e.Record.From.AgentID
	if from == "" {
		from = "the provider this run was created with"
	}
	return "refused to move run " + e.Record.RunID + " from " + from + " to " + e.Record.To.AgentID +
		": " + handoffRefusalGuidance(e.Record)
}

// handoffRefusalGuidance renders the operator-facing explanation from the
// durable code. The guidance is generated, never stored: a journalled record
// keeps meaning the same thing after the wording improves.
func handoffRefusalGuidance(record AgentHandoffRecord) string {
	switch record.RefusalCode {
	case HandoffTrustDowngrade:
		return "this run was created under protected execution, and a protected requirement is never satisfied by an operator_trusted worker. " +
			"Changing provider must not lower the trust the run was governed under"
	default:
		return "in-run provider handoff is not implemented. The candidate, budgets, evidence and admitted feedback recorded in this transition " +
			"would have to be carried forward and the successor's grant re-resolved, and this runtime refuses rather than switching provider without doing so. " +
			"Start a new generation with the agent you want (`autonomy run issue N --agent " + record.To.AgentID + " --new-generation`); " +
			"the existing run and its journal are left untouched"
	}
}

// TrustDowngradeRefused reports the specific refusal an operator most needs to
// see distinctly: a run whose policy requires protected execution cannot be
// continued by an operator_trusted worker.
func (r AgentHandoffRecord) TrustDowngradeRefused() bool {
	return r.From.TrustMode == TrustProtected && r.To.TrustMode == TrustOperatorTrusted
}

// recordedAgent replays the agent this run was created to be worked by. A run
// created before the registry existed reads back as the zero identity, which is
// its documented legacy meaning: the single provider the operator configuration
// named at the time. No identity is invented for it.
//
// An assignment that IS present and cannot be read is a different thing
// entirely, and it is an error rather than a zero identity. The zero identity
// means "unbound", and unbound is a permission here, not an absence: the
// controller backfills a binding onto an unbound run from whichever agent is
// running now, and a zero From makes TrustDowngradeRefused false. An
// unreadable assignment answered as "unbound" would therefore let a corrupt
// event rebind a protected run to an operator-trusted worker and record the
// rebinding as legitimate.
func (s *runState) recordedAgent() (AgentIdentity, error) {
	for _, event := range s.events {
		if event.Type != EventRunAgentAssigned {
			continue
		}
		if len(event.Payload) == 0 {
			// A journal that recorded the event with no content at all. The
			// previous code tolerated exactly this and read it as the legacy
			// zero identity; failing every adoption pass over such a journal
			// would be a worse answer than the meaning it already had. A
			// payload that HAS content and does not decode is still an error.
			break
		}
		var payload AgentAssignedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return AgentIdentity{}, fmt.Errorf("run %s records an agent assignment that cannot be read: %w", s.run.ID, err)
		}
		return AgentIdentity{
			AgentID: payload.AgentID, Kind: payload.ProviderKind,
			TrustMode: payload.TrustMode, Model: payload.Model,
		}, nil
	}
	return AgentIdentity{}, nil
}

// remainingBudgets is the cumulative allowance left on this run. The runtime's
// own bounds are counted from durable scheduler state; the provider-reported
// dimensions stay unknown, because no configured worker in #63 reports them for
// a subscription CLI and inventing a number would be worse than saying so.
func (r *EngineeringRuntime) remainingBudgets(state *runState) RemainingBudgets {
	budgets := state.budgets()
	// ACTIVE time, by the same rule conditions() enforces the budget with.
	// Raw elapsed contradicted it: a run that sat overnight awaiting review
	// would record wall_seconds: 0 in a durable handoff record while the
	// runtime would still grant it nearly its whole budget - provenance
	// disagreeing with the semantics it exists to carry forward.
	elapsed := state.activeElapsed(r.deps.Clock.Now())
	wall := int64((budgets.WallLimit - elapsed) / time.Second)
	if wall < 0 {
		wall = 0
	}
	remaining := func(kind string, ceiling int) BudgetDimension {
		spent := state.projection.Attempts[kind]
		if left := ceiling - spent; left > 0 {
			return KnownBudget(int64(left))
		}
		return KnownBudget(0)
	}
	continuations := state.continuationLimit() - len(state.startedContinuationBindings())
	if continuations < 0 {
		continuations = 0
	}
	// A run total is OPTIONAL - a plan states it, an ordinary run may not - and
	// where none was stated the answer is not zero and not a number the runtime
	// made up. It is unknown, by the same rule the provider-reported dimensions
	// follow.
	invocations := UnknownBudget()
	if limit := state.providerInvocationLimit(); limit > 0 {
		left := limit - state.projection.Attempts[OpExecutionInvoke]
		if left < 0 {
			left = 0
		}
		invocations = KnownBudget(int64(left))
	}
	return RemainingBudgets{
		WallSeconds:         KnownBudget(wall),
		ExecutionAttempts:   remaining(OpExecutionInvoke, budgets.MaxExecutionAttempts),
		ProviderInvocations: invocations,
		RemediationAttempts: remaining(OpRemediationGofmt, budgets.MaxRemediationAttempts),
		AssuranceAttempts:   remaining(OpAssuranceGo, budgets.MaxAssuranceAttempts),
		Continuations:       KnownBudget(int64(continuations)),
		Tokens:              UnknownBudget(),
		Cost:                UnknownBudget(),
	}
}

// RequestAgentHandoff is the explicit operator action for moving an existing
// run to a different execution agent. In #63 it always REFUSES, and the refusal
// is the product: it journals a complete transition record and returns it, so
// the attempt is auditable and the operator is told what to do instead.
//
// It mutates nothing. No authority, no evidence, no candidate state and no
// budget changes, which is what makes recording the attempt safe.
func (r *EngineeringRuntime) RequestAgentHandoff(runID, agentID, reason string) (AgentHandoffRecord, error) {
	if strings.TrimSpace(reason) == "" {
		return AgentHandoffRecord{}, fmt.Errorf("changing the execution agent of a live run requires an explicit reason")
	}
	target, err := r.agents.Agent(agentID)
	if err != nil {
		return AgentHandoffRecord{}, err
	}
	state, err := r.load(runID)
	if err != nil {
		return AgentHandoffRecord{}, err
	}
	from, err := state.recordedAgent()
	if err != nil {
		return AgentHandoffRecord{}, err
	}
	record := AgentHandoffRecord{
		RunID:             runID,
		CandidateRevision: state.projection.CandidateRevision,
		CandidateTree:     state.projection.CandidateTree,
		From:              from,
		To: AgentIdentity{
			AgentID: target.ID, Kind: target.Kind,
			TrustMode: target.TrustMode, Model: target.Model,
		},
		Reason:                 boundedDetail(reason),
		Contract:               state.projection.Contract,
		Config:                 r.deps.ConfigDigest,
		Budgets:                r.remainingBudgets(state),
		EvidenceBundles:        len(state.projection.EvidenceBundles),
		FeedbackConsumed:       state.consumedFeedbackCount(),
		FeedbackConsumedDigest: state.consumedFeedbackDigest(),
		// Nothing in #63 persists a provider-native session identifier, so
		// there is none to migrate. Recording the law explicitly keeps it true
		// if one is ever added: a predecessor's session is retired by the
		// transition and never becomes the successor's live session.
		ProviderSessionRetired: true,
		Refused:                true,
		RecordedAt:             r.deps.Clock.Now().UTC().Format(time.RFC3339),
	}
	if operation, ok := state.currentOperation(); ok {
		record.PredecessorOperationID = operation.ID
		record.PredecessorAttempt = operation.Attempt
	}
	if decision, ok := state.publicationDecision(); ok {
		record.Authority = decision.Status
	}
	switch {
	case record.From.AgentID == target.ID:
		return AgentHandoffRecord{}, fmt.Errorf("run %s is already worked by agent %s", runID, target.ID)
	case record.TrustDowngradeRefused():
		record.RefusalCode = HandoffTrustDowngrade
	default:
		record.RefusalCode = HandoffNotImplemented
	}
	if err := r.append(state, EventRunAgentHandoffRefused, "", record, nil); err != nil {
		return AgentHandoffRecord{}, err
	}
	return record, &AgentHandoffRefusedError{Record: record}
}

// CancelRun records durable operator cancellation intent for one run and stops
// its scheduling. It lives here, in the runtime, because two callers need
// exactly one cancellation path: the `stop RUN` command and the supervisor's
// explicit stop-all action. A second implementation would be a second answer to
// "is this run cancelled".
//
// Three things happen, in this order, and nothing else does:
//
//  1. run.cancelled is appended through the journal, which is the authority
//     every later replay reads. Because it is durable, the run is still
//     cancelled after a restart.
//  2. the run document is settled as cancelled, which is what stops the
//     scheduler from handing this run out again.
//  3. every operation the store still believes is active has cancellation
//     REQUESTED on it through the scheduler's existing mechanism, which the
//     runtime already honours. No second cancellation mechanism is introduced,
//     and no lease another process owns is written out from under it.
//
// It is idempotent: cancelling an already cancelled run appends nothing and
// reports the same answer.
func CancelRun(store *SQLiteOperationStore, scheduler Scheduler, now time.Time, runID, reason string) (Outcome, error) {
	run, found, err := store.Run(runID)
	if err != nil {
		return Outcome{}, err
	}
	if !found {
		return Outcome{}, fmt.Errorf("unknown run %q", runID)
	}
	outcome := Outcome{RunID: runID, Disposition: Cancelled, Reason: reason}
	if run.Disposition == Cancelled {
		outcome.Reason = run.Reason
		return outcome, nil
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason,omitempty"`
	}{reason})
	if err != nil {
		return Outcome{}, err
	}
	if _, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		// The operator's stated reason belongs in the PAYLOAD, where it is
		// bounded, and not in the durable identity. `stop-all --reason <text>`
		// and the control endpoint both carry arbitrary operator text, and an
		// event id is a primary key that is read back forever.
		ID:         fmt.Sprintf("%s-cancelled-%d", runID, now.UnixNano()),
		RunID:      runID,
		Type:       EventRunCancelled,
		OccurredAt: now,
		Payload:    payload,
	}); err != nil {
		return Outcome{}, err
	}
	run.Disposition, run.Reason, run.UpdatedAt = Cancelled, reason, now
	if err := store.PutRun(run); err != nil {
		return Outcome{}, err
	}
	operations, err := store.Operations(runID)
	if err != nil {
		return Outcome{}, err
	}
	for _, op := range operations {
		if op.State != Leased && op.State != Running {
			continue
		}
		if _, err := scheduler.RequestCancel(op.ID); err != nil {
			return Outcome{}, err
		}
	}
	return outcome, nil
}
