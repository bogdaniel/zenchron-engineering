package reassessment_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/evidence"
	"github.com/bogdaniel/zenchron-engineering/policy"
	"github.com/bogdaniel/zenchron-engineering/reassessment"
)

func TestReassessSecurityScopeExpansionRevisesContractAndSuspendsActions(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := compile(t, policy.CompileInput{
		ContractID:       "contract-session",
		ContractRevision: "1",
		Objective:        "Update documentation.",
		AcceptanceIntent: []string{"Documentation is clear."},
		Subject:          model.Subject,
		Scope:            domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: []string{"README.md"}},
		ProjectModel:     model, Policy: policyFixture,
		Facts: []domain.EngineeringFact{fact(model.Subject, domain.StagePredicted, domain.FactFalse)},
	})

	result, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: current,
		Compile: policy.CompileInput{
			ContractID:       "contract-session",
			ContractRevision: "2",
			Objective:        current.Objective,
			AcceptanceIntent: current.AcceptanceIntent,
			Subject:          domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"},
			ProjectModel:     model, Policy: policyFixture,
		},
		ObservedChange: analysis.ObservedChange{Paths: []string{"internal/auth/session.go"}, PathsKnown: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Material || result.Contract == nil {
		t.Fatalf("expected a material reassessment and new contract: %#v", result)
	}
	if result.Contract.Revision != "2" || result.Contract.Provenance.PreviousContractRevision == nil || *result.Contract.Provenance.PreviousContractRevision != "1" {
		t.Fatalf("unexpected revised contract provenance: %#v", result.Contract.Provenance)
	}
	if _, ok := result.Contract.Obligations["auth-regression-tests"]; !ok {
		t.Fatalf("security obligation was not added: %#v", result.Contract.Obligations)
	}
	if !result.Suspends(domain.Action{Type: "git.pull_request.create", Target: "main"}) || !result.Suspends(domain.Action{Type: "git.merge", Target: "main"}) {
		t.Fatalf("affected actions were not suspended: %#v", result.SuspendedActions)
	}
	if len(result.Contract.Permissions) != 0 {
		t.Fatalf("recompilation silently expanded permissions: %#v", result.Contract.Permissions)
	}

	stable, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: *result.Contract,
		Compile: policy.CompileInput{
			ContractID:       "contract-session",
			ContractRevision: "3",
			Objective:        current.Objective,
			AcceptanceIntent: current.AcceptanceIntent, Subject: result.Contract.Subject,
			ProjectModel: model, Policy: policyFixture,
		},
		ObservedChange: analysis.ObservedChange{Paths: []string{"internal/auth/session.go"}, PathsKnown: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stable.Material || stable.Contract != nil {
		t.Fatalf("the already-reassessed scope recompiled again: %#v", stable)
	}
}

func TestReassessIgnoresObservedPathsAlreadyWithinTheContract(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := compile(t, policy.CompileInput{
		ContractID:       "contract-session",
		ContractRevision: "1",
		Objective:        "Update documentation.",
		AcceptanceIntent: []string{"Documentation is clear."},
		Subject:          model.Subject,
		Scope:            domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: []string{"README.md"}},
		ProjectModel:     model, Policy: policyFixture,
		Facts: []domain.EngineeringFact{fact(model.Subject, domain.StagePredicted, domain.FactFalse)},
	})

	result, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: current,
		Compile: policy.CompileInput{
			ContractID:       "contract-session",
			ContractRevision: "2",
			Objective:        current.Objective,
			AcceptanceIntent: current.AcceptanceIntent,
			Subject:          model.Subject,
			ProjectModel:     model, Policy: policyFixture,
		},
		ObservedChange: analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Material || result.Contract != nil || len(result.SuspendedActions) != 0 {
		t.Fatalf("irrelevant in-scope observation recompiled: %#v", result)
	}
}

func TestReassessStalesPriorEvidenceAcrossContractRevision(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := compile(t, policy.CompileInput{
		ContractID:       "contract-session",
		ContractRevision: "1",
		Objective:        "Update documentation.",
		AcceptanceIntent: []string{"Documentation is clear."},
		Subject:          model.Subject,
		Scope:            domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: []string{"README.md"}},
		ProjectModel:     model, Policy: policyFixture,
		Facts: []domain.EngineeringFact{fact(model.Subject, domain.StagePredicted, domain.FactFalse)},
	})
	bundle := validBundle(current)
	result, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: current,
		Compile: policy.CompileInput{
			ContractID:       "contract-session",
			ContractRevision: "2",
			Objective:        current.Objective,
			AcceptanceIntent: current.AcceptanceIntent,
			Subject:          domain.Subject{Repository: model.Subject.Repository, Revision: "rev-b"},
			ProjectModel:     model, Policy: policyFixture,
		},
		ObservedChange:    analysis.ObservedChange{Paths: []string{"internal/auth/session.go"}, PathsKnown: true},
		EvidenceBundles:   map[string]domain.EvidenceBundle{bundle.ID: bundle},
		EvidenceRevisions: map[string]string{bundle.ID: "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := result.StaleEvidence[bundle.ID]
	if stale.Subject != bundle.Subject || stale.Contract != bundle.Contract || stale.Evidence["evidence-test"].Lifecycle.Status != domain.EvidenceStale {
		t.Fatalf("prior evidence was not retained as stale: %#v", stale)
	}
	target := evidence.Binding{Subject: result.Contract.Subject, Contract: domain.ObjectRevision{ID: result.Contract.ID, Revision: result.Contract.Revision}, Policy: result.Contract.Provenance.Policy}
	has, err := evidence.HasApplicablePassingEvidence(stale, target, "claim-trivial-validation", "test_result")
	if err != nil || has {
		t.Fatalf("stale evidence was applicable to revised contract: has=%t err=%v", has, err)
	}
}

func compile(t *testing.T, input policy.CompileInput) domain.EngineeringWorkContract {
	t.Helper()
	contract, err := policy.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func fact(subject domain.Subject, stage domain.Stage, value domain.FactValue) domain.EngineeringFact {
	return domain.EngineeringFact{SchemaVersion: domain.SchemaVersion, ID: "fact-auth-" + string(stage), Key: "authentication.boundary_modified", Value: value, Stage: stage, Confidence: domain.ConfidenceHigh, Subject: subject, Provenance: domain.FactProvenance{Type: "test", Producer: "test"}}
}

func projectModel() domain.ProjectModel {
	boundaries := map[string]domain.CriticalBoundary{"authentication": {Type: "authentication", Paths: []string{"internal/auth/**"}}}
	return domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: domain.Subject{Repository: "acme/payments", Revision: "rev-a"}, CriticalBoundaries: &boundaries}
}

func validBundle(contract domain.EngineeringWorkContract) domain.EvidenceBundle {
	return domain.EvidenceBundle{SchemaVersion: domain.SchemaVersion, ID: "bundle", Revision: "1", Subject: contract.Subject, Contract: domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}, Policy: contract.Provenance.Policy, Evidence: map[string]domain.EvidenceItem{
		"evidence-test": {ClaimID: "claim-trivial-validation", EvidenceClass: "test_result", Producer: domain.EvidenceProducer{ID: "ci", Type: domain.ProducerDeterministicTool}, Environment: domain.EvidenceEnvironment{Type: "ci", Identifier: "run-1"}, Result: domain.EvidenceResult{Status: domain.EvidencePassed}, Lifecycle: domain.EvidenceLifecycle{Status: domain.EvidenceValid}, Provenance: domain.EvidenceProvenance{Source: "run-1", RecordedAt: "2026-08-29T10:00:00Z"}},
	}}
}

func fixture[T domain.Contract](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[T](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
