package reassessment_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
			Subject:          model.Subject,
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
	if !hasDeviation(result, "scope_expansion", "internal/auth/session.go") {
		t.Fatalf("security scope expansion was not recorded: %#v", result.Deviations)
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

func TestReassessRefreshesContractForChangedSubjectRevision(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	bundle := validBundle(current)

	result, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: current,
		Compile: policy.CompileInput{
			ContractID: current.ID, ContractRevision: "2", Objective: current.Objective,
			AcceptanceIntent: current.AcceptanceIntent,
			Subject:          domain.Subject{Repository: current.Subject.Repository, Revision: "rev-b"},
			ProjectModel:     model, Policy: policyFixture,
		},
		ObservedChange:    analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true},
		EvidenceBundles:   map[string]domain.EvidenceBundle{bundle.ID: bundle},
		EvidenceRevisions: map[string]string{bundle.ID: "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Material || result.Contract == nil || !hasDeviation(result, "subject_revision_changed", "rev-b") {
		t.Fatalf("changed subject revision did not refresh the contract: %#v", result)
	}
	if result.Contract.Subject.Revision != "rev-b" || result.Contract.Revision != "2" || result.Contract.Provenance.PreviousContractRevision == nil || *result.Contract.Provenance.PreviousContractRevision != current.Revision {
		t.Fatalf("revised contract has incorrect subject or provenance: %#v", result.Contract)
	}
	if !result.Suspends(current.Permissions[0]) {
		t.Fatalf("current protected action was not suspended: %#v", result.SuspendedActions)
	}
	stale := result.StaleEvidence[bundle.ID]
	if stale.Evidence["evidence-test"].Lifecycle.Status != domain.EvidenceStale {
		t.Fatalf("prior evidence was not staled: %#v", stale)
	}
	target := evidence.Binding{Subject: result.Contract.Subject, Contract: domain.ObjectRevision{ID: result.Contract.ID, Revision: result.Contract.Revision}, Policy: result.Contract.Provenance.Policy}
	has, err := evidence.HasApplicablePassingEvidence(stale, target, "claim-trivial-validation", "test_result")
	if err != nil || has {
		t.Fatalf("prior evidence was applicable to revised subject: has=%t err=%v", has, err)
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

func TestReassessTreatsSameIDChangedClaimAsMaterial(t *testing.T) {
	model := projectModel()
	base := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, base)
	candidatePolicy := base
	rule := candidatePolicy.Rules["TRIVIAL-CHANGE-001"]
	claims := *rule.Effect.RequiredClaims
	claim := claims["claim-trivial-validation"]
	claim.EvidenceClass = "independent_test_result"
	claims["claim-trivial-validation"] = claim
	rule.Effect.RequiredClaims = &claims
	candidatePolicy.Rules["TRIVIAL-CHANGE-001"] = rule

	result := reassess(t, current, model, candidatePolicy, analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true})
	if !result.Material || result.Contract == nil || !hasDeviation(result, "changed_claim", "claim-trivial-validation") {
		t.Fatalf("same-ID changed claim was not material: %#v", result)
	}
}

func TestReassessTreatsSameIDChangedRequirementAsMaterial(t *testing.T) {
	model := projectModel()
	base := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, base)
	candidatePolicy := base
	rule := candidatePolicy.Rules["TRIVIAL-CHANGE-001"]
	obligations := *rule.Effect.Obligations
	requirement := obligations["trivial-change-validation"]
	requirement.Statement = "A revised validation requirement must pass."
	obligations["trivial-change-validation"] = requirement
	rule.Effect.Obligations = &obligations
	candidatePolicy.Rules["TRIVIAL-CHANGE-001"] = rule

	result := reassess(t, current, model, candidatePolicy, analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true})
	if !result.Material || !hasDeviation(result, "changed_obligation", "trivial-change-validation") {
		t.Fatalf("same-ID changed requirement was not material: %#v", result)
	}
}

func TestReassessTreatsRemovedPermissionAsMaterial(t *testing.T) {
	model := projectModel()
	base := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, base)
	candidatePolicy := base
	rule := candidatePolicy.Rules["TRIVIAL-CHANGE-001"]
	rule.Effect.Permissions = nil
	candidatePolicy.Rules["TRIVIAL-CHANGE-001"] = rule

	result := reassess(t, current, model, candidatePolicy, analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true})
	if !result.Material || result.Contract == nil || !hasDeviation(result, "removed_permission", "git.pull_request.create\x00main") {
		t.Fatalf("removed permission was not material: %#v", result)
	}
	if len(result.Contract.Permissions) != 0 {
		t.Fatalf("revised contract retained removed permission: %#v", result.Contract.Permissions)
	}
}

func TestReassessSurfacesRequestedPrivilegeExpansionWithoutGrantingIt(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	candidatePolicy := policyFixture
	rule := candidatePolicy.Rules["AUTH-BOUNDARY-001"]
	permissions := []domain.Action{{Type: "git.merge", Target: "main"}}
	rule.Effect.Permissions = &permissions
	candidatePolicy.Rules["AUTH-BOUNDARY-001"] = rule

	result := reassess(t, current, model, candidatePolicy, analysis.ObservedChange{Paths: []string{"internal/auth/session.go"}, PathsKnown: true})
	requested := domain.Action{Type: "git.merge", Target: "main"}
	if !result.Material || result.Contract == nil || !containsAction(result.RequestedPrivilegeExpansion, requested) || !hasDeviation(result, "requested_privilege_expansion", "git.merge\x00main") {
		t.Fatalf("requested privilege expansion was not surfaced: %#v", result)
	}
	if containsAction(result.Contract.Permissions, requested) || !result.Suspends(requested) {
		t.Fatalf("requested privilege was granted or not suspended: %#v", result)
	}
	encoded, err := domain.Encode(*result.Contract)
	if err != nil {
		t.Fatalf("permission-capped contract must remain schema-valid: %v", err)
	}
	if result.Contract.Permissions == nil || !bytes.Contains(encoded, []byte(`"permissions":[]`)) {
		t.Fatalf("empty permission ceiling must encode as [], got permissions %#v in %s", result.Contract.Permissions, encoded)
	}
}

func TestReassessRejectsGovernanceBaselineChanges(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	cases := []struct {
		name   string
		model  domain.ProjectModel
		policy domain.EngineeringPolicy
		want   string
	}{
		{name: "policy", model: model, policy: func() domain.EngineeringPolicy { changed := policyFixture; changed.Revision = "2"; return changed }(), want: "policy"},
		{name: "project model", model: func() domain.ProjectModel { changed := model; changed.Revision = "2"; return changed }(), policy: policyFixture, want: "ProjectModel"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := reassessment.Reassess(reassessment.Input{CurrentContract: current, Compile: policy.CompileInput{ContractID: current.ID, ContractRevision: "2", Objective: current.Objective, AcceptanceIntent: current.AcceptanceIntent, Subject: current.Subject, ProjectModel: testCase.model, Policy: testCase.policy}, ObservedChange: analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true}})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected %s baseline rejection, got %v", testCase.want, err)
			}
		})
	}
}

func TestReassessKeepsObservedProhibitedPathOutOfAllowedScope(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	current.Scope.ProhibitedPaths = []string{"internal/secrets/**"}
	result := reassess(t, current, model, policyFixture, analysis.ObservedChange{Paths: []string{"internal/secrets/key.go"}, PathsKnown: true})
	if !result.Material || result.Contract == nil || !hasDeviation(result, "prohibited_path", "internal/secrets/key.go") {
		t.Fatalf("prohibited observed path was not material: %#v", result)
	}
	if matches(result.Contract.Scope.AllowedPaths, "internal/secrets/key.go") || !matches(result.Contract.Scope.ProhibitedPaths, "internal/secrets/key.go") {
		t.Fatalf("prohibited path escaped scope boundary: %#v", result.Contract.Scope)
	}
}

func TestReassessRejectsTraversalBeforeProhibitedPathHandling(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	current.Scope.ProhibitedPaths = []string{"internal/auth/**"}
	result, err := reassessment.Reassess(reassessment.Input{
		CurrentContract: current,
		Compile: policy.CompileInput{
			ContractID: current.ID, ContractRevision: "2", Objective: current.Objective,
			AcceptanceIntent: current.AcceptanceIntent, Subject: current.Subject,
			ProjectModel: model, Policy: policyFixture,
		},
		ObservedChange: analysis.ObservedChange{Paths: []string{"internal/worker/../auth/session.go"}, PathsKnown: true},
	})
	if err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("expected traversal rejection, got result=%#v err=%v", result, err)
	}
	if result.Contract != nil {
		t.Fatalf("invalid observed path entered a revised contract: %#v", result.Contract.Scope)
	}
}

func TestReassessRejectsStableWorkContextChanges(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	cases := []struct {
		name   string
		mutate func(*policy.CompileInput)
		want   string
	}{
		{name: "objective", mutate: func(input *policy.CompileInput) { input.Objective = "Replace engineering intent." }, want: "objective"},
		{name: "acceptance intent", mutate: func(input *policy.CompileInput) { input.AcceptanceIntent = []string{"Different acceptance."} }, want: "acceptance intent"},
		{name: "prohibited paths", mutate: func(input *policy.CompileInput) { input.Scope.ProhibitedPaths = []string{"internal/new-governance/**"} }, want: "prohibited paths"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			compileInput := policy.CompileInput{ContractID: current.ID, ContractRevision: "2", Objective: current.Objective, AcceptanceIntent: current.AcceptanceIntent, Subject: current.Subject, ProjectModel: model, Policy: policyFixture}
			testCase.mutate(&compileInput)
			_, err := reassessment.Reassess(reassessment.Input{CurrentContract: current, Compile: compileInput, ObservedChange: analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true}})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected %s rejection, got %v", testCase.want, err)
			}
		})
	}
}

func TestReassessAcceptsOrderIndependentAcceptanceIntent(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := compile(t, policy.CompileInput{ContractID: "contract-session", ContractRevision: "1", Objective: "Update documentation.", AcceptanceIntent: []string{"Documentation is clear.", "Links resolve."}, Subject: model.Subject, Scope: domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: []string{"README.md"}}, ProjectModel: model, Policy: policyFixture, Facts: []domain.EngineeringFact{fact(model.Subject, domain.StagePredicted, domain.FactFalse)}})
	result, err := reassessment.Reassess(reassessment.Input{CurrentContract: current, Compile: policy.CompileInput{ContractID: current.ID, ContractRevision: "2", Objective: current.Objective, AcceptanceIntent: []string{"Links resolve.", "Documentation is clear."}, Subject: current.Subject, ProjectModel: model, Policy: policyFixture}, ObservedChange: analysis.ObservedChange{Paths: []string{"README.md"}, PathsKnown: true}})
	if err != nil || result.Material {
		t.Fatalf("equivalent acceptance intent should remain non-material: result=%#v err=%v", result, err)
	}
}

func TestReassessAddsLegitimateObservedPathWithoutChangingStableIntent(t *testing.T) {
	model := projectModel()
	policyFixture := fixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	current := trivialContract(t, model, policyFixture)
	result := reassess(t, current, model, policyFixture, analysis.ObservedChange{Paths: []string{"internal/worker.go"}, PathsKnown: true})
	if !result.Material || result.Contract == nil || !matches(result.Contract.Scope.AllowedPaths, "internal/worker.go") {
		t.Fatalf("legitimate observed path was not incorporated: %#v", result)
	}
	if result.Contract.Objective != current.Objective || !sameStringSet(result.Contract.AcceptanceIntent, current.AcceptanceIntent) {
		t.Fatalf("stable work intent changed: %#v", result.Contract)
	}
}

func trivialContract(t *testing.T, model domain.ProjectModel, policyFixture domain.EngineeringPolicy) domain.EngineeringWorkContract {
	t.Helper()
	return compile(t, policy.CompileInput{ContractID: "contract-session", ContractRevision: "1", Objective: "Update documentation.", AcceptanceIntent: []string{"Documentation is clear."}, Subject: model.Subject, Scope: domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: []string{"README.md"}}, ProjectModel: model, Policy: policyFixture, Facts: []domain.EngineeringFact{fact(model.Subject, domain.StagePredicted, domain.FactFalse)}})
}

func reassess(t *testing.T, current domain.EngineeringWorkContract, model domain.ProjectModel, policyFixture domain.EngineeringPolicy, observed analysis.ObservedChange) reassessment.Result {
	t.Helper()
	result, err := reassessment.Reassess(reassessment.Input{CurrentContract: current, Compile: policy.CompileInput{ContractID: current.ID, ContractRevision: "2", Objective: current.Objective, AcceptanceIntent: current.AcceptanceIntent, Subject: current.Subject, ProjectModel: model, Policy: policyFixture}, ObservedChange: observed})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func containsAction(actions []domain.Action, target domain.Action) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
}

func hasDeviation(result reassessment.Result, kind, detail string) bool {
	for _, deviation := range result.Deviations {
		if deviation.Kind == kind && deviation.Detail == detail {
			return true
		}
	}
	return false
}

func matches(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if pattern == path || strings.HasSuffix(pattern, "/**") && strings.HasPrefix(path, strings.TrimSuffix(pattern, "/**")+"/") {
			return true
		}
	}
	return false
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
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
