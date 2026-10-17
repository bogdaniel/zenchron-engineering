package runtime

// Remediation epoch/authorization persistence (#581). Deliberately isolated
// in its own file and its own two tables, exactly like WorkUnitActivation
// (workgraph_store.go): nothing here rewrites work_graph_activations, and
// nothing here is a second run database.
//
// Batch 1 shipped the read paths and the schema alone, with nothing calling
// them. Batch 2 adds the insert-only write paths: AdmitRemediationAuthorization
// (the governed grant - never a Finding by itself) and AdmitRemediationEpochBatch,
// which claims the epoch row and writes the batch it names TOGETHER, in one
// transaction (independent review finding, PR #585: #470's reconcileOrchestration
// materializes a RUN for ANY batch its own OrchestrationBatches() read
// returns, unconditionally - so a batch visible with no governing epoch
// claim yet is not merely "not yet claimed", it is an executable run
// nothing governs. Two decoupled, idempotent inserts - this batch's first
// drafted approach, mirroring activateWorkUnit - leave exactly that window
// open between them. Atomicity closes it: either both rows commit together,
// so a batch is NEVER visible to materialization without its epoch claim
// already alongside it, or neither does, and a crash in between leaves
// nothing for a retry to find but durable state identical to never having
// tried.
//
// ActivateRemediationEpoch (below) is kept as a standalone, narrower
// primitive for direct tests of the epoch row's own validation and
// uniqueness; runtime/remediation_activation.go no longer calls it as a
// second step after creating the batch - it calls AdmitRemediationEpochBatch
// instead.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
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
	return queryWorkUnitRemediationEpochs(s.db, graphID, unitID)
}

// queryWorkUnitRemediationEpochs is the sqlExecutor-polymorphic read
// WorkGraphStatus (workgraph_status.go) pins inside its own single
// read-only transaction, exactly as queryWorkUnitActivations already does
// for work_graph_activations - never a second, separately-snapshotted read
// racing the rest of that one projection.
func queryWorkUnitRemediationEpochs(q sqlExecutor, graphID, unitID string) ([]RemediationEpoch, error) {
	rows, err := q.Query(`SELECT epoch, batch_id, run_id, inputs_digest, authorization_id, activated_unix_nano
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
	return queryRemediationAuthorizationsFor(s.db, graphID, unitID)
}

// queryRemediationAuthorizationsFor is the sqlExecutor-polymorphic read,
// same reason as queryWorkUnitRemediationEpochs above.
func queryRemediationAuthorizationsFor(q sqlExecutor, graphID, unitID string) ([]RemediationAuthorization, error) {
	rows, err := q.Query(`SELECT id, proposed_epoch, finding_id, authorized_by, policy_clause_ref,
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

// AdmitRemediationAuthorization inserts ONE durable grant, insert-only: a
// repeated reconcile pass that recomputes the identical authorization for
// the same (finding, unit, proposed epoch) finds the row it already wrote
// (inserted=false, no error), never a second, possibly-conflicting one. The
// id is the caller's to compute (orchestration.EpochAuthorizationRequestID
// for the decision-authorized path), so two independent callers proposing
// the SAME authorization always agree on its identity before either writes.
func (s *SQLiteOperationStore) AdmitRemediationAuthorization(authorization RemediationAuthorization) (inserted bool, err error) {
	for name, value := range map[string]string{
		"id": authorization.ID, "graph_id": authorization.GraphID, "unit_id": authorization.UnitID,
		"finding_id": authorization.FindingID,
	} {
		if strings.TrimSpace(value) == "" {
			return false, fmt.Errorf("a remediation authorization needs its %s", name)
		}
	}
	if authorization.ProposedEpoch < 2 {
		return false, fmt.Errorf("a remediation authorization's proposed epoch must be 2 or later, got %d", authorization.ProposedEpoch)
	}
	if authorization.AuthorizedAt.IsZero() {
		return false, errors.New("a remediation authorization needs its authorization time")
	}
	var policyClauseRef, decisionResolutionID any
	switch authorization.AuthorizedBy {
	case RemediationAuthorizedByPolicy:
		if strings.TrimSpace(authorization.PolicyClauseRef) == "" {
			return false, errors.New("a policy-authorized remediation needs its policy clause reference")
		}
		if authorization.DecisionResolutionID != "" || authorization.RaisesCeilingTo != nil {
			return false, errors.New("a policy-authorized remediation may not carry a decision resolution or a ceiling raise")
		}
		policyClauseRef = authorization.PolicyClauseRef
	case RemediationAuthorizedByDecision:
		if strings.TrimSpace(authorization.DecisionResolutionID) == "" {
			return false, errors.New("a decision-authorized remediation needs the resolution that granted it")
		}
		if authorization.PolicyClauseRef != "" {
			return false, errors.New("a decision-authorized remediation may not also carry a policy clause reference")
		}
		decisionResolutionID = authorization.DecisionResolutionID
	default:
		return false, fmt.Errorf("remediation authorization authorized_by %q is not %q or %q",
			authorization.AuthorizedBy, RemediationAuthorizedByPolicy, RemediationAuthorizedByDecision)
	}
	var raisesCeilingTo any
	if authorization.RaisesCeilingTo != nil {
		raisesCeilingTo = *authorization.RaisesCeilingTo
	}
	result, err := s.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, policy_clause_ref, decision_resolution_id, raises_ceiling_to, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		authorization.ID, authorization.GraphID, authorization.UnitID, authorization.ProposedEpoch, authorization.FindingID,
		string(authorization.AuthorizedBy), policyClauseRef, decisionResolutionID, raisesCeilingTo, authorization.AuthorizedAt.UnixNano())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

// ActivateRemediationEpoch claims one unit's Nth remediation attempt,
// exactly once - the same insert-only, primary-key-is-the-whole-idempotency-
// story discipline ActivateWorkUnit already uses for epoch 1
// (workgraph_store.go). The caller (remediation_activation.go) writes the
// batch/run this epoch names FIRST, through the ordinary, already-idempotent
// planOrchestrationBatch/CreateOrchestrationBatch path - never inside one
// transaction with this insert - so a crash between the two never orphans a
// run: the batch/run is a real, independently idempotent claim on its own
// deterministic identity (WorkUnitBatchID, now epoch-bound via
// BatchOrigin.Epoch), and replaying this call after such a crash finds that
// SAME batch/run again rather than creating a second one, then simply
// retries recording the epoch row.
func (s *SQLiteOperationStore) ActivateRemediationEpoch(epoch RemediationEpoch) (bool, error) {
	if err := validateRemediationEpoch(epoch); err != nil {
		return false, err
	}
	inserted, err := execRemediationEpochInsert(s.db, epoch)
	return inserted, err
}

// validateRemediationEpoch is the one validation both ActivateRemediationEpoch
// and AdmitRemediationEpochBatch enforce - never duplicated, so the atomic
// path cannot be looser than the standalone one.
func validateRemediationEpoch(epoch RemediationEpoch) error {
	for name, value := range map[string]string{
		"graph_id": epoch.GraphID, "unit_id": epoch.UnitID, "batch_id": epoch.BatchID,
		"run_id": epoch.RunID, "inputs_digest": epoch.InputsDigest, "authorization_id": epoch.AuthorizationID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("a remediation epoch activation needs its %s", name)
		}
	}
	if epoch.Epoch < 2 {
		return fmt.Errorf("a remediation epoch must be 2 or later, got %d", epoch.Epoch)
	}
	if epoch.ActivatedAt.IsZero() {
		return errors.New("a remediation epoch activation needs its activation time")
	}
	return nil
}

// execRemediationEpochInsert is the sqlExecutor-polymorphic insert both the
// standalone method and the atomic batch+epoch admission below share.
func execRemediationEpochInsert(q sqlExecutor, epoch RemediationEpoch) (bool, error) {
	result, err := q.Exec(`INSERT INTO work_graph_remediation_epochs
		(graph_id, unit_id, epoch, batch_id, run_id, inputs_digest, authorization_id, activated_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(graph_id, unit_id, epoch) DO NOTHING`,
		epoch.GraphID, epoch.UnitID, epoch.Epoch, epoch.BatchID, epoch.RunID, epoch.InputsDigest,
		epoch.AuthorizationID, epoch.ActivatedAt.UnixNano())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// AdmitRemediationEpochBatch claims one unit's remediation epoch AND writes
// the batch it names, together, in one transaction (independent review
// finding on PR #585 - see the file-level comment above for why decoupling
// them is unsafe). Idempotent exactly like CreateOrchestrationBatch: a
// batch already stored is returned unchanged with created=false, never
// re-inserted and never re-validated against a possibly-different epoch
// argument - the FIRST writer's epoch claim is the one that stands, by the
// same "first activation wins" discipline #472's own activation already
// uses. A caller that is replaying after an earlier crash passes the exact
// same batch and epoch it would have on a first attempt (both are built
// from deterministic facts - graph, unit, proposed epoch, inputs - never
// freshly randomized per call), so the replayed call finds exactly what it
// would have created.
func (s *SQLiteOperationStore) AdmitRemediationEpochBatch(batch orchestration.Batch, epoch RemediationEpoch) (orchestration.Batch, bool, error) {
	if err := validateEpochAdmission(batch, epoch); err != nil {
		return orchestration.Batch{}, false, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	defer tx.Rollback()
	stored, created, err := admitRemediationEpochTx(tx, batch, epoch)
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	return stored, created, tx.Commit()
}

func validateEpochAdmission(batch orchestration.Batch, epoch RemediationEpoch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	if err := validateRemediationEpoch(epoch); err != nil {
		return err
	}
	if batch.ID != epoch.BatchID {
		return fmt.Errorf("batch %s does not match the epoch claim's own batch %s", batch.ID, epoch.BatchID)
	}
	return nil
}

// admitRemediationEpochTx writes the batch and its epoch claim inside the
// caller's transaction, so a caller that must commit something else with them
// (a plan's child-run charge) can make all of it one atomic fact.
func admitRemediationEpochTx(tx *sql.Tx, batch orchestration.Batch, epoch RemediationEpoch) (orchestration.Batch, bool, error) {
	document, err := CanonicalJSON(batch)
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	if err := requireBatchProductBinding(tx, batch); err != nil {
		return orchestration.Batch{}, false, err
	}
	result, err := tx.Exec(`INSERT INTO orchestration_batches (id, repository, created_unix_nano, document)
		VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		batch.ID, batch.Repository, batch.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return orchestration.Batch{}, false, err
	}
	if inserted == 0 {
		// Already claimed by an earlier, committed attempt - including its
		// epoch row, which can only ever have committed in the SAME
		// transaction as this batch row. Read and return it; write nothing.
		stored, found, err := queryOrchestrationBatch(tx, batch.ID)
		if err != nil {
			return orchestration.Batch{}, false, err
		}
		if !found {
			return orchestration.Batch{}, false, fmt.Errorf("orchestration batch %s was neither inserted nor found", batch.ID)
		}
		return stored, false, nil
	}
	if _, err := execRemediationEpochInsert(tx, epoch); err != nil {
		return orchestration.Batch{}, false, err
	}
	return batch, true, nil
}

// RemediationAuthorizationByID reads one authorization by its own
// deterministic id, regardless of unit - used to recognize an authorization
// a repeated reconcile pass would otherwise redo the work of proposing
// again (remediation_eligibility.go).
func (s *SQLiteOperationStore) RemediationAuthorizationByID(id string) (RemediationAuthorization, bool, error) {
	row := s.db.QueryRow(`SELECT graph_id, unit_id, proposed_epoch, finding_id, authorized_by, policy_clause_ref,
		decision_resolution_id, raises_ceiling_to, authorized_unix_nano FROM remediation_authorizations WHERE id = ?`, id)
	authorization := RemediationAuthorization{ID: id}
	var authorizedBy string
	var policyClauseRef, decisionResolutionID *string
	var raisesCeilingTo *int
	var authorized int64
	switch err := row.Scan(&authorization.GraphID, &authorization.UnitID, &authorization.ProposedEpoch, &authorization.FindingID,
		&authorizedBy, &policyClauseRef, &decisionResolutionID, &raisesCeilingTo, &authorized); {
	case errors.Is(err, sql.ErrNoRows):
		return RemediationAuthorization{}, false, nil
	case err != nil:
		return RemediationAuthorization{}, false, err
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
	return authorization, true, nil
}
