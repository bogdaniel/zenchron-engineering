package analysis_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestLoadProjectModel(t *testing.T) {
	model, err := analysis.LoadProjectModel(filepath.Join("..", "fixtures", "v0.1", "valid", "security-sensitive.project-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	if model.Subject.Repository != "acme/payments" || model.Revision != "1" {
		t.Fatalf("unexpected snapshot: %#v", model)
	}
}

func TestCriticalBoundaryFacts(t *testing.T) {
	model := projectModel()
	analyzer := analysis.NewAnalyzer()
	tests := []struct {
		name       string
		paths      []string
		pathsKnown bool
		want       domain.FactValue
		confidence domain.Confidence
	}{
		{name: "security sensitive", paths: []string{"internal/auth/session.go"}, pathsKnown: true, want: domain.FactTrue, confidence: domain.ConfidenceHigh},
		{name: "trivial", paths: []string{"README.md"}, pathsKnown: true, want: domain.FactFalse, confidence: domain.ConfidenceHigh},
		{name: "material unknown", pathsKnown: false, want: domain.FactUnknown, confidence: domain.ConfidenceLow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := analyzer.Predict(model, model.Subject, analysis.Intent{AffectedPaths: test.paths, PathsKnown: test.pathsKnown})
			if err != nil {
				t.Fatal(err)
			}
			fact := factByKey(t, facts, "authentication.boundary_modified")
			if fact.Value != test.want || fact.Confidence != test.confidence {
				t.Fatalf("value/confidence = %q/%q, want %q/%q", fact.Value, fact.Confidence, test.want, test.confidence)
			}
			if fact.Stage != domain.StagePredicted || fact.Subject != model.Subject {
				t.Fatalf("fact binding changed: %#v", fact)
			}
			if fact.Provenance.Type != "intent_analysis" || fact.Provenance.Producer == "" {
				t.Fatalf("missing deterministic provenance: %#v", fact.Provenance)
			}
		})
	}
}

func TestObservedFactsBindCandidateRevision(t *testing.T) {
	model := projectModel()
	subject := domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"}
	facts, err := analysis.NewAnalyzer().Observe(model, subject, analysis.ObservedChange{
		Paths: []string{"internal/auth/handler.go"}, PathsKnown: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fact := factByKey(t, facts, "authentication.boundary_modified")
	if fact.Stage != domain.StageObserved || fact.Subject != subject || fact.Value != domain.FactTrue || fact.Provenance.Type != "path_analysis" {
		t.Fatalf("unexpected observed fact: %#v", fact)
	}
}

func TestObservedPathsUseCanonicalRepositorySemantics(t *testing.T) {
	model := projectModel()
	subject := domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"}
	facts, err := analysis.NewAnalyzer().Observe(model, subject, analysis.ObservedChange{
		Paths: []string{"internal/auth/session.go"}, PathsKnown: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if factByKey(t, facts, "authentication.boundary_modified").Value != domain.FactTrue {
		t.Fatalf("canonical sensitive path did not intersect authentication boundary: %#v", facts)
	}
}

func TestObservedPathsRejectUnsafeRepositorySpellings(t *testing.T) {
	model := projectModel()
	subject := domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"}
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "traversal", path: "internal/worker/../auth/session.go", want: "traversal"},
		{name: "repository escape", path: "../outside.go", want: "traversal"},
		{name: "absolute", path: "/tmp/outside.go", want: "repository-relative"},
		{name: "windows volume", path: "C:/outside.go", want: "repository-relative"},
		{name: "empty", path: "", want: "not a repository file"},
		{name: "dot", path: ".", want: "not a repository file"},
		{name: "backslash", path: "internal\\auth\\session.go", want: "backslash"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := analysis.NewAnalyzer().Observe(model, subject, analysis.ObservedChange{Paths: []string{test.path}, PathsKnown: true})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q rejection, got %v", test.want, err)
			}
		})
	}
}

func TestNormalizeObservedChangeCanonicalizesLeadingDotSlash(t *testing.T) {
	normalized, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: []string{"./internal/auth/session.go"}, PathsKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Paths) != 1 || normalized.Paths[0] != "internal/auth/session.go" {
		t.Fatalf("unexpected canonical paths: %#v", normalized.Paths)
	}
}

func TestPredictedUnknownUsesIntentAnalysisProvenance(t *testing.T) {
	model := projectModel()
	facts, err := analysis.NewAnalyzer().Predict(model, model.Subject, analysis.Intent{PathsKnown: false})
	if err != nil {
		t.Fatal(err)
	}
	fact := factByKey(t, facts, "sensitive_data.boundary_modified")
	if fact.Value != domain.FactUnknown || fact.Provenance.Type != "intent_analysis" {
		t.Fatalf("unexpected predicted unknown: %#v", fact)
	}
}

func TestAnalyzerRejectsPredictedRevisionMismatch(t *testing.T) {
	model := projectModel()
	subject := domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"}
	_, err := analysis.NewAnalyzer().Predict(model, subject, analysis.Intent{PathsKnown: true})
	if err == nil {
		t.Fatal("expected predicted revision mismatch to fail")
	}
}

func TestAnalyzerRejectsRepositoryMismatch(t *testing.T) {
	model := projectModel()
	_, err := analysis.NewAnalyzer().Predict(model, domain.Subject{Repository: "other/repo", Revision: "rev-a"}, analysis.Intent{PathsKnown: true})
	if err == nil {
		t.Fatal("expected repository mismatch to fail")
	}
}

func TestAnalyzerRejectsDuplicateFactIDs(t *testing.T) {
	detector := staticDetector{}
	_, err := analysis.NewAnalyzerWithDetectors(detector, detector).Predict(projectModel(), projectModel().Subject, analysis.Intent{PathsKnown: true})
	if err == nil {
		t.Fatal("expected duplicate fact identity to fail")
	}
}

type staticDetector struct{}

func (staticDetector) Detect(_ domain.ProjectModel, input analysis.Input) ([]domain.EngineeringFact, error) {
	return []domain.EngineeringFact{{
		SchemaVersion: domain.SchemaVersion,
		ID:            "fact-duplicate",
		Key:           "duplicate.test",
		Value:         domain.FactFalse,
		Stage:         input.Stage,
		Confidence:    domain.ConfidenceHigh,
		Subject:       input.Subject,
		Provenance: domain.FactProvenance{
			Type: "test", Producer: "test-detector",
		},
	}}, nil
}

func factByKey(t *testing.T, facts analysis.FactSet, key string) domain.EngineeringFact {
	t.Helper()
	for _, fact := range facts {
		if fact.Key == key {
			return fact
		}
	}
	t.Fatalf("fact %q not found in %#v", key, facts)
	return domain.EngineeringFact{}
}

func projectModel() domain.ProjectModel {
	boundaries := map[string]domain.CriticalBoundary{
		"authentication": {Type: "authentication", Paths: []string{"internal/auth/**"}},
		"payment-data":   {Type: "sensitive_data", Paths: []string{"internal/payments/**"}},
	}
	return domain.ProjectModel{
		SchemaVersion: domain.SchemaVersion,
		ID:            "project-acme-payments", Revision: "1",
		Subject:            domain.Subject{Repository: "acme/payments", Revision: "rev-a"},
		CriticalBoundaries: &boundaries,
	}
}
