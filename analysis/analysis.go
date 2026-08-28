// Package analysis derives engineering facts from normalized intent and
// observed repository changes. It contains deterministic detectors only;
// probabilistic classifiers can be added later behind the Detector interface.
package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const boundaryDetectorProducer = "critical-boundary-detector-v1"

// Intent is the normalized, repository-relevant portion of engineering intent.
// PathsKnown distinguishes a known empty path set from intent whose affected
// paths could not be established deterministically.
type Intent struct {
	Objective        string
	AcceptanceIntent []string
	AffectedPaths    []string
	PathsKnown       bool
}

// ObservedChange is the normalized repository change presented to detectors.
type ObservedChange struct {
	Paths      []string
	PathsKnown bool
}

// Input is the stage-specific input shared by fact detectors.
type Input struct {
	Subject    domain.Subject
	Stage      domain.Stage
	Paths      []string
	PathsKnown bool
}

// Detector produces attributable facts from a ProjectModel and normalized
// stage input. A detector must not omit a material fact merely because it is
// unable to resolve it; it should emit FactUnknown instead.
type Detector interface {
	Detect(domain.ProjectModel, Input) ([]domain.EngineeringFact, error)
}

// FactSet is an identity-keyed set suitable as policy-resolver input.
type FactSet map[string]domain.EngineeringFact

// Sorted returns facts ordered by durable fact identity.
func (s FactSet) Sorted() []domain.EngineeringFact {
	ids := make([]string, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	facts := make([]domain.EngineeringFact, 0, len(ids))
	for _, id := range ids {
		facts = append(facts, s[id])
	}
	return facts
}

// Analyzer runs a deterministic set of fact detectors.
type Analyzer struct {
	detectors []Detector
}

// NewAnalyzer constructs the baseline v0.1 deterministic analyzer.
func NewAnalyzer() Analyzer {
	return Analyzer{detectors: []Detector{CriticalBoundaryDetector{}}}
}

// NewAnalyzerWithDetectors constructs an analyzer with explicitly supplied
// detectors, primarily for extension and testing.
func NewAnalyzerWithDetectors(detectors ...Detector) Analyzer {
	return Analyzer{detectors: append([]Detector(nil), detectors...)}
}

// Predict derives predicted facts from normalized engineering intent.
func (a Analyzer) Predict(model domain.ProjectModel, subject domain.Subject, intent Intent) (FactSet, error) {
	if subject != model.Subject {
		return nil, fmt.Errorf("predicted analysis subject %q@%q does not match ProjectModel subject %q@%q", subject.Repository, subject.Revision, model.Subject.Repository, model.Subject.Revision)
	}
	return a.analyze(model, Input{
		Subject: subject, Stage: domain.StagePredicted,
		Paths: intent.AffectedPaths, PathsKnown: intent.PathsKnown,
	})
}

// Observe derives observed facts from an actual repository change.
func (a Analyzer) Observe(model domain.ProjectModel, subject domain.Subject, change ObservedChange) (FactSet, error) {
	return a.analyze(model, Input{
		Subject: subject, Stage: domain.StageObserved,
		Paths: change.Paths, PathsKnown: change.PathsKnown,
	})
}

func (a Analyzer) analyze(model domain.ProjectModel, input Input) (FactSet, error) {
	if input.Subject.Repository == "" || input.Subject.Revision == "" {
		return nil, fmt.Errorf("analysis subject requires repository and revision")
	}
	if model.Subject.Repository != input.Subject.Repository {
		return nil, fmt.Errorf("analysis subject repository %q does not match ProjectModel repository %q", input.Subject.Repository, model.Subject.Repository)
	}
	set := make(FactSet)
	for _, detector := range a.detectors {
		facts, err := detector.Detect(model, input)
		if err != nil {
			return nil, err
		}
		for _, fact := range facts {
			if fact.Stage != input.Stage || fact.Subject != input.Subject {
				return nil, fmt.Errorf("fact %q is not bound to analysis stage and subject", fact.ID)
			}
			if _, err := domain.Encode(fact); err != nil {
				return nil, fmt.Errorf("detector produced invalid fact %q: %w", fact.ID, err)
			}
			if _, exists := set[fact.ID]; exists {
				return nil, fmt.Errorf("duplicate fact identity %q", fact.ID)
			}
			set[fact.ID] = fact
		}
	}
	return set, nil
}

// CriticalBoundaryDetector compares normalized paths with every configured
// critical boundary. Boundaries of the same type are combined into one fact.
type CriticalBoundaryDetector struct{}

// Detect implements Detector.
func (CriticalBoundaryDetector) Detect(model domain.ProjectModel, input Input) ([]domain.EngineeringFact, error) {
	if input.Stage != domain.StagePredicted && input.Stage != domain.StageObserved {
		return nil, fmt.Errorf("critical boundary detector does not support stage %q", input.Stage)
	}
	pathsByType := make(map[string][]string)
	if model.CriticalBoundaries != nil {
		for _, boundary := range *model.CriticalBoundaries {
			pathsByType[boundary.Type] = append(pathsByType[boundary.Type], boundary.Paths...)
		}
	}
	types := make([]string, 0, len(pathsByType))
	for boundaryType := range pathsByType {
		types = append(types, boundaryType)
	}
	sort.Strings(types)

	facts := make([]domain.EngineeringFact, 0, len(types))
	provenanceType := "path_analysis"
	if input.Stage == domain.StagePredicted {
		provenanceType = "intent_analysis"
	}
	for _, boundaryType := range types {
		value := domain.FactFalse
		confidence := domain.ConfidenceHigh
		if !input.PathsKnown {
			value = domain.FactUnknown
			confidence = domain.ConfidenceLow
		} else if intersects(input.Paths, pathsByType[boundaryType]) {
			value = domain.FactTrue
		}
		key := boundaryType + ".boundary_modified"
		facts = append(facts, domain.EngineeringFact{
			SchemaVersion: domain.SchemaVersion,
			ID:            factID(input.Stage, key),
			Key:           key,
			Value:         value,
			Stage:         input.Stage,
			Confidence:    confidence,
			Subject:       input.Subject,
			Provenance: domain.FactProvenance{
				Type: provenanceType, Producer: boundaryDetectorProducer,
			},
		})
	}
	return facts, nil
}

func factID(stage domain.Stage, key string) string {
	replacer := strings.NewReplacer(".", "-", "_", "-", "/", "-")
	return "fact-" + replacer.Replace(key) + "-" + string(stage)
}

func intersects(changedPaths, patterns []string) bool {
	for _, changed := range changedPaths {
		changed = strings.TrimPrefix(changed, "./")
		for _, pattern := range patterns {
			pattern = strings.TrimPrefix(pattern, "./")
			if pattern == changed || strings.HasSuffix(pattern, "/**") && pathWithin(changed, strings.TrimSuffix(pattern, "/**")) {
				return true
			}
		}
	}
	return false
}

func pathWithin(candidate, directory string) bool {
	return candidate == directory || strings.HasPrefix(candidate, strings.TrimSuffix(directory, "/")+"/")
}
