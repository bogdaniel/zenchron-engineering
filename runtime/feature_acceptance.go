package runtime

// Operator-declared final-integration feature acceptance as verified evidence.
//
// A plan the operator declared a feature for carries it on its one terminal
// deterministic integration stage (domain.PlanStage.AcceptanceFeature); the
// stage's run records it on its verified BatchOrigin. From that origin - never
// from candidate paths - the runtime produces the fact
// acceptance.final_integration for that run only, an operator-activated policy
// rule turns the fact into the claim FeatureAcceptanceClaimID, and the claim is
// discharged only by a feature_acceptance record: the browser machinery run
// with the feature's own manifest, browser-acceptance/<feature>.txt, read from
// the trusted base together with the base's test surface, against the exact
// integrated tree. Generic green assurance and an ordinary browser record are
// different evidence and prove nothing here.

import (
	"errors"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	// FeatureAcceptanceClaimID is the one claim id the runtime recognizes for
	// feature acceptance. A policy rule on the acceptance.final_integration
	// fact that requires it (evidence class automated_test) turns the check on.
	FeatureAcceptanceClaimID = "claim-feature-acceptance"
	// FinalIntegrationFactKey is the fact the runtime derives from a verified
	// origin's declared feature.
	FinalIntegrationFactKey = "acceptance.final_integration"

	featureCheckerID   = "feature-acceptance-playwright"
	featureProviderID  = "baseline-go-feature"
	featureManifestDir = "browser-acceptance"
)

// featureManifestFile is where a feature's selected tests are listed, on the
// base revision.
func featureManifestFile(feature string) string {
	return featureManifestDir + "/" + feature + ".txt"
}

// featureCheckerRevision identifies the implementation that produced a feature
// record, so a change to how it is made invalidates earlier records.
func featureCheckerRevision() string {
	digest, _ := Digest(struct{ Name, Version string }{"feature-acceptance", "go-test-json-playwright-v1-base-pinned-feature-manifest"})
	return digest
}

// featureAcceptanceRequired reports whether the contract's compiled required
// claims include the feature claim.
func featureAcceptanceRequired(contract domain.EngineeringWorkContract) bool {
	_, required := contract.RequiredClaims[FeatureAcceptanceClaimID]
	return required
}

// declaredFeature is the feature the run's verified origin declares, or "".
func (s *runState) declaredFeature() string {
	if s.origin == nil {
		return ""
	}
	return s.origin.AcceptanceFeature
}

// featureAcceptanceFeature is the feature a contract's assurance must prove, or
// "" when the contract does not require the claim. A required claim with no
// declared feature or no base revision is refused by featureBaseRefusal, never
// skipped.
func featureAcceptanceFeature(contract domain.EngineeringWorkContract, feature string) string {
	if !featureAcceptanceRequired(contract) {
		return ""
	}
	return feature
}

func featureBaseRefusal(contract domain.EngineeringWorkContract, feature, base string) error {
	if !featureAcceptanceRequired(contract) {
		return nil
	}
	if feature == "" {
		return errors.New("the contract requires feature acceptance but this run's verified origin declares no feature; refusing to verify without it")
	}
	if base == "" {
		return errors.New("the contract requires feature acceptance but the run has no base revision to read its tests from; refusing to verify without it")
	}
	return nil
}

func featureSpec(request AssuranceRequest) browserCheckSpec {
	return browserCheckSpec{
		base: request.FeatureAcceptanceBase, manifest: featureManifestFile(request.FeatureAcceptance), feature: request.FeatureAcceptance,
		kind: CheckKindFeatureAcceptance, checker: featureCheckerID, checkerRevision: featureCheckerRevision(),
		provider: featureProviderID, workDir: "feature", failureClass: FailureFeatureAcceptance,
	}
}

// featureCheckProves reports whether the observation PROVES feature acceptance
// for this exact integrated candidate: a completed, passing feature record by
// the current checker for this declared feature, bound to the observation's own
// commit and tree and to the base whose manifest and tests ran, in which every
// selected test passed, none failed or skipped, and the tests left artifacts.
// An ordinary browser record is a different kind and never qualifies.
func featureCheckProves(observation *AssuranceObservation, commit, base, feature string) bool {
	if observation == nil || commit == "" || base == "" || feature == "" {
		return false
	}
	for _, check := range observation.Checks {
		results := check.Results
		if check.Kind == CheckKindFeatureAcceptance && check.Outcome == CheckOutcomePassed &&
			check.Feature == feature &&
			check.Checker == featureCheckerID && check.CheckerRevision == featureCheckerRevision() &&
			check.BaseRevision == base && check.Selection != "" && check.Surface != "" && check.ArtifactsSHA256 != "" &&
			check.Environment != "" && check.Artifact != "" &&
			check.Commit == commit && check.Commit == observation.Commit && check.Tree != "" && check.Tree == observation.Tree &&
			results != nil && results.Total > 0 && results.Passed == results.Total && results.Failed == 0 && results.Skipped == 0 {
			return true
		}
	}
	return false
}

// featureEvidenceItem turns the generic passing-assurance item for the feature
// claim into the evidence that actually proves it, or reports that none can be
// produced.
func featureEvidenceItem(item domain.EvidenceItem, observation *AssuranceObservation, commit, base, feature string) (domain.EvidenceItem, bool) {
	if !featureCheckProves(observation, commit, base, feature) {
		return domain.EvidenceItem{}, false
	}
	for _, check := range observation.Checks {
		if check.Kind == CheckKindFeatureAcceptance && check.Outcome == CheckOutcomePassed && check.Feature == feature {
			item.Producer = domain.EvidenceProducer{ID: featureCheckerID, Type: domain.ProducerDeterministicTool}
			item.Environment = domain.EvidenceEnvironment{Type: "assurance_provider", Identifier: check.Environment}
			return item, true
		}
	}
	return domain.EvidenceItem{}, false
}

// featureBindingSuffix extends the assurance operation binding when, and only
// when, the run's contract requires the feature claim: a newly required check
// is planned, and a changed base, feature or checker makes it eligible again.
// A run whose contract does not require it keeps the binding it always had.
func (s *runState) featureBindingSuffix() string {
	feature := s.declaredFeature()
	if s.rt == nil || feature == "" {
		return ""
	}
	contract, err := s.rt.contractFor(s)
	if err != nil || !featureAcceptanceRequired(contract) {
		return ""
	}
	return featureBindingSuffixFor(feature, s.pinnedBase())
}

// featureBindingSuffixFor is the pure suffix: it changes with the declared
// feature, the pinned base (whose manifest and tests ran) and the checker.
func featureBindingSuffixFor(feature, base string) string {
	return "|feature-acceptance@" + fileSizeBindingDigest(featureCheckerRevision()+"\x00"+feature, base)
}

// declaredFeatureDetector produces FinalIntegrationFactKey from a verified
// origin's declared feature. It is the only source of the fact: no path, issue
// text, provider output or candidate content reaches it.
type declaredFeatureDetector struct{ feature string }

func (d declaredFeatureDetector) Detect(_ domain.ProjectModel, input analysis.Input) ([]domain.EngineeringFact, error) {
	if d.feature == "" {
		return nil, nil
	}
	return []domain.EngineeringFact{{
		SchemaVersion: domain.SchemaVersion,
		ID:            "fact-acceptance-final-integration-" + string(input.Stage),
		Key:           FinalIntegrationFactKey,
		Value:         domain.FactTrue,
		Stage:         input.Stage,
		Confidence:    domain.ConfidenceHigh,
		Subject:       input.Subject,
		Provenance:    domain.FactProvenance{Type: "approved_plan", Producer: "feature-acceptance-declaration"},
	}}, nil
}

// flowFor is the kernel flow a run compiles its contracts with: the shared flow,
// plus the declaration detector for a run whose verified origin declares a
// feature. The same analyzer serves the predicted and the observed compilation,
// so reassessment cannot lose the fact.
func (r *EngineeringRuntime) flowFor(state *runState) KernelFlow {
	feature := state.declaredFeature()
	if feature == "" {
		return r.flow
	}
	return KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(analysis.CriticalBoundaryDetector{}, declaredFeatureDetector{feature: feature})}
}

// featureAcceptanceBase is the base revision whose feature manifest and test
// sources the assurance must run, or "" when the contract does not require the
// claim.
func featureAcceptanceBase(contract domain.EngineeringWorkContract, base string) string {
	if !featureAcceptanceRequired(contract) {
		return ""
	}
	return base
}
