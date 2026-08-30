package runtime

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestKernelFlowHiddenScopeStalesEvidenceAndPrivilegeWaits(t *testing.T) {
	model, policy := runtimeGovernance()
	flow := KernelFlow{}
	state, err := flow.Compile(SourceSnapshot{ID: "issue-1", Objective: "change", AcceptanceIntent: []string{"works"}, PredictedPaths: []string{"README.md"}, PathsKnown: true}, model, policy, "contract-1", "1")
	if err != nil {
		t.Fatal(err)
	}
	state.Evidence["bundle"] = boundEvidence(state.Contract)
	state, err = flow.ObserveCandidate(state, model, policy, domain.Subject{Repository: model.Subject.Repository, Revision: "candidate-1"}, []string{"internal/auth/session.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Reassessment.Material || len(state.Reassessment.RequestedPrivilegeExpansion) == 0 {
		t.Fatalf("expected governed hidden scope privilege request: %#v", state.Reassessment)
	}
	if d, reason := NextDisposition(state); d != Waiting || reason != "requested_privilege_expansion" {
		t.Fatal(d, reason)
	}
	if got := state.Evidence["bundle"].Evidence["approval"].Lifecycle.Status; got != domain.EvidenceStale {
		t.Fatalf("old exact-bound evidence was not stale: %s", got)
	}
}
func TestKernelFlowRevisionRefreshAwaitsAuthorityWithoutRemediation(t *testing.T) {
	model, policy := runtimeGovernance()
	flow := KernelFlow{}
	state, err := flow.Compile(SourceSnapshot{ID: "issue-2", Objective: "change auth", AcceptanceIntent: []string{"works"}, PredictedPaths: []string{"internal/auth/session.go"}, PathsKnown: true}, model, policy, "contract-2", "1")
	if err != nil {
		t.Fatal(err)
	}
	state, err = flow.ObserveCandidate(state, model, policy, domain.Subject{Repository: model.Subject.Repository, Revision: "candidate-2"}, []string{"internal/auth/session.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Reassessment.Material || state.Contract.Subject.Revision != "candidate-2" {
		t.Fatalf("expected exact subject revision refresh: %#v", state.Reassessment)
	}
	state, err = flow.Decide(state, domain.Action{Type: "git.merge", Target: "main"}, domain.EvidenceProducer{ID: "producer", Type: domain.ProducerExecutionProvider})
	if err != nil {
		t.Fatal(err)
	}
	if state.Decision.Status != domain.AuthorityAwaitingAuthority {
		t.Fatalf("want awaiting authority, got %s", state.Decision.Status)
	}
	if err := RequireNoRemediationForAuthority(state); err != nil {
		t.Fatal(err)
	}
}
func boundEvidence(contract domain.EngineeringWorkContract) domain.EvidenceBundle {
	return domain.EvidenceBundle{SchemaVersion: domain.SchemaVersion, ID: "bundle", Revision: "1", Subject: contract.Subject, Contract: domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}, Policy: contract.Provenance.Policy, Evidence: map[string]domain.EvidenceItem{"approval": {ClaimID: "approval", EvidenceClass: "human_approval", Producer: domain.EvidenceProducer{ID: "human", Type: domain.ProducerHuman}, Environment: domain.EvidenceEnvironment{Type: "review", Identifier: "one"}, Result: domain.EvidenceResult{Status: domain.EvidencePassed}, Lifecycle: domain.EvidenceLifecycle{Status: domain.EvidenceValid}, Provenance: domain.EvidenceProvenance{Source: "review", RecordedAt: "2026-08-29T00:00:00Z"}}}}
}
func runtimeGovernance() (domain.ProjectModel, domain.EngineeringPolicy) {
	boundaries := map[string]domain.CriticalBoundary{"auth": {Type: "authentication", Paths: []string{"internal/auth/**"}}}
	model := domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "model", Revision: "1", Subject: domain.Subject{Repository: "acme/repo", Revision: "base"}, CriticalBoundaries: &boundaries}
	claims := map[string]domain.RequiredClaim{"approval": {EvidenceClass: "human_approval", IndependentFromChangeProducer: true}}
	obligations := map[string]domain.PolicyRequirement{"approval": {Statement: "human approval", RequiredClaims: &[]string{"approval"}}}
	permissions := []domain.Action{{Type: "git.merge", Target: "main"}}
	conditions := []domain.AuthorityCondition{{Action: permissions[0], RequiredClaims: []string{"approval"}}}
	stage := domain.StagePredicted
	return model, domain.EngineeringPolicy{SchemaVersion: domain.SchemaVersion, ID: "policy", Revision: "1", Rules: map[string]domain.PolicyRule{"auth": {When: domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue}, Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &obligations, Permissions: &permissions, AuthorityConditions: &conditions}}, "auth-observed": {When: domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue, Stage: &stage}, Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &obligations, Permissions: &permissions, AuthorityConditions: &conditions}}}}
}
