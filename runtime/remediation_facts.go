package runtime

// Remediation facts for WorkGraphStatus (#581 Batch 2): which run projects a
// unit's CURRENT epoch, and its derived RemediationState. Read entirely
// inside WorkGraphStatus's own single read-only transaction (workgraph_status.go),
// exactly like every other fact that projection already reads - never a
// second, separately-snapshotted query.

import (
	"database/sql"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// defaultPolicyMaxEpochs is the plan-preauthorized ceiling every unit has
// until an approved plan's own compiled EngineeringPolicy says otherwise.
// #480's plan-to-WorkGraph compiler, which would carry a real per-unit
// value through as an ordinary obligation, does not exist yet (confirmed:
// #480 is conceptual only, zero linked PRs) - wiring this constant through
// that compiler is explicitly deferred to whenever #480 ships (ADR-0008
// §15, Phase 3), not invented here. 1 means "no remediation ever" without
// an explicit human decision - the safe default for every plan compiled
// before or without this feature, matching every existing graph's actual
// behavior today exactly.
const defaultPolicyMaxEpochs = 1

// policyMaxEpochs is the one seam #480 will eventually replace.
func policyMaxEpochs(orchestration.WorkUnit) int { return defaultPolicyMaxEpochs }

// effectiveCeiling is ADR-0008 §11.1: the compiled policy ceiling, raised
// only by an explicit, per-unit, per-decision override - never by the
// plan-preauthorized path itself (AdmitRemediationAuthorization's own shape
// validation enforces that structurally; this is simply the MAX read back
// out).
func effectiveCeiling(unit orchestration.WorkUnit, authorizations []RemediationAuthorization) int {
	ceiling := policyMaxEpochs(unit)
	for _, authorization := range authorizations {
		if authorization.RaisesCeilingTo != nil && *authorization.RaisesCeilingTo > ceiling {
			ceiling = *authorization.RaisesCeilingTo
		}
	}
	return ceiling
}

// currentEpochSource is which durable row actually projects a unit's CURRENT
// epoch (ADR-0008 §3's CurrentEpochSource), plus its derived RemediationState.
type currentEpochSource struct {
	RunID            string
	BatchID          string
	InputsDigest     string
	RemediationState orchestration.RemediationState
}

// remediationUnitFacts computes currentEpochSource - the one extension
// WorkGraphStatus's existing per-unit loop needs to make (ADR-0008 §4).
//
// For a unit with no remediation activity at all - no epoch beyond 1, no
// authorization ever granted, no epoch_authorization request ever raised
// against it - this returns exactly activation's own RunID/BatchID/InputsDigest
// and RemediationNone, REGARDLESS of that unit's own outcome: an ordinary
// failed or parked run that no Finding ever named is RemediationNone, not
// exhausted_unresolved. This is deliberately checked FIRST and short-circuits
// everything else, so a unit #581 has never touched is provably unaffected
// (confirmed by TestExistingIntegrationFixtureIsUnaffectedByTheNewTables,
// Batch 1, and the full #475 suite re-run on top of this change).
func remediationUnitFacts(tx *sql.Tx, stateDir string, graphID string, unit orchestration.WorkUnit,
	activation WorkUnitActivation, now time.Time) (currentEpochSource, error) {
	source := currentEpochSource{RunID: activation.RunID, BatchID: activation.BatchID, InputsDigest: activation.InputsDigest}
	epochs, err := queryWorkUnitRemediationEpochs(tx, graphID, unit.ID)
	if err != nil {
		return currentEpochSource{}, err
	}
	authorizations, err := queryRemediationAuthorizationsFor(tx, graphID, unit.ID)
	if err != nil {
		return currentEpochSource{}, err
	}
	latest := latestAttemptEpoch(epochs)
	for _, epoch := range epochs {
		if epoch.Epoch == latest {
			source.RunID, source.BatchID, source.InputsDigest = epoch.RunID, epoch.BatchID, epoch.InputsDigest
		}
	}
	messages, err := queryScopeMessages(tx, activation.BatchID)
	if err != nil {
		return currentEpochSource{}, err
	}
	hasLivePendingDecision, dismissed, everRaised, err := epochAuthorizationDecisionFacts(tx, messages)
	if err != nil {
		return currentEpochSource{}, err
	}
	if len(epochs) == 0 && len(authorizations) == 0 && !everRaised {
		source.RemediationState = orchestration.RemediationNone
		return source, nil
	}
	item := projectOrchestrationItem(tx, stateDir, orchestration.BatchItem{Issue: unit.Issue, RunID: source.RunID}, now)
	if item.State == "" {
		// Unreadable: WorkGraphStatus's own caller already treats an
		// unreadable child as fact.Unreadable through the ordinary path;
		// RemediationNone here is inert in that case, never consulted.
		source.RemediationState = orchestration.RemediationNone
		return source, nil
	}
	termination, err := runTermination(item.Disposition)
	if err != nil {
		return currentEpochSource{}, err
	}
	outcome := orchestration.DeriveEpochOutcome(termination, item.State == orchestration.ItemCompleted)
	reached := (latest + 1) > effectiveCeiling(unit, authorizations)
	source.RemediationState = orchestration.DeriveRemediationState(outcome, hasLivePendingDecision, dismissed, reached)
	return source, nil
}

// epochAuthorizationDecisionFacts scans one unit's own batch scope for every
// live epoch_authorization decision_request (#581), reporting whether any
// is still unresolved, whether any resolved as dismiss, and whether any
// exists at all - this last fact is what lets remediationUnitFacts tell "no
// remediation activity ever" apart from "activity exists but settled",
// without recomputing an epoch number this message never carried in the
// first place (its own Subject already binds it to whichever handoff was
// current when it was raised; a stale one naturally stops mattering once a
// later epoch supersedes it, the same AppliesTo discipline #579 already
// established).
func epochAuthorizationDecisionFacts(tx *sql.Tx, messages []orchestration.EngineeringMessage) (hasLivePendingDecision, dismissed, everRaised bool, err error) {
	for _, message := range orchestration.Live(messages) {
		if message.Kind != orchestration.KindDecisionRequest || message.Purpose != orchestration.EpochAuthorizationPurpose {
			continue
		}
		everRaised = true
		resolution, found, err := decisionResolutionByRequestID(tx, message.ID)
		if err != nil {
			return false, false, false, err
		}
		if !found {
			hasLivePendingDecision = true
			continue
		}
		if resolution.Outcome.Value == orchestration.EpochAuthorizationDismiss {
			dismissed = true
		}
	}
	return hasLivePendingDecision, dismissed, everRaised, nil
}
