package runtime

import (
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// RunIsBatchItem reports whether a run is an item of any orchestration batch,
// which is the only place a run's message report is ever admitted.
//
// ponytail: scans the batches' items in SQL; index by run id if batches grow
// past thousands.
func (s *SQLiteOperationStore) RunIsBatchItem(runID string) (bool, error) {
	var found int
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM orchestration_batches b, json_each(b.document, '$.items') item
		WHERE json_extract(item.value, '$.run_id') = ?)`, runID).Scan(&found)
	return found == 1, err
}

// DecidedMessageInvocations is every invocation key of one run whose message
// report was already admitted or durably refused.
func (s *SQLiteOperationStore) DecidedMessageInvocations(runID string) (map[string]bool, error) {
	decided := map[string]bool{}
	refused, err := s.db.Query(`SELECT id FROM orchestration_message_refusals WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer refused.Close()
	for refused.Next() {
		var key string
		if err := refused.Scan(&key); err != nil {
			return nil, err
		}
		decided[key] = true
	}
	if err := refused.Err(); err != nil {
		return nil, err
	}
	admitted, err := s.db.Query(`SELECT operation_id, attempt FROM orchestration_messages WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer admitted.Close()
	for admitted.Next() {
		var operationID string
		var attempt int
		if err := admitted.Scan(&operationID, &attempt); err != nil {
			return nil, err
		}
		decided[messageInvocationKey(runID, operationID, attempt)] = true
	}
	return decided, admitted.Err()
}

// awaitsHumanDecision answers whether a run still owes a human decision its
// answer: it asked one that is open, one is resolved but not yet delivered to
// the run, or the run's last invocation wrote a decision_request the runtime
// has not yet admitted. The last case is the ordering hazard: an invocation can
// commit a candidate and ask in the same pass, and the run reads as having
// reached its goal until the next orchestration pass admits the question.
//
// The run keeps its authority throughout. This only withholds the SETTLEMENT a
// dependent stage would consume; the existing #508 protocol resumes the run.
func (r PlanReconciler) awaitsHumanDecision(run EngineeringRun) (bool, error) {
	open, err := r.Store.OpenDecisionRequestsForRun(run.ID)
	if err != nil || len(open) > 0 {
		return len(open) > 0, err
	}
	events, err := r.Store.Events(run.ID)
	if err != nil {
		return false, err
	}
	delivered := map[string]bool{}
	for _, event := range events {
		if event.Type != EventDecisionsDelivered {
			continue
		}
		payload, err := decodePayload[DecisionsDeliveredPayload](event.Payload)
		if err != nil {
			return false, err
		}
		for _, decision := range payload.Decisions {
			delivered[decision.RequestID] = true
		}
	}
	resolved, err := r.Store.ResolvedOwnDecisionRequests(run.ID)
	if err != nil {
		return false, err
	}
	for _, resolution := range resolved {
		if !delivered[resolution.RequestID] {
			return true, nil
		}
	}
	batched, err := r.Store.RunIsBatchItem(run.ID)
	if err != nil || !batched {
		return false, err
	}
	decided, err := r.Store.DecidedMessageInvocations(run.ID)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Type != EventMessagesObserved {
			continue
		}
		observed, err := decodePayload[MessagesObservedPayload](event.Payload)
		if err != nil {
			return false, err
		}
		if observed.Refusal != "" || decided[messageInvocationKey(run.ID, observed.OperationID, observed.Attempt)] {
			continue
		}
		path, err := MessageReportPath(r.StateDir, ExecutionAttemptRef{RunID: run.ID, OperationID: observed.OperationID, Attempt: observed.Attempt})
		if err != nil {
			return false, err
		}
		report, digest, present, err := readMessageReport(path)
		if !present || err != nil || digest != observed.DocumentSHA256 {
			continue // admission refuses a report that is no longer the journalled one
		}
		for _, draft := range report.Messages {
			if draft.Kind == orchestration.KindDecisionRequest {
				return true, nil
			}
		}
	}
	return false, nil
}

// settledRun is runSettled with the human-decision hold applied: a run that
// stopped moving only because it is waiting on a question has not settled on a
// head dependent work may consume.
func (r PlanReconciler) settledRun(run EngineeringRun) (string, bool, error) {
	outcome, settled := runSettled(run)
	if !settled || run.Disposition != Waiting {
		return outcome, settled, nil
	}
	awaiting, err := r.awaitsHumanDecision(run)
	if err != nil || awaiting {
		return "", false, err
	}
	return outcome, true, nil
}
