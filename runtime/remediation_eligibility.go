package runtime

// Remediation eligibility (#581 Batch 2): the governed Finding -> request ->
// authorization bridge ADR-0008 designs. Mirrors deliverIntegrationConflicts's
// shape exactly (runtime/integration_conflict_delivery.go): operates over the
// SAME batch list reconcileOrchestration already loaded, returns one bounded
// problem line per issue, never stops at the first, and is unconditionally
// safe to run every reconcile pass - every admission below is insert-only
// and keyed by a deterministic id, so a conflict already handled on an
// earlier pass simply re-derives the identical row.
//
// It operates ONLY on a unit's epoch-1 batch (BatchOrigin.Epoch == 0): that
// is the one stable, permanent scope #579's deliverIntegrationConflicts
// already always delivers a producer's Findings into (it resolves the
// destination through WorkUnitActivations, which only ever knows epoch 1),
// and the one scope the unit's own real communicationContext inbox is
// rendered from. A later epoch's OWN batch (BatchOrigin.Epoch >= 2) is a
// genuinely different #473 scope - its own one-issue batch, never read for
// Finding/decision traffic here or anywhere else.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// remediationEligibilityOperationID is this reconciler's own, synthetic,
// non-FK-checked operation identity (orchestration_messages.operation_id
// has no foreign key - only run_id does) - never a real run_operations row,
// exactly as deliverIntegrationConflicts's own admitted rows need no
// operation beyond their Source's descriptive fields.
const remediationEligibilityOperationID = "remediation.epoch_authorization"

// reconcileRemediationEligibility is the supervisor pass's remediation step:
// it admits a governed epoch_authorization request for every live, current
// Finding a producer's own epoch-1 batch holds, and turns every resolved
// "approve" into a durable RemediationAuthorization. Called from
// reconcileOrchestration right after deliverIntegrationConflicts, over the
// SAME freshly reloaded batch list.
func reconcileRemediationEligibility(store *SQLiteOperationStore, stateDir string, batches []orchestration.Batch, now time.Time) []string {
	var problems []string
	for _, batch := range batches {
		if batch.Origin == nil || batch.Origin.Epoch != 0 || len(batch.Items) != 1 {
			continue
		}
		graphID, unitID := batch.Origin.GraphID, batch.Origin.UnitID
		unitName := orchestration.BatchItemUnit(batch.Items[0].Issue)
		messages, err := store.ScopeMessages(batch.ID)
		if err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s remediation eligibility: %v", batch.ID, err)))
			continue
		}
		if err := admitEpochAuthorizationRequests(store, stateDir, batch, graphID, unitID, unitName, messages, now); err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s epoch authorization request: %v", batch.ID, err)))
		}
		if err := admitApprovedRemediationAuthorizations(store, graphID, unitID, messages, now); err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s remediation authorization: %v", batch.ID, err)))
		}
	}
	return problems
}

// admitEpochAuthorizationRequests raises the governed question - never the
// authorization itself - for every live Finding bound to this unit's
// CURRENT admitted epoch, that no request already covers.
func admitEpochAuthorizationRequests(store *SQLiteOperationStore, stateDir string, batch orchestration.Batch,
	graphID, unitID, unitName string, messages []orchestration.EngineeringMessage, now time.Time) error {
	currentRunID, epochs, err := currentEpochRunID(store, graphID, unitID, batch.Items[0].RunID)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item := projectOrchestrationItem(tx, stateDir, orchestration.BatchItem{Issue: batch.Items[0].Issue, RunID: currentRunID}, now)
	if err := tx.Commit(); err != nil {
		return err
	}
	if item.State != orchestration.ItemCompleted || item.HandoffID == "" {
		return nil // no admitted handoff to be live/current against yet
	}
	currentSubject, found := handoffSubject(store, currentRunID, item.HandoffID)
	if !found {
		return fmt.Errorf("producer unit %s's exact current handoff %s is no longer readable", unitID, item.HandoffID)
	}
	nextEpoch := latestAttemptEpoch(epochs) + 1
	for _, message := range orchestration.Live(messages) {
		if message.Kind != orchestration.KindFinding || message.Route.Unit != unitName {
			continue
		}
		if message.Subject == nil || message.Subject.Handoff != item.HandoffID || !message.AppliesTo(currentSubject) {
			continue // not about the unit's CURRENT admitted epoch
		}
		requestID, err := orchestration.EpochAuthorizationRequestID(graphID, unitID, message.ID, nextEpoch)
		if err != nil {
			return err
		}
		if _, found, err := store.MessageByID(requestID); err != nil {
			return err
		} else if found {
			continue // already raised on an earlier pass
		}
		request, err := buildEpochAuthorizationRequest(batch.ID, unitName, currentRunID, item.HandoffID, message.ID, currentSubject, requestID, now)
		if err != nil {
			return err
		}
		if err := store.AdmitMessages([]orchestration.EngineeringMessage{request}); err != nil {
			return err
		}
	}
	return nil
}

// buildEpochAuthorizationRequest constructs the one new message kind this
// mechanism needs - an ordinary #473 decision_request, never a new field on
// EngineeringMessage, never a new message kind: Purpose names it, and its
// existing selected_option shape (#508 review P4c) carries the closed
// approve/reject/dismiss menu. The justifying Finding's own id travels in
// Body - a short, bounded, machine-read reference, not prose - so resolving
// this request can recover exactly which Finding it answers without a new
// typed field.
func buildEpochAuthorizationRequest(scope, unitName, sourceRunID, handoffID, findingID string,
	subject orchestration.HandoffSubject, id string, now time.Time) (orchestration.EngineeringMessage, error) {
	message := orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion, ID: id, Scope: scope,
		Kind: orchestration.KindDecisionRequest,
		Source: orchestration.MessageSource{
			Unit: unitName, RunID: sourceRunID, OperationID: remediationEligibilityOperationID,
			Attempt: 1, Index: 0, System: true,
		},
		Subject:             &orchestration.MessageSubject{Handoff: handoffID, Owner: unitName, Revision: subject},
		Purpose:             orchestration.EpochAuthorizationPurpose,
		ExpectedOutcomeKind: orchestration.DecisionSelectedOption,
		PermittedOptions:    orchestration.EpochAuthorizationPermittedOptions,
		Body:                findingID,
		AdmittedAt:          now,
	}
	message.Route = orchestration.MessageRoute{Audience: orchestration.AudienceAuthority}
	message.DocumentSHA256 = epochAuthorizationDocumentDigest(id, findingID)
	return message, message.Validate()
}

// epochAuthorizationDocumentDigest stands in for "the digest of the report
// document that produced this message" (communication_slot.go's own
// readMessageReport) for a message this mechanism synthesizes directly,
// with no slot file ever written: a reproducible digest of the one fact
// that makes this request unique, never a value anything else validates
// against.
func epochAuthorizationDocumentDigest(id, findingID string) string {
	sum := sha256.Sum256([]byte(id + ":" + findingID))
	return hex.EncodeToString(sum[:])
}

// admitApprovedRemediationAuthorizations turns every resolved, approved
// epoch_authorization request into its durable RemediationAuthorization -
// the one record EligibleForNextEpoch (pendingRemediationEpoch) actually
// reads. A rejected or dismissed resolution writes nothing here: reject
// leaves the Finding live for a fresh request later, and dismiss's effect
// is entirely read-time (RemediationState, remediation_facts.go) - never a
// row in this table, which exists only for grants.
func admitApprovedRemediationAuthorizations(store *SQLiteOperationStore, graphID, unitID string,
	messages []orchestration.EngineeringMessage, now time.Time) error {
	epochs, err := store.WorkUnitRemediationEpochs(graphID, unitID)
	if err != nil {
		return err
	}
	authorizations, err := store.RemediationAuthorizationsFor(graphID, unitID)
	if err != nil {
		return err
	}
	nextEpoch := latestAttemptEpoch(epochs) + 1
	ceiling := effectiveCeiling(orchestration.WorkUnit{ID: unitID}, authorizations)
	for _, message := range orchestration.Live(messages) {
		if message.Kind != orchestration.KindDecisionRequest || message.Purpose != orchestration.EpochAuthorizationPurpose {
			continue
		}
		resolution, found, err := store.DecisionResolutionByRequestID(message.ID)
		if err != nil {
			return err
		}
		if !found || resolution.Outcome.Value != orchestration.EpochAuthorizationApprove {
			continue
		}
		if _, found, err := store.RemediationAuthorizationByID(message.ID); err != nil {
			return err
		} else if found {
			continue // already granted on an earlier pass
		}
		var raisesCeilingTo *int
		if nextEpoch > ceiling {
			raise := nextEpoch
			raisesCeilingTo = &raise
		}
		authorization := RemediationAuthorization{
			ID: message.ID, GraphID: graphID, UnitID: unitID, ProposedEpoch: nextEpoch,
			FindingID: message.Body, AuthorizedBy: RemediationAuthorizedByDecision,
			DecisionResolutionID: resolution.ID, RaisesCeilingTo: raisesCeilingTo, AuthorizedAt: now,
		}
		if _, err := store.AdmitRemediationAuthorization(authorization); err != nil {
			return err
		}
	}
	return nil
}

// systemReintegrationFindingID is the synthetic finding_id a downstream
// integration_compose unit's automatic reintegration authorization carries:
// there is no Finding driving this grant at all (never-cut: a Finding never
// authorizes anything by itself, and this IS a grant - just one that needs
// no human judgment call, since deterministic Git composition is already
// fully automatic for an ordinary first attempt, #475's own design). A
// readable, descriptive reference, never a value anything joins against.
const systemReintegrationFindingID = "system:upstream-remediation-advanced"

// systemReintegrationPolicyClause names which compiled obligation grants
// this: none does yet (#480's plan->WorkGraph compiler does not exist),
// so this is the one deliberately bounded exception - integration_compose
// units only, triggered only by a dependency's OWN successful remediation,
// never a human decision and never a second authority mechanism.
const systemReintegrationPolicyClause = "system:integration-reintegration-on-upstream-remediation"

// admitAutomaticReintegrationAuthorization grants an invalidated
// integration_compose unit's own next epoch automatically, IF AND ONLY IF
// its invalidation was caused by an upstream dependency's legitimate
// remediation (ADR-0008 §4.3's remediation_epoch_advance cause) rather than
// an arbitrary external replacement - detected the only way available
// without a stored Cause column (deliberately not added, ADR-0008 §4.1's
// "no stored status" discipline extended here): a dependency that has
// EVER remediated (latestAttemptEpoch > 1) is, by this mechanism's own
// construction, the only way a producer's output legitimately changes
// after its first admission. Recomposing deterministically needs no human
// judgment call - #475 already runs integration_compose with no provider
// and no human gate for an ordinary first attempt - so this is the
// plan-preauthorized ("policy") path, never the decision path, and never
// usable for any OTHER execution kind.
func admitAutomaticReintegrationAuthorization(store *SQLiteOperationStore, graphID, unitID string, now time.Time) error {
	epochs, err := store.WorkUnitRemediationEpochs(graphID, unitID)
	if err != nil {
		return err
	}
	nextEpoch := latestAttemptEpoch(epochs) + 1
	id, err := orchestration.EpochAuthorizationRequestID(graphID, unitID, systemReintegrationFindingID, nextEpoch)
	if err != nil {
		return err
	}
	if _, found, err := store.RemediationAuthorizationByID(id); err != nil {
		return err
	} else if found {
		return nil // already granted on an earlier pass
	}
	_, err = store.AdmitRemediationAuthorization(RemediationAuthorization{
		ID: id, GraphID: graphID, UnitID: unitID, ProposedEpoch: nextEpoch,
		FindingID: systemReintegrationFindingID, AuthorizedBy: RemediationAuthorizedByPolicy,
		PolicyClauseRef: systemReintegrationPolicyClause, AuthorizedAt: now,
	})
	return err
}

// currentEpochRunID is the non-tx-scoped sibling of remediation_facts.go's
// currentEpochSource lookup - this reconciler runs outside WorkGraphStatus's
// own snapshot, over the batch list reconcileOrchestration already loaded,
// so a small staleness window here (another pass may activate a newer
// epoch between this read and this function's own work) is exactly as safe
// as deliverIntegrationConflicts' own equivalent window: every write below
// is insert-only and idempotent, so a pass that acted on an epoch since
// superseded simply re-derives a row a later pass's own fresher read
// already matches, or is harmlessly refused by AppliesTo/staleness checks.
func currentEpochRunID(store *SQLiteOperationStore, graphID, unitID, epoch1RunID string) (string, []RemediationEpoch, error) {
	epochs, err := store.WorkUnitRemediationEpochs(graphID, unitID)
	if err != nil {
		return "", nil, err
	}
	latest := latestAttemptEpoch(epochs)
	runID := epoch1RunID
	for _, epoch := range epochs {
		if epoch.Epoch == latest {
			runID = epoch.RunID
		}
	}
	return runID, epochs, nil
}
