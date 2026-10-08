package runtime

// #508 P4b: the bounded, same-RunID decision continuation. A worker's own
// resolved decision_request(s) cause exactly one additional, bounded
// provider invocation on the SAME run - never a duplicate of one an
// ordinary checkpoint continuation, head remediation or reviewer-feedback
// invocation already delivered, never evading the existing continuation or
// attempt budgets, and never dispatched with a decision set or candidate
// subject that has moved since the operation was created.
//
// Three durable facts, all already owned elsewhere, compose the whole
// mechanism:
//   - orchestration.DecisionResolution (decision_resolution.go) - the
//     immutable, authority-checked answer;
//   - EventDecisionsDelivered (this file) - the exact, per-physical-attempt
//     record of what one invocation's compiled context actually carried,
//     journalled only once that attempt reached the worker (reachedWorker),
//     never inferred from a timestamp;
//   - the existing OpExecutionInvoke scheduler, attempt ceilings and
//     continuation-depth ceiling (reconciler.go) - unmodified, and now
//     counting this binding shape too.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// decisionResumptionPrefix marks an execution-invoke binding as resuming a
// run after its own resolved decision_request(s) - the same durable-identity
// convention invocationContinuationPrefix already uses for a checkpoint
// continuation, a DIFFERENT prefix so the two are never confused, but
// counted against the SAME finite continuation ceiling (isResumptionBinding,
// reconciler.go).
const decisionResumptionPrefix = "decision-resumed|"

// DeliveredDecision is one resolved decision a compiled context actually
// carried: the request it answers and the exact resolution it delivered.
type DeliveredDecision struct {
	RequestID    string `json:"request_id"`
	ResolutionID string `json:"resolution_id"`
}

// DecisionsDeliveredPayload is EventDecisionsDelivered's schema: the exact,
// sorted set one physical attempt's compiled context carried, and the
// candidate/contract subject it was compiled against. Decisions is never
// empty - the event is journalled only when there is at least one.
type DecisionsDeliveredPayload struct {
	OperationID      string              `json:"operation_id"`
	Attempt          int                 `json:"attempt"`
	Decisions        []DeliveredDecision `json:"decisions"`
	ContractRevision string              `json:"contract_revision,omitempty"`
	CandidateSubject string              `json:"candidate_subject,omitempty"`
}

func validateDecisionsDelivered(p DecisionsDeliveredPayload) error {
	if len(p.Decisions) == 0 || len(p.Decisions) > orchestration.MaxMessagesPerInvocation {
		return fmt.Errorf("a decision delivery names 1 to %d decisions, not %d", orchestration.MaxMessagesPerInvocation, len(p.Decisions))
	}
	for _, d := range p.Decisions {
		if err := errors.Join(required("request_id", d.RequestID), required("resolution_id", d.ResolutionID)); err != nil {
			return err
		}
	}
	return errors.Join(required("operation_id", p.OperationID), positive("attempt", p.Attempt))
}

// deliveredDecisionsEvent builds EventDecisionsDelivered's journal entry for
// one physical attempt, or ok=false when it delivered nothing (an ordinary
// invocation whose scope has no resolved decision at all).
func deliveredDecisionsEvent(resolved []orchestration.DecisionResolution, operationID string, attempt int, contractRevision, candidateSubject string) (journalEntry, bool) {
	if len(resolved) == 0 {
		return journalEntry{}, false
	}
	decisions := make([]DeliveredDecision, len(resolved))
	for i, r := range resolved {
		decisions[i] = DeliveredDecision{RequestID: r.RequestID, ResolutionID: r.ID}
	}
	return journalEntry{Type: EventDecisionsDelivered, Payload: DecisionsDeliveredPayload{
		OperationID: operationID, Attempt: attempt, Decisions: decisions,
		ContractRevision: contractRevision, CandidateSubject: candidateSubject,
	}}, true
}

// deliveredDecisionIDs is the set of this run's own decision request IDs ANY
// attempt has already been shown - durable, from this run's own append-only
// event journal, never inferred from a timestamp. An attempt that delivered
// a decision and then failed still counts: delivery is not completion (#508
// review P4b §3), so the SAME decision is never redelivered through a
// SECOND operation merely because the first one did not succeed.
func (s *runState) deliveredDecisionIDs() map[string]bool {
	delivered := map[string]bool{}
	for _, event := range s.events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err != nil {
			continue // malformed historical payload never blocks the run on it
		}
		for _, d := range payload.Decisions {
			delivered[d.RequestID] = true
		}
	}
	return delivered
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

// decisionResumeStillValid re-derives, from a FRESH store read taken
// immediately before this attempt is given any context, the binding a
// decision-resumed operation would get RIGHT NOW, and compares it against
// the binding this exact operation already carries. A mismatch - a decision
// superseded, a newer one now claiming the coalesced set, or the candidate
// subject having moved since this operation was created - means its claim
// no longer holds, and it is refused rather than dispatched with context
// that would contradict its own admitted binding (#508 review P4b §6).
//
// This re-read is compared against an already-admitted, immutable binding;
// it is not a second atomic transaction guarding that binding's own
// creation. The existing AcquireOperation lease fence (sqlite_store.go)
// already serializes which driver may run this exact operation at all; this
// check runs once more, later, immediately before that driver commits to
// inviting a provider into this exact context - the closest this
// architecture's existing scheduler gets to a dispatch-time fence, reusing
// its own store reads rather than adding a second one.
//
// Nil means proceed. Every operation whose binding is not decision-resumed
// returns immediately, at the cost of one store read only for the one
// binding shape that needs it.
func (r *EngineeringRuntime) decisionResumeStillValid(state *runState, operation RunOperation) *effect {
	binding := bindingOf(operation)
	if !strings.HasPrefix(binding, decisionResumptionPrefix) {
		return nil
	}
	fresh, err := r.deps.Store.ResolvedOwnDecisionRequests(state.run.ID)
	if err != nil {
		return &effect{state: OperationFailed, result: executionRecord{
			mutationResult: mutationResult{FailureClass: FailureUnknown},
			Diagnostic: r.executionDiagnostic(execStageWorkspaceSubject, FailureUnknown, ExecutionResult{},
				fmt.Errorf("revalidating this decision-resumed operation's own decision set: %w", err)),
		}}
	}
	undelivered := undeliveredOf(fresh, state.deliveredDecisionIDs())
	if len(undelivered) == 0 || decisionResumeBinding(state.contractRevision(), state.projection.CandidateRevision, undelivered) != binding {
		cause := errors.New("this decision-resumed operation's own decision set or candidate subject no longer matches current durable state")
		return &effect{state: OperationFailed, result: executionRecord{
			mutationResult: mutationResult{FailureClass: FailureDecisionBindingStale},
			Diagnostic:     r.executionDiagnostic(execStageWorkspaceSubject, FailureDecisionBindingStale, ExecutionResult{}, cause),
		}}
	}
	return nil
}
