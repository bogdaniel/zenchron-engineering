package runtime

// A producer run that ends FAILED after coding but before successful assurance
// (#693: its assurance budget ran out) leaves a remediation epoch without a
// current successful producer output. This is true whether the worker reported
// no handoff or an earlier handoff was admitted: terminal failure dominates
// that handoff, and it cannot satisfy downstream units. A Finding about a
// completed producer's admitted handoff cannot repair this failed run.
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
