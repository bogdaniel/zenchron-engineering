package runtime

// #508 P4b: the bounded, same-RunID decision continuation. A worker's own
// resolved decision_request(s) cause exactly one additional, bounded
// provider invocation on the SAME run - never a duplicate of one an
// ordinary checkpoint continuation, head remediation or reviewer-feedback
// invocation already delivered, never evading the existing continuation or
// attempt budgets, and never dispatched with a decision set or candidate
// subject that has moved since the operation was created.
//
// Four durable facts, all already owned elsewhere or newly insert-once like
// their siblings, compose the whole mechanism:
//   - orchestration.DecisionResolution (decision_resolution.go) - the
//     immutable, authority-checked answer;
//   - EventDecisionsDelivered (this file) - the exact, per-physical-attempt
//     record of what one invocation's compiled context actually carried for
//     THIS run's own decisions, journalled once that attempt reached the
//     worker (reachedWorker), on every exit that is true for, never
//     inferred from a timestamp;
//   - decision_resumption_admissions (decision_store.go) - the ONE pinned,
//     insert-once snapshot a decision-resumed operation is authorized to
//     resume for, validated fresh inside one BEGIN IMMEDIATE transaction the
//     first time, reused unchanged on every later retry of the SAME
//     operation (review B1/B2: a retry is never re-validated against its
//     OWN prior delivery, and a supersession after admission belongs to a
//     later epoch, never retroactively rewriting it);
//   - the existing OpExecutionInvoke scheduler, attempt ceilings and
//     continuation-depth ceiling (execution_continuation_budget.go) -
//     unmodified, and now counting this binding shape too.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// decisionResumptionPrefix marks an execution-invoke binding as resuming a
// run after its own resolved decision_request(s) - the same durable-identity
// convention invocationContinuationPrefix already uses for a checkpoint
// continuation, a DIFFERENT prefix so the two are never confused, but
// counted against the SAME finite continuation ceiling (isResumptionBinding,
// execution_continuation_budget.go).
const decisionResumptionPrefix = "decision-resumed|"

// maxDeliveredDecisions bounds one delivery record - always THIS run's own
// decisions alone (never a whole, possibly multi-unit, batch scope; see the
// review B4 fix in deliveredForThisRun), so it is set generously above any
// realistic single-run question count rather than reused from #473's own,
// differently-scoped MaxMessagesPerInvocation.
const maxDeliveredDecisions = 64

// DeliveredDecision is one resolved decision a compiled context actually
// carried: the request it answers and the exact resolution it delivered.
type DeliveredDecision struct {
	RequestID    string `json:"request_id"`
	ResolutionID string `json:"resolution_id"`
}

// DecisionsDeliveredPayload is EventDecisionsDelivered's schema: the exact,
// sorted set one physical attempt's compiled context carried OF THIS RUN'S
// OWN decisions alone, and the candidate/contract subject it was compiled
// against. Decisions is never empty - the event is journalled only when
// there is at least one.
type DecisionsDeliveredPayload struct {
	OperationID      string              `json:"operation_id"`
	Attempt          int                 `json:"attempt"`
	Decisions        []DeliveredDecision `json:"decisions"`
	ContractRevision string              `json:"contract_revision,omitempty"`
	CandidateSubject string              `json:"candidate_subject,omitempty"`
}

func validateDecisionsDelivered(p DecisionsDeliveredPayload) error {
	if len(p.Decisions) == 0 || len(p.Decisions) > maxDeliveredDecisions {
		return fmt.Errorf("a decision delivery names 1 to %d decisions, not %d", maxDeliveredDecisions, len(p.Decisions))
	}
	seen := make(map[string]bool, len(p.Decisions))
	for i, d := range p.Decisions {
		if err := errors.Join(required("request_id", d.RequestID), required("resolution_id", d.ResolutionID)); err != nil {
			return err
		}
		if seen[d.RequestID] {
			return fmt.Errorf("a decision delivery names request %s more than once", d.RequestID)
		}
		seen[d.RequestID] = true
		if i > 0 && d.RequestID <= p.Decisions[i-1].RequestID {
			return fmt.Errorf("a decision delivery's own decisions must be sorted and unique by request id")
		}
	}
	return errors.Join(required("operation_id", p.OperationID), positive("attempt", p.Attempt))
}

// deliveredDecisions converts a sorted resolution slice to the sorted,
// deduplicated DeliveredDecision form EventDecisionsDelivered persists.
func deliveredDecisions(resolved []orchestration.DecisionResolution) []DeliveredDecision {
	decisions := make([]DeliveredDecision, len(resolved))
	for i, r := range resolved {
		decisions[i] = DeliveredDecision{RequestID: r.RequestID, ResolutionID: r.ID}
	}
	return decisions
}

// deliveredForThisRun is what ONE physical attempt's compiled context
// actually carried of THIS RUN'S OWN resolved decisions alone (#508 review
// B4): never the whole batch scope communicationContext itself renders for
// every unit alike, which can hold more live resolved decisions than any
// one run asked and overran the per-event bound when journalled whole. own
// is this run's resolvedOwnDecisions (load()-time); shown is the exact set
// communicationContext/prepareMessages rendered for this one attempt. The
// intersection is what THIS attempt is authoritative proof of having shown.
func deliveredForThisRun(own, shown []orchestration.DecisionResolution) []orchestration.DecisionResolution {
	shownIDs := make(map[string]bool, len(shown))
	for _, d := range shown {
		shownIDs[d.RequestID] = true
	}
	var delivered []orchestration.DecisionResolution
	for _, d := range own {
		if shownIDs[d.RequestID] {
			delivered = append(delivered, d)
		}
	}
	sort.Slice(delivered, func(i, j int) bool { return delivered[i].RequestID < delivered[j].RequestID })
	return delivered
}

// decisionDeliveryEntry builds EventDecisionsDelivered's journal entry for
// one physical attempt, or ok=false when nothing of this run's own was
// delivered. admission, when non-zero (a decision-resumed operation that has
// an admitted snapshot - see admitDecisionResumption), is the AUTHORITATIVE
// delivered set: the pinned snapshot, never a fresh recomputation that a
// retry's own prior delivery would make look empty (review B1). Every other
// operation (an ordinary checkpoint continuation, remediation or feedback
// invocation) uses deliveredForThisRun instead. An oversized result fails
// closed - an error, never a silently truncated or dropped delivery record
// (review B4).
func decisionDeliveryEntry(admission DecisionResumptionAdmission, own, shown []orchestration.DecisionResolution,
	operationID string, attempt int, contractRevision, candidateSubject string) (journalEntry, bool, error) {
	decisions := decisionsToDeliver(admission, own, shown)
	if len(decisions) == 0 {
		return journalEntry{}, false, nil
	}
	if len(decisions) > maxDeliveredDecisions {
		return journalEntry{}, false, fmt.Errorf("this attempt's own decisions to deliver (%d) exceed the %d bound a durable delivery record enforces",
			len(decisions), maxDeliveredDecisions)
	}
	return journalEntry{Type: EventDecisionsDelivered, Payload: DecisionsDeliveredPayload{
		OperationID: operationID, Attempt: attempt, Decisions: decisions,
		ContractRevision: contractRevision, CandidateSubject: candidateSubject,
	}}, true, nil
}

// decisionsToDeliver is the one definition of "what this attempt delivers"
// decisionDeliveryEntry and decisionDeliveryPreflight (review F3) both read:
// a decision-resumed operation's own pinned admission when one exists,
// otherwise this run's own resolutions actually shown (deliveredForThisRun).
func decisionsToDeliver(admission DecisionResumptionAdmission, own, shown []orchestration.DecisionResolution) []DeliveredDecision {
	if len(admission.Decisions) > 0 {
		return admission.Decisions
	}
	return deliveredDecisions(deliveredForThisRun(own, shown))
}

// decisionDeliveryEffect is the one shared shell both invokeExecution exit
// points use to journal this attempt's own decision delivery (#508 P4b
// review B3): a reached-worker exit that cannot durably represent its
// delivery becomes a failure instead of silently forgetting it, and an
// exit that never reached the worker is untouched.
func (r *EngineeringRuntime) decisionDeliveryEffect(admission DecisionResumptionAdmission, own, shown []orchestration.DecisionResolution,
	operationID string, attempt int, contractRevision, candidateSubject string, reached bool, events []journalEntry) ([]journalEntry, *effect) {
	if !reached {
		return events, nil
	}
	entry, ok, err := decisionDeliveryEntry(admission, own, shown, operationID, attempt, contractRevision, candidateSubject)
	if err != nil {
		return events, &effect{state: OperationFailed, events: events, result: executionRecord{
			mutationResult: mutationResult{FailureClass: FailureUnknown},
			Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureUnknown, ExecutionResult{}, err),
		}}
	}
	if ok {
		events = append(events, entry)
	}
	return events, nil
}

// deliveredDecisionIDsFromEvents computes the delivered-ID set from a run's
// own append-only event journal, FAILING CLOSED on any EventDecisionsDelivered
// payload present but unreadable (#508 review: "distinguish legacy absence
// from present-but-invalid evidence"): a real delivery this build cannot
// decode must never be silently forgotten and treated as though it never
// happened, which is why this runs once in load() (which already fails the
// whole pass on a corrupt projection) rather than being swallowed inside a
// pure bind function that has no error to return.
func deliveredDecisionIDsFromEvents(events []EngineeringEvent) (map[string]bool, error) {
	delivered := map[string]bool{}
	for _, event := range events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err != nil {
			return nil, fmt.Errorf("a decision-delivery event exists but cannot be decoded: %w", err)
		}
		for _, d := range payload.Decisions {
			delivered[d.RequestID] = true
		}
	}
	return delivered, nil
}

// deliveredDecisionIDs is the set of this run's own decision request IDs ANY
// attempt has already been shown - precomputed once in load() via
// deliveredDecisionIDsFromEvents, never inferred from a timestamp. An
// attempt that delivered a decision and then failed still counts: delivery
// is not completion (#508 review P4b §3), so the SAME decision is never
// redelivered through a SECOND operation merely because the first one did
// not succeed.
func (s *runState) deliveredDecisionIDs() map[string]bool {
	return s.deliveredOwnDecisionIDs
}

// undeliveredResolvedOwnDecisions is this run's own resolved, live decision
// requests (loaded once in load(), s.resolvedOwnDecisions) that no attempt
// has yet been shown, sorted by request id for deterministic binding
// derivation and replay.
func (s *runState) undeliveredResolvedOwnDecisions() []orchestration.DecisionResolution {
	return undeliveredOf(s.resolvedOwnDecisions, s.deliveredDecisionIDs())
}

func undeliveredOf(resolved []orchestration.DecisionResolution, delivered map[string]bool) []orchestration.DecisionResolution {
	var undelivered []orchestration.DecisionResolution
	for _, resolution := range resolved {
		if !delivered[resolution.RequestID] {
			undelivered = append(undelivered, resolution)
		}
	}
	sort.Slice(undelivered, func(i, j int) bool { return undelivered[i].RequestID < undelivered[j].RequestID })
	return undelivered
}

// decisionResumeBinding is the canonical identity a coalesced set of
// undelivered own decisions resumes under: the contract revision, the exact
// candidate subject, and the digest of the sorted remaining request ids -
// never just pinnedBase(), which can be identical across several distinct
// commits or contract epochs (#508 review P4b §4).
func decisionResumeBinding(contractRevision, candidateSubject string, undelivered []orchestration.DecisionResolution) string {
	ids := make([]string, len(undelivered))
	for i, r := range undelivered {
		ids[i] = r.RequestID
	}
	// Both halves are digested, never embedded verbatim: a contract revision
	// already carries a "-next" reassessment suffix on top of a full commit
	// SHA, and the resulting operation id (RunID:kind:kind#binding) overran
	// the 200-byte field bound every event payload enforces. digestOfKeys is
	// the same fixed-width identity feedback's own binding already uses for
	// a SET; one more call gives the (revision, subject) PAIR the same
	// bound, at the cost of a binding a human cannot read back, exactly
	// like "feedback|<rev>|<digest>" already accepts for its own digest half.
	return decisionResumptionPrefix + digestOfKeys([]string{contractRevision, candidateSubject}) + "|" + digestOfKeys(ids)
}

// unresumedDecisionResumeBinding is bindExecutionInvoke's fallback (#508
// review P4b §4): an already-started, NOT-succeeded decision-resumed
// operation for this run is reselected before deriving any new set - the
// same invariant unresolvedFeedbackBinding already uses for reviewer
// feedback - so its ORIGINAL attempt ceiling keeps being spent and no
// decision set is ever planned twice under two different bindings. Only
// once none exists is a fresh binding derived, and only when there is a
// genuinely undelivered decision to derive one for. A bounded SET of several
// decisions resolved before any resumption runs coalesces into exactly one
// binding, never one per request.
func (s *runState) unresumedDecisionResumeBinding() (string, bool) {
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke || op.State == Succeeded {
			continue
		}
		if binding := bindingOf(op); strings.HasPrefix(binding, decisionResumptionPrefix) {
			return binding, true
		}
	}
	undelivered := s.undeliveredResolvedOwnDecisions()
	if len(undelivered) == 0 {
		return "", false
	}
	return decisionResumeBinding(s.contractRevision(), s.projection.CandidateRevision, undelivered), true
}

// DecisionResumptionAdmissionSchemaVersion versions the durable admission
// record decision_store.go's AdmitDecisionResumption writes.
const DecisionResumptionAdmissionSchemaVersion = "0.1"

// DecisionResumptionAdmission is the ONE pinned, insert-once snapshot a
// decision-resumed operation is authorized to resume for (#508 review
// B1/B2). The zero value (Decisions nil) means "no admission exists yet for
// this operation."
type DecisionResumptionAdmission struct {
	SchemaVersion    string              `json:"schema_version"`
	OperationID      string              `json:"operation_id"`
	Binding          string              `json:"binding"`
	Decisions        []DeliveredDecision `json:"decisions"`
	ContractRevision string              `json:"contract_revision,omitempty"`
	CandidateSubject string              `json:"candidate_subject,omitempty"`
	AdmittedAt       time.Time           `json:"admitted_at"`
}

// admitDecisionResumption is invokeExecution's dispatch-time gate for a
// decision-resumed operation (#508 review B1/B2/F2), called once, immediately
// before any context is assembled for it. Every other operation kind or
// binding returns (zero value, nil) at once.
//
// SQLiteOperationStore.AdmitDecisionResumption is idempotent-first-wins,
// exactly like admitHandoff/InsertDecisionResolution beside it: an EXISTING
// admission for this exact operation ID is returned UNCHANGED regardless of
// current decision state, which is what makes a RETRY of the same operation
// - after its own first attempt already delivered this exact set - proceed
// under its own original envelope instead of being refused as "stale"
// against its own prior delivery (review B1). With no existing admission,
// nothing this call passes is trusted as proposed content: the store itself
// re-derives the run's complete current undelivered decision set and
// contract/candidate epoch, inside its own BEGIN IMMEDIATE transaction
// (review B2/F2), and refuses unless decisionResumeBinding recomputed from
// that fresh read reproduces the SAME binding this operation names.
func (r *EngineeringRuntime) admitDecisionResumption(state *runState, operation RunOperation) (DecisionResumptionAdmission, *effect) {
	binding := bindingOf(operation)
	if !strings.HasPrefix(binding, decisionResumptionPrefix) {
		return DecisionResumptionAdmission{}, nil
	}
	admission, err := r.deps.Store.AdmitDecisionResumption(state.run.ID, operation.ID, binding, r.deps.Clock.Now())
	if err != nil {
		cause := fmt.Errorf("this decision-resumed operation's own decision set, contract or candidate subject no longer matches current durable state: %w", err)
		return DecisionResumptionAdmission{}, &effect{state: OperationFailed, result: executionRecord{
			mutationResult: mutationResult{FailureClass: FailureDecisionBindingStale},
			Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureDecisionBindingStale, ExecutionResult{}, cause),
		}}
	}
	return admission, nil
}

// recoverDecisionResumptionContext wraps
// SQLiteOperationStore.RecoverDecisionResumptionContext (review F1) with the
// same OperationFailed/FailureDecisionBindingStale refusal shape
// admitDecisionResumption uses: reached before any provider invocation, so a
// mismatch here never dispatches and never falsely claims a delivery that
// did not happen.
func (r *EngineeringRuntime) recoverDecisionResumptionContext(admission DecisionResumptionAdmission) ([]orchestration.DecisionResolution, *effect) {
	resolved, err := r.deps.Store.RecoverDecisionResumptionContext(admission)
	if err != nil {
		cause := fmt.Errorf("this decision-resumed operation's pinned decisions no longer match current durable state: %w", err)
		return nil, &effect{state: OperationFailed, result: executionRecord{
			mutationResult: mutationResult{FailureClass: FailureDecisionBindingStale},
			Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureDecisionBindingStale, ExecutionResult{}, cause),
		}}
	}
	return resolved, nil
}

// decisionDeliveryPreflight is #508 P4b review F3's dispatch-time bound: the
// count decisionDeliveryEntry will later have to journal is known and
// checked BEFORE Provider.Execute ever runs, so an oversized set is refused
// with ZERO provider starts and no attempt spent on work whose own delivery
// could never be durably recorded, rather than discovered only after a
// worker already saw it.
func (r *EngineeringRuntime) decisionDeliveryPreflight(admission DecisionResumptionAdmission, own, shown []orchestration.DecisionResolution) *effect {
	count := len(decisionsToDeliver(admission, own, shown))
	if count <= maxDeliveredDecisions {
		return nil
	}
	cause := fmt.Errorf("this attempt's own decisions to deliver (%d) exceed the %d bound a durable delivery record enforces", count, maxDeliveredDecisions)
	return &effect{state: OperationFailed, result: executionRecord{
		mutationResult: mutationResult{FailureClass: FailureUnknown},
		Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureUnknown, ExecutionResult{}, cause),
	}}
}

// workspaceSubjectFailure is the one-line OperationFailed/FailureUnknown
// refusal shape the decision-resumed dispatch helpers above share with
// invokeExecution's own pre-dispatch workspace/subject failures.
func (r *EngineeringRuntime) workspaceSubjectFailure(err error) *effect {
	return &effect{state: OperationFailed, result: executionRecord{
		mutationResult: mutationResult{FailureClass: FailureUnknown},
		Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureUnknown, ExecutionResult{}, err),
	}}
}

// prepareDecisionResumedMessages is invokeExecution's one combined step for
// admission, dispatch-time recovery, message rendering and the delivery-size
// preflight (review F1/F3): for an ordinary operation it is a thin pass-
// through to prepareMessages; for a decision-resumed one, the provider's own
// context is built from the exact, freshly re-verified admission snapshot,
// never a fresh scope-wide read, so the decisions this attempt is shown and
// the decisions the journal will claim delivered are always the same set.
func (r *EngineeringRuntime) prepareDecisionResumedMessages(state *runState, operation RunOperation, attempt int) (
	messagePath, communication string, shownDecisions []orchestration.DecisionResolution,
	resumptionAdmission DecisionResumptionAdmission, refusal *effect) {
	resumptionAdmission, refusal = r.admitDecisionResumption(state, operation)
	if refusal != nil {
		return "", "", nil, DecisionResumptionAdmission{}, refusal
	}
	pinnedDecisions, refusal := r.recoverDecisionResumptionContext(resumptionAdmission)
	if refusal != nil {
		return "", "", nil, DecisionResumptionAdmission{}, refusal
	}
	var err error
	messagePath, communication, shownDecisions, err = r.prepareMessages(state, operation.ID, attempt, pinnedDecisions)
	if err != nil {
		return "", "", nil, DecisionResumptionAdmission{}, r.workspaceSubjectFailure(err)
	}
	if refusal := r.decisionDeliveryPreflight(resumptionAdmission, state.resolvedOwnDecisions, shownDecisions); refusal != nil {
		return "", "", nil, DecisionResumptionAdmission{}, refusal
	}
	return messagePath, communication, shownDecisions, resumptionAdmission, nil
}
