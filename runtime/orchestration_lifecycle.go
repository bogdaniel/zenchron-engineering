package runtime

import "fmt"

// A read transaction prevents mixed reads, but a process can crash between
// journal and row writes. Those durable transitions remain explicit on read.
func orchestrationLifecycleCoherent(run EngineeringRun, snapshot RunSnapshot, operations map[string]RunOperation) error {
	if run.Disposition != snapshot.Disposition || run.Reason != snapshot.Reason {
		return fmt.Errorf("transitioning: run row and journal lifecycle disagree")
	}
	for id, row := range operations {
		if run.Disposition == Completed && row.Lease != nil && (row.State == Leased || row.State == Running) {
			return fmt.Errorf("transitioning: completed child still owns an operation")
		}
		journal, found := snapshot.Operations[id]
		if !found {
			return fmt.Errorf("transitioning: operation row has no journal record")
		}
		if row.RunID != journal.RunID || row.Kind != journal.Kind || row.IdempotencyKey != journal.IdempotencyKey {
			return fmt.Errorf("transitioning: operation row and journal binding disagree")
		}
		// Acquisition precedes operation.before, so the owned row is enough to
		// show running while this ordinary boundary is still being journalled.
		if (row.State == Leased || row.State == OperationCancelled) && journal.State == Pending {
			continue
		}
		// A retry lease is acquired before StartWithin increments the physical
		// attempt identity and before the new operation.before is journalled.
		// During that narrow boundary the row is Leased while the journal still
		// proves the previous attempt failed. Matching attempt identity is what
		// makes this acquisition-only transition unambiguous; a cancelled row
		// paired with a failed journal remains a durable disagreement.
		if row.State == Leased && journal.State == OperationFailed && row.AttemptIdentity == journal.AttemptIdentity {
			continue
		}
		if row.State != journal.State || row.AttemptIdentity != journal.AttemptIdentity {
			return fmt.Errorf("transitioning: operation %s row %s/%d and journal %s/%d disagree", id, row.State, row.AttemptIdentity, journal.State, journal.AttemptIdentity)
		}
	}
	return nil
}

func (s *runState) producerStageFinished() bool {
	if s.snapshot.Disposition != Waiting || s.snapshot.Reason != ReasonGoalStateReached || s.snapshot.Paused != nil {
		return false
	}
	if _, failing := s.currentHeadFailure(); failing || s.projection.SourceIntentChanged {
		return false
	}
	finished := false
	for _, event := range s.events {
		switch event.Type {
		case EventRunWaiting:
			finished = payloadReason(event.Payload) == ReasonGoalStateReached
		case EventFeedbackObserved:
			var feedback FeedbackObservedPayload
			if decodeJSON(event.Payload, &feedback) != nil || feedback.Admitted {
				finished = false
			}
		case EventRunCompleted, EventRunFailed, EventRunCancelled,
			EventCandidateChanged, EventCandidateCommitted, EventCandidateCheckpointed,
			EventCandidateBaseIntegrated, EventExecutionCompleted, EventContractCompiled,
			EventReassessmentCompleted, EventAssuranceObserved, EventSemanticAssuranceObserved,
			EventAuthorityEvaluated, EventStageReviewBlocked,
			EventSourceIntentChanged, EventSourceOptInRemoved, EventSourceOptInRestored,
			EventCandidateExternalChanged, EventControllerSuccessionAdmitted, EventHumanAuthorityRecorded:
			finished = false
		case EventOperationPlanned, EventOperationBefore:
			var op RunOperation
			if decodeJSON(event.Payload, &op) != nil {
				return false
			}
			if OperationCapacityClass(op.Kind) == CapacityWork {
				finished = false
			}
		}
	}
	return finished
}

// Supersession is durable: starting the matching resource proves it was
// reacquired, even if the controller never records its next disposition.
func capacityWaitFor(events []EngineeringEvent, reason string) (*CapacityWait, bool, error) {
	started := map[CapacityClass]bool{}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case EventRunCompleted, EventRunFailed, EventRunCancelled:
			return nil, false, nil
		case EventOperationBefore:
			var op RunOperation
			if err := decodeJSON(event.Payload, &op); err != nil {
				return nil, false, err
			}
			started[OperationCapacityClass(op.Kind)] = true
			if consumesVerification(op.Kind) {
				started[CapacityVerification] = true
			}
		case EventRunWaiting:
			var record dispositionRecord
			if err := decodeJSON(event.Payload, &record); err != nil {
				return nil, false, err
			}
			if record.Reason != reason || record.Capacity == nil {
				return nil, false, nil
			}
			if err := record.Capacity.validate(reason); err != nil {
				return nil, false, err
			}
			if started[record.Capacity.Class] {
				return nil, true, nil
			}
			return record.Capacity, false, nil
		}
	}
	return nil, false, nil
}
