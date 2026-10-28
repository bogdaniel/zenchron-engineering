package runtime

// Operator attribution of a feature-acceptance failure.
//
// A judged feature failure on an integrated candidate names no upstream input:
// the integration is deterministic composition, and the tests exercise the
// composed whole. So the runtime does not decide whom to blame. It asks the
// operator, with a decision_request whose closed option set is the integration's
// exact consumed inputs plus "none", frozen at admission. The answer selects;
// it authorizes nothing. A selected input receives one system-authored Finding
// bound to its exact admitted handoff, and from there the existing path is the
// only path: delivery into that unit's own scope, the system-raised
// epoch_authorization decision the operator must still answer, a
// RemediationAuthorization, the unit's next epoch, and the integrator's
// automatic reintegration once that exact corrected output is admitted.
//
// Every write is insert-only under a deterministic identity, so a repeated pass
// - or a pass after a restart - finds what it already wrote. A scope with no
// room, a stale request and an unreadable record are reported as problems and
// write nothing: the hold stays, visible, and nothing is attributed by default.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

const (
	// FeatureAttributionPurpose discriminates the attribution decision_request
	// from every other decision a scope can hold.
	FeatureAttributionPurpose = "feature_attribution"

	featureAttributionOperationID = "feature.attribution"
)

// failedFeatureObservation is the run's current, unstale, judged feature
// failure, or nil.
func failedFeatureObservation(projection RunProjection) *AssuranceObservation {
	a := projection.Assurance
	if a == nil || a.Stale || a.Passed || a.FailureClass != FailureFeatureAcceptance {
		return nil
	}
	return a
}

// featureAttributionRequestID is the deterministic identity of the attribution
// asked about one failed candidate of one unit execution.
func featureAttributionRequestID(origin orchestration.BatchOrigin, commit, tree string) (string, error) {
	digest, err := domain.Digest(struct {
		Graph, Unit, Feature, Commit, Tree string
		Epoch                              int
	}{origin.GraphID, origin.UnitID, origin.AcceptanceFeature, commit, tree, origin.Epoch})
	if err != nil {
		return "", err
	}
	return "message-feature-attribution-" + digest[:32], nil
}

func featureAttributionBody(origin orchestration.BatchOrigin, commit, tree string) string {
	return fmt.Sprintf("feature=%s commit=%s tree=%s", origin.AcceptanceFeature, commit, tree)
}

func reconcileFeatureAttribution(store *SQLiteOperationStore, batches []orchestration.Batch, now time.Time) []string {
	var problems []string
	latest := map[string]int{}
	for _, batch := range batches {
		if origin := batch.Origin; origin != nil && origin.AcceptanceFeature != "" {
			key := origin.GraphID + "\x00" + origin.UnitID
			latest[key] = max(latest[key], origin.Epoch)
		}
	}
	for _, batch := range batches {
		origin := batch.Origin
		if origin == nil || origin.ExecutionKind != orchestration.ExecutionKindIntegrationCompose || origin.AcceptanceFeature == "" || len(batch.Items) != 1 {
			continue
		}
		// A superseded epoch is history: only the unit's newest execution is asked about.
		if origin.Epoch < latest[origin.GraphID+"\x00"+origin.UnitID] {
			continue
		}
		if err := reconcileBatchAttribution(store, batch, *origin, now); err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s feature attribution: %v", batch.ID, err)))
		}
	}
	return problems
}

func reconcileBatchAttribution(store *SQLiteOperationStore, batch orchestration.Batch, origin orchestration.BatchOrigin, now time.Time) error {
	if err := verifyAcceptanceFeatureOrigin(store, &origin); err != nil {
		return err
	}
	runID := batch.Items[0].RunID
	if _, found, err := store.Run(runID); err != nil || !found {
		return err
	}
	events, err := store.Events(runID)
	if err != nil {
		return err
	}
	projection, err := Project(events)
	if err != nil {
		return err
	}
	failure := failedFeatureObservation(projection)
	if failure == nil {
		return nil
	}
	commit, tree := failure.Commit, failure.Tree
	requestID, err := featureAttributionRequestID(origin, commit, tree)
	if err != nil {
		return err
	}
	messages, err := store.ScopeMessages(batch.ID)
	if err != nil {
		return err
	}
	request, asked, err := store.MessageByID(requestID)
	if err != nil {
		return err
	}
	if !asked {
		return admitFeatureAttributionRequest(store, batch, origin, runID, requestID, commit, tree, len(messages), now)
	}
	resolution, resolved, err := store.DecisionResolutionByRequestID(requestID)
	if err != nil || !resolved || resolution.Outcome.Value == attributionNone {
		return err
	}
	return admitAttributedFinding(store, batch, origin, runID, request, resolution, len(messages), now)
}

func admitFeatureAttributionRequest(store *SQLiteOperationStore, batch orchestration.Batch, origin orchestration.BatchOrigin,
	runID, requestID, commit, tree string, scopeSize int, now time.Time) error {
	options := make([]string, 0, len(origin.Inputs)+1)
	for _, input := range origin.Inputs {
		options = append(options, input.UnitID)
	}
	sort.Strings(options)
	if len(options) == 0 || len(options) > maxAttributedInputs {
		return fmt.Errorf("the integration consumed %d inputs; the attribution decision names between 1 and %d", len(options), maxAttributedInputs)
	}
	options = append(options, attributionNone)
	if scopeSize >= orchestration.MaxScopeMessages {
		return fmt.Errorf("scope %s holds %d messages, at its bound; the attribution decision cannot be asked and the failure stays held", batch.ID, scopeSize)
	}
	body := featureAttributionBody(origin, commit, tree)
	message := orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion, ID: requestID, Scope: batch.ID, Kind: orchestration.KindDecisionRequest,
		Source: orchestration.MessageSource{
			Unit: orchestration.BatchItemUnit(batch.Items[0].Issue), RunID: runID, OperationID: featureAttributionOperationID,
			Attempt: 1, Index: 0, System: true,
		},
		Purpose:             FeatureAttributionPurpose,
		ExpectedOutcomeKind: orchestration.DecisionSelectedOption,
		PermittedOptions:    options,
		Body:                body,
		Route:               orchestration.MessageRoute{Audience: orchestration.AudienceAuthority},
		DocumentSHA256:      epochAuthorizationDocumentDigest(requestID, body),
		AdmittedAt:          now,
	}
	// The bound is enforced in the same transaction as the insert, so a
	// concurrent delivery into this scope cannot overshoot it.
	_, err := store.AdmitDeliveredFinding(message)
	return err
}

func admitAttributedFinding(store *SQLiteOperationStore, batch orchestration.Batch, origin orchestration.BatchOrigin,
	runID string, request orchestration.EngineeringMessage, resolution orchestration.DecisionResolution, scopeSize int, now time.Time) error {
	selected := resolution.Outcome.Value
	var input *orchestration.WorkUnitInput
	for i := range origin.Inputs {
		if origin.Inputs[i].UnitID == selected {
			input = &origin.Inputs[i]
		}
	}
	if input == nil {
		return fmt.Errorf("the resolution selects %q, which is not one of the integration's consumed inputs", selected)
	}
	subject, found := handoffSubject(store, input.RunID, input.HandoffID)
	if !found {
		return fmt.Errorf("input %s's exact handoff %s is no longer readable; nothing is attributed to it", selected, input.HandoffID)
	}
	id, err := domain.Digest(struct{ Request, Unit string }{request.ID, selected})
	if err != nil {
		return err
	}
	id = "message-feature-finding-" + id[:32]
	if _, exists, err := store.MessageByID(id); err != nil || exists {
		return err
	}
	if scopeSize >= orchestration.MaxScopeMessages {
		return fmt.Errorf("scope %s holds %d messages, at its bound; the attribution to %s cannot be recorded and the failure stays held", batch.ID, scopeSize, selected)
	}
	body := strings.Join([]string{
		"The operator-declared feature acceptance " + origin.AcceptanceFeature + " failed on the integrated candidate that consumed this unit's admitted output.",
		"The operator selected this input; the runtime did not detect a defect in it.",
		request.Body,
	}, " ")
	message := orchestration.EngineeringMessage{
		SchemaVersion: orchestration.MessageSchemaVersion, ID: id, Scope: batch.ID, Kind: orchestration.KindFinding,
		Source: orchestration.MessageSource{
			Unit: orchestration.BatchItemUnit(batch.Items[0].Issue), RunID: runID, OperationID: featureAttributionOperationID,
			Attempt: 1, Index: 1, System: true,
		},
		Subject:        &orchestration.MessageSubject{Handoff: input.HandoffID, Owner: selected, Revision: subject},
		Category:       orchestration.FindingInconsistency,
		Body:           body,
		Route:          orchestration.MessageRoute{Audience: orchestration.AudienceUnit, Unit: selected},
		DocumentSHA256: epochAuthorizationDocumentDigest(id, request.ID),
		AdmittedAt:     now,
	}
	_, err = store.AdmitDeliveredFinding(message)
	return err
}

// priorFailedFeatureVerdict finds an earlier execution of this same unit that
// already judged this exact integrated TREE and failed it. A remediation epoch
// that recomposes to the identical tree has changed nothing the tests can see,
// and verifying it again would spend a full browser run to learn the answer the
// journal already holds. The verdict it returns is only ever a failure: nothing
// here can produce a pass.
func (r *EngineeringRuntime) priorFailedFeatureVerdict(state *runState, tree string) (string, bool, error) {
	origin := state.origin
	if origin == nil || origin.AcceptanceFeature == "" || tree == "" {
		return "", false, nil
	}
	runs, err := r.deps.Store.Runs()
	if err != nil {
		return "", false, err
	}
	for _, run := range runs {
		if run.ID == state.run.ID || run.Goal != state.run.Goal || run.Orchestration == nil {
			continue
		}
		batch, found, err := r.deps.Store.OrchestrationBatch(run.Orchestration.BatchID)
		if err != nil {
			return "", false, err
		}
		if !found || batch.Origin == nil || batch.Origin.GraphID != origin.GraphID || batch.Origin.UnitID != origin.UnitID ||
			batch.Origin.AcceptanceFeature != origin.AcceptanceFeature {
			continue
		}
		events, err := r.deps.Store.Events(run.ID)
		if err != nil {
			return "", false, err
		}
		projection, err := Project(events)
		if err != nil {
			return "", false, err
		}
		if a := projection.Assurance; a != nil && !a.Passed && a.FailureClass == FailureFeatureAcceptance && a.Tree == tree && featureVerdictBase(a, state.pinnedBase()) {
			return run.ID, true, nil
		}
	}
	return "", false, nil
}

// featureVerdictBase reports whether a failed observation's feature record, when
// it has one, was made against the base the current run pins. An observation
// that carries no record is itself a carried-forward prior verdict.
func featureVerdictBase(observation *AssuranceObservation, base string) bool {
	for _, check := range observation.Checks {
		if check.Kind == CheckKindFeatureAcceptance {
			return check.BaseRevision == base && check.CheckerRevision == featureCheckerRevision()
		}
	}
	return true
}
