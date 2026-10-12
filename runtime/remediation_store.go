package runtime

// Remediation epoch/authorization persistence (#581, ADR-0008 Batch 1:
// additive foundation only). Deliberately isolated in its own file and its
// own two tables, exactly like WorkUnitActivation (workgraph_store.go):
// nothing here rewrites work_graph_activations, and nothing here is a
// second run database. These are READ paths only - the atomic
// epoch+run+operations admission transaction ADR-0008 §8 specifies is #581
// Batch 2, not this change. No existing code calls anything in this file
// yet, so adding it changes no existing behavior (ADR-0008 §3).

import (
	"time"
)

// RemediationEpoch is one durable row of work_graph_remediation_epochs: a
// WorkUnit's remediation attempt beyond its original epoch 1 (which stays
// recorded, unconditionally, as a WorkUnitActivation - never duplicated
// here). Every field is a fact about how this epoch was authorized and
// activated; none is ever updated after insertion (ADR-0008 §4.1). "How it
// went" is read off RunID's own lifecycle via orchestration.DeriveEpochOutcome,
// never a field on this struct.
type RemediationEpoch struct {
	GraphID         string
	UnitID          string
	Epoch           int
	BatchID         string
	RunID           string
	InputsDigest    string
	AuthorizationID string
	ActivatedAt     time.Time
}

// RemediationAuthorizedBy distinguishes remediation_authorizations' two
// mutually exclusive provenance paths (ADR-0008 §5, enforced by the table's
// own CHECK constraint, not merely by this type).
type RemediationAuthorizedBy string

const (
	// RemediationAuthorizedByPolicy is a plan-preauthorized grant, compiled
	// into the unit's EngineeringContract. It can never carry
	// RaisesCeilingTo or DecisionResolutionID.
	RemediationAuthorizedByPolicy RemediationAuthorizedBy = "policy"
	// RemediationAuthorizedByDecision is a human's resolved #508
	// decision_request(epoch_authorization). It always carries
	// DecisionResolutionID, and may carry RaisesCeilingTo.
	RemediationAuthorizedByDecision RemediationAuthorizedBy = "decision"
)

// RemediationAuthorization is one durable row of remediation_authorizations
// (ADR-0008 §5): the only record - never a Finding by itself - that makes a
// WorkUnit eligible for a given next epoch.
type RemediationAuthorization struct {
	ID                   string
	GraphID              string
	UnitID               string
	ProposedEpoch        int
	FindingID            string
	AuthorizedBy         RemediationAuthorizedBy
	PolicyClauseRef      string
	DecisionResolutionID string
	// RaisesCeilingTo is non-nil only for a RemediationAuthorizedByDecision
	// row whose resolving decision explicitly raised the unit's effective
	// ceiling (ADR-0008 §11.1). Nil means "granted within the existing
	// ceiling, no raise."
	RaisesCeilingTo *int
	AuthorizedAt    time.Time
}

// WorkUnitRemediationEpochs reads one unit's remediation epochs (epoch 2
// and beyond; epoch 1 is always a WorkUnitActivation, read separately via
// WorkUnitActivations). Returns an empty slice, never an error, for a unit
// that has never remediated - which is every unit on every graph that
// existed before this table did.
func (s *SQLiteOperationStore) WorkUnitRemediationEpochs(graphID, unitID string) ([]RemediationEpoch, error) {
	rows, err := s.db.Query(`SELECT epoch, batch_id, run_id, inputs_digest, authorization_id, activated_unix_nano
		FROM work_graph_remediation_epochs WHERE graph_id = ? AND unit_id = ? ORDER BY epoch ASC`,
		graphID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var epochs []RemediationEpoch
	for rows.Next() {
		epoch := RemediationEpoch{GraphID: graphID, UnitID: unitID}
		var activated int64
		if err := rows.Scan(&epoch.Epoch, &epoch.BatchID, &epoch.RunID, &epoch.InputsDigest,
			&epoch.AuthorizationID, &activated); err != nil {
			return nil, err
		}
		epoch.ActivatedAt = time.Unix(0, activated).UTC()
		epochs = append(epochs, epoch)
	}
	return epochs, rows.Err()
}

// RemediationAuthorizationsFor reads every remediation authorization ever
// written for one unit, in authorization order. Returns an empty slice,
// never an error, for a unit that has never had one requested.
func (s *SQLiteOperationStore) RemediationAuthorizationsFor(graphID, unitID string) ([]RemediationAuthorization, error) {
	rows, err := s.db.Query(`SELECT id, proposed_epoch, finding_id, authorized_by, policy_clause_ref,
		decision_resolution_id, raises_ceiling_to, authorized_unix_nano
		FROM remediation_authorizations WHERE graph_id = ? AND unit_id = ? ORDER BY authorized_unix_nano ASC, id ASC`,
		graphID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var authorizations []RemediationAuthorization
	for rows.Next() {
		authorization := RemediationAuthorization{GraphID: graphID, UnitID: unitID}
		var authorizedBy string
		var policyClauseRef, decisionResolutionID *string
		var raisesCeilingTo *int
		var authorized int64
		if err := rows.Scan(&authorization.ID, &authorization.ProposedEpoch, &authorization.FindingID,
			&authorizedBy, &policyClauseRef, &decisionResolutionID, &raisesCeilingTo, &authorized); err != nil {
			return nil, err
		}
		authorization.AuthorizedBy = RemediationAuthorizedBy(authorizedBy)
		if policyClauseRef != nil {
			authorization.PolicyClauseRef = *policyClauseRef
		}
		if decisionResolutionID != nil {
			authorization.DecisionResolutionID = *decisionResolutionID
		}
		authorization.RaisesCeilingTo = raisesCeilingTo
		authorization.AuthorizedAt = time.Unix(0, authorized).UTC()
		authorizations = append(authorizations, authorization)
	}
	return authorizations, rows.Err()
}

// latestAttemptEpoch returns the highest epoch number across epoch 1
// (always present once activated, via WorkUnitActivations) and any
// RemediationEpoch rows - ADR-0008 §4.1's LatestAttemptEpoch, as a small
// pure helper over already-read slices so callers (Batch 2's projection
// wiring) need not duplicate the max-of-two-sources logic.
func latestAttemptEpoch(remediationEpochs []RemediationEpoch) int {
	latest := 1
	for _, epoch := range remediationEpochs {
		if epoch.Epoch > latest {
			latest = epoch.Epoch
		}
	}
	return latest
}
