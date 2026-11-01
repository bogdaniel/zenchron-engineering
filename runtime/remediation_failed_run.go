package runtime

// A producer run that ends FAILED (#693: its assurance budget ran out, after a
// remediation attempt's handoff had been admitted) leaves a remediation epoch
// nothing to hang on: the epoch_authorization request is raised for a live
// Finding about a COMPLETED unit's admitted handoff, and a failed run has no
// such Finding. The unit stayed failed, its dependents blocked for good, and
// the plan could only be revised - which an objective graph with started work
// refuses.
//
// This raises the SAME governed request for that case, and nothing else. The
// operator still answers it, an approval still becomes the ordinary
// RemediationAuthorization that raises the unit's ceiling by one, and the epoch
// is still a new run charged to the plan's remaining budget with fresh
// assurance. The failed run, its journal and any held candidate stay exactly as
// they are; the new run starts from the unit's inputs, not from that candidate,
// and nothing about the failure is treated as a handoff.

import (
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// failedProducerNeedsEpochRequest is true for a unit whose run ended FAILED and
// is not executing. Whether a handoff was admitted before it failed does not
// matter: a remediation attempt's worker can report "completed", have that
// handoff admitted, and still fail exact-candidate assurance (#693). That
// handoff vouches for nothing the run did not verify, and a failed run
// completes no unit, so the question is the same either way.
func failedProducerNeedsEpochRequest(item OrchestrationItemView) bool {
	return item.State == orchestration.ItemFailed && !item.Executing
}

// failedRunReference names the failed run, and the candidate it held when it
// ended, for the operator deciding and for the authorization that records the
// decision. It is evidence about what failed, not a statement that it passed.
func failedRunReference(runID string, projection RunProjection) string {
	reference := "failed-run:" + runID
	if projection.CandidateRevision == "" {
		return reference
	}
	return fmt.Sprintf("%s candidate=%s tree=%s", reference, projection.CandidateRevision, projection.CandidateTree)
}

// admitFailedRunEpochRequest raises the request once per (unit, next epoch,
// failed run). Insert-only under a deterministic identity, like the Finding path.
func admitFailedRunEpochRequest(store *SQLiteOperationStore, scope, graphID, unitID, unitName, runID string, nextEpoch int, now time.Time) error {
	events, err := store.Events(runID)
	if err != nil {
		return err
	}
	projection, err := Project(events)
	if err != nil {
		return err
	}
	reference := failedRunReference(runID, projection)
	requestID, err := orchestration.EpochAuthorizationRequestID(graphID, unitID, reference, nextEpoch)
	if err != nil {
		return err
	}
	if _, found, err := store.MessageByID(requestID); err != nil || found {
		return err
	}
	request, err := buildEpochAuthorizationRequest(scope, unitName, runID, "", reference, orchestration.HandoffSubject{}, requestID, now)
	if err != nil {
		return err
	}
	return store.AdmitMessages([]orchestration.EngineeringMessage{request})
}

// failedRunRequestIsCurrent reports whether an approved failed-run request still
// describes the unit: it was raised about the unit's CURRENT run, and its
// deterministic identity is the one for the epoch that would follow now. An
// approval that arrives after the unit has moved on is a decision about work
// that has since been superseded, and confers nothing.
func failedRunRequestIsCurrent(request orchestration.EngineeringMessage, graphID, unitID, currentRunID string, nextEpoch int) bool {
	if request.Source.RunID != currentRunID {
		return false
	}
	id, err := orchestration.EpochAuthorizationRequestID(graphID, unitID, request.Body, nextEpoch)
	return err == nil && id == request.ID
}
