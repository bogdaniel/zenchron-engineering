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
	// INCOMPLETE, not awaiting authority: the contract now carries a material
	// acceptance obligation discharged by a semantic claim, and nothing has
	// produced that evidence. A human is asked only once every non-human claim
	// is satisfied - acceptance is not something an approval stands in for.
	if state.Decision.Status != domain.AuthorityIncomplete {
		t.Fatalf("want incomplete while acceptance is undischarged, got %s", state.Decision.Status)
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
	acceptanceDischarge := []string{"acceptance"}
	claims["acceptance"] = domain.RequiredClaim{EvidenceClass: SemanticEvidenceClass, IndependentFromChangeProducer: true}
	return model, domain.EngineeringPolicy{SchemaVersion: domain.SchemaVersion, ID: "policy", Revision: "1", Rules: map[string]domain.PolicyRule{"auth": {When: domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue}, Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &obligations, Permissions: &permissions, AuthorityConditions: &conditions, AcceptanceDischargeClaims: &acceptanceDischarge}}, "auth-observed": {When: domain.PolicyCondition{Fact: "authentication.boundary_modified", Equals: domain.FactTrue, Stage: &stage}, Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &obligations, Permissions: &permissions, AuthorityConditions: &conditions, AcceptanceDischargeClaims: &acceptanceDischarge}}}}
}

// After a material reassessment the old evidence keeps its original binding. It
// can no longer authorize, and it is reported as missing rather than stale; fresh
// evidence bound to the new contract is what converges the run.
func TestKernelReassessmentConvergesOnFreshEvidenceNotRetainedBindings(t *testing.T) {
	model, policy := runtimeGovernance()
	flow := KernelFlow{}
	state, err := flow.Compile(SourceSnapshot{ID: "issue-3", Objective: "change auth", AcceptanceIntent: []string{"works"}, PredictedPaths: []string{"internal/auth/session.go"}, PathsKnown: true}, model, policy, "contract-3", "1")
	if err != nil {
		t.Fatal(err)
	}
	state.Evidence["bundle"] = boundEvidence(state.Contract)
	state, err = flow.ObserveCandidate(state, model, policy, domain.Subject{Repository: model.Subject.Repository, Revision: "candidate-3"}, []string{"internal/auth/session.go"})
	if err != nil {
		t.Fatal(err)
	}
	merge := domain.Action{Type: "git.merge", Target: "main"}
	producer := domain.EvidenceProducer{ID: "producer", Type: domain.ProducerExecutionProvider}

	retained, err := flow.Decide(state, merge, producer)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Decision.Status == domain.AuthorityAuthorized || !containsString(retained.Decision.Missing, "approval") || containsString(retained.Decision.Stale, "approval") {
		t.Fatalf("retained-binding evidence must be missing and must not authorize: %#v", retained.Decision)
	}

	fresh := boundEvidence(state.Contract)
	fresh.ID = "fresh"
	state.Evidence["fresh"] = fresh
	converged, err := flow.Decide(state, merge, producer)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(converged.Decision.Missing, "approval") || containsString(converged.Decision.Stale, "approval") {
		t.Fatalf("fresh evidence bound to the new contract must satisfy the approval claim: %#v", converged.Decision)
	}
	if converged.Decision.Status == domain.AuthorityAuthorized {
		t.Fatal("the acceptance claim is still undischarged, so nothing may be authorized")
	}
}
