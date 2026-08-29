package authority_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/authority"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestEvaluateDecisionStates(t *testing.T) {
	tests := []struct {
		name       string
		contract   string
		action     domain.Action
		capability domain.CapabilityStatus
		bundles    []string
		want       domain.AuthorityStatus
	}{
		{
			name: "authorized PR with no action claims", contract: "trivial.engineering-work-contract.json",
			action: domain.Action{Type: "git.pull_request.create", Target: "main"}, capability: domain.CapabilityAvailable,
			want: domain.AuthorityAuthorized,
		},
		{
			name: "incomplete missing technical evidence", contract: "security-sensitive.engineering-work-contract.json",
			action: domain.Action{Type: "git.merge", Target: "main"}, capability: domain.CapabilityAvailable,
			want: domain.AuthorityIncomplete,
		},
		{
			name: "blocked by failed assurance", contract: "security-sensitive.engineering-work-contract.json",
			action: domain.Action{Type: "git.merge", Target: "main"}, capability: domain.CapabilityAvailable,
			bundles: []string{"failed-assurance.evidence-bundle.json"}, want: domain.AuthorityBlocked,
		},
		{
			name: "stale evidence", contract: "security-sensitive.engineering-work-contract.json",
			action: domain.Action{Type: "git.merge", Target: "main"}, capability: domain.CapabilityAvailable,
			bundles: []string{"stale-evidence.evidence-bundle.json"}, want: domain.AuthorityStale,
		},
		{
			name: "awaiting human approval", contract: "security-sensitive.engineering-work-contract.json",
			action: domain.Action{Type: "git.merge", Target: "main"}, capability: domain.CapabilityAvailable,
			bundles: []string{"security-sensitive.evidence-bundle.json"}, want: domain.AuthorityAwaitingAuthority,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := evaluate(t, inputFor(t, test.contract, test.action, test.capability, test.bundles...))
			if decision.Status != test.want {
				t.Fatalf("status = %q, want %q", decision.Status, test.want)
			}
		})
	}
}

func TestEvaluateSameEvidenceCanAuthorizePRButLeaveMergeAwaitingAuthority(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	prAction := domain.Action{Type: "git.pull_request.create", Target: "main"}
	mergeAction := domain.Action{Type: "git.merge", Target: "main"}
	contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
		Action:         prAction,
		RequiredClaims: []string{"claim-auth-regression-tests", "claim-security-review"},
	})

	pr := evaluate(t, authority.Input{
		DecisionID: "decision-pr", DecisionRevision: "1", Contract: contract, Action: prAction,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	merge := evaluate(t, authority.Input{
		DecisionID: "decision-merge", DecisionRevision: "1", Contract: contract, Action: mergeAction,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	if pr.Status != domain.AuthorityAuthorized || merge.Status != domain.AuthorityAwaitingAuthority {
		t.Fatalf("PR = %q, merge = %q; want authorized and awaiting_authority", pr.Status, merge.Status)
	}
}

func TestEvaluateCannotBypassDeniedPermission(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	action := domain.Action{Type: "deploy.production", Target: "payments"}
	decision := evaluate(t, authority.Input{
		DecisionID: "decision-deploy", DecisionRevision: "1", Contract: contract, Action: action,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	if decision.Status != domain.AuthorityBlocked || decision.Permission.Status != domain.PermissionDenied || !contains(decision.Blocking, "permission:deploy.production:payments") {
		t.Fatalf("decision = %#v, want denied permission block", decision)
	}
}

func TestEvaluateRejectsChangeProducerAsSoleIndependentEvidence(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	item := bundle.Evidence["evidence-auth-tests-passed"]
	item.Producer = changeProducer()
	bundle.Evidence["evidence-auth-tests-passed"] = item
	decision := evaluate(t, authority.Input{
		DecisionID: "decision-independent", DecisionRevision: "1", Contract: contract,
		Action: domain.Action{Type: "git.merge", Target: "main"}, Capability: domain.CapabilityAvailable,
		ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	if decision.Status != domain.AuthorityIncomplete || !contains(decision.Missing, "claim-auth-regression-tests") {
		t.Fatalf("decision = %#v, want incomplete independent test evidence", decision)
	}
}

func TestEvaluateRecordsChangeProducerUsedForIndependence(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	bundle.Evidence["evidence-security-owner-approval"] = humanApproval("security-owner-1")
	action := domain.Action{Type: "git.merge", Target: "main"}

	independent := evaluate(t, authority.Input{
		DecisionID: "decision-independent-producer", DecisionRevision: "1", Contract: contract, Action: action,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	changeProducerIsTestTool := bundle.Evidence["evidence-auth-tests-passed"].Producer
	dependent := evaluate(t, authority.Input{
		DecisionID: "decision-dependent-producer", DecisionRevision: "1", Contract: contract, Action: action,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducerIsTestTool, EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})

	if independent.Status != domain.AuthorityAuthorized {
		t.Fatalf("independent producer status = %q, want authorized", independent.Status)
	}
	if dependent.Status != domain.AuthorityIncomplete || !contains(dependent.Missing, "claim-auth-regression-tests") {
		t.Fatalf("dependent producer decision = %#v, want incomplete with missing independent test evidence", dependent)
	}
	if independent.Basis.ChangeProducer != changeProducer() {
		t.Fatalf("independent basis producer = %#v, want %#v", independent.Basis.ChangeProducer, changeProducer())
	}
	if dependent.Basis.ChangeProducer != changeProducerIsTestTool {
		t.Fatalf("dependent basis producer = %#v, want %#v", dependent.Basis.ChangeProducer, changeProducerIsTestTool)
	}
}

func TestEvaluateHumanApprovalRequiresHumanProducer(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	bundle.Evidence["evidence-security-owner-approval"] = humanApproval("approval-bot")
	approval := bundle.Evidence["evidence-security-owner-approval"]
	approval.Producer.Type = domain.ProducerDeterministicTool
	bundle.Evidence["evidence-security-owner-approval"] = approval

	decision := evaluate(t, authority.Input{
		DecisionID: "decision-non-human-approval", DecisionRevision: "1", Contract: contract,
		Action: domain.Action{Type: "git.merge", Target: "main"}, Capability: domain.CapabilityAvailable,
		ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
	})
	if decision.Status != domain.AuthorityAwaitingAuthority || !contains(decision.Missing, "claim-security-owner-approval") || contains(decision.Satisfied, "claim-security-owner-approval") {
		t.Fatalf("decision = %#v, want non-human approval to remain awaiting authority", decision)
	}
}

func TestEvaluateSecurityMergeRequiresTechnicalClaimsAndHumanApproval(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	action := domain.Action{Type: "git.merge", Target: "main"}

	t.Run("all required claims", func(t *testing.T) {
		bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
		bundle.Evidence["evidence-security-owner-approval"] = humanApproval("security-owner-1")
		decision := evaluate(t, authority.Input{
			DecisionID: "decision-all-required-claims", DecisionRevision: "1", Contract: contract, Action: action,
			Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
		})
		if decision.Status != domain.AuthorityAuthorized {
			t.Fatalf("status = %q, want authorized", decision.Status)
		}
	})

	t.Run("typed human approval without technical claims", func(t *testing.T) {
		bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
		delete(bundle.Evidence, "evidence-auth-tests-passed")
		delete(bundle.Evidence, "evidence-security-review-approved")
		bundle.Evidence["evidence-security-owner-approval"] = humanApproval("security-owner-1")
		decision := evaluate(t, authority.Input{
			DecisionID: "decision-missing-technical-claims", DecisionRevision: "1", Contract: contract, Action: action,
			Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
		})
		if decision.Status != domain.AuthorityIncomplete || !contains(decision.Missing, "claim-auth-regression-tests") || !contains(decision.Missing, "claim-security-review") {
			t.Fatalf("decision = %#v, want incomplete missing technical claims", decision)
		}
	})
}

func TestEvaluateIsDeterministicAndRecordsAllEvidenceRevisions(t *testing.T) {
	input := inputFor(t, "security-sensitive.engineering-work-contract.json", domain.Action{Type: "git.merge", Target: "main"}, domain.CapabilityAvailable, "security-sensitive.evidence-bundle.json")
	first := evaluate(t, input)
	second := evaluate(t, input)
	firstJSON, err := domain.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := domain.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) || !reflect.DeepEqual(first, second) {
		t.Fatal("same evaluator state did not produce the same decision")
	}
	if revision := first.Basis.EvidenceBundles["evidence-auth-review"].Revision; revision != "1" {
		t.Fatalf("basis revision = %q, want 1", revision)
	}
}

func TestEvaluateFailingEvidenceWinsOverPassingEvidenceIndependentlyOfMapOrder(t *testing.T) {
	input := inputFor(t, "security-sensitive.engineering-work-contract.json", domain.Action{Type: "git.merge", Target: "main"}, domain.CapabilityAvailable, "security-sensitive.evidence-bundle.json", "failed-assurance.evidence-bundle.json")
	passing := input.EvidenceBundles["evidence-auth-review"]
	failed := input.EvidenceBundles["evidence-auth-failed"]
	failure := failed.Evidence["evidence-auth-tests-failed"]
	failure.ClaimID = "claim-auth-regression-tests"
	failed.Evidence["evidence-auth-tests-failed"] = failure
	input.EvidenceBundles = map[string]domain.EvidenceBundle{
		failed.ID:  failed,
		passing.ID: passing,
	}
	decision := evaluate(t, input)
	if decision.Status != domain.AuthorityBlocked || !contains(decision.Blocking, "claim-auth-regression-tests") {
		t.Fatalf("decision = %#v, want the valid failure to block", decision)
	}
}

func TestEvaluateCapabilityStates(t *testing.T) {
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	for _, test := range []struct {
		capability domain.CapabilityStatus
		status     domain.AuthorityStatus
	}{
		{domain.CapabilityUnavailable, domain.AuthorityBlocked},
		{domain.CapabilityUnknown, domain.AuthorityIncomplete},
	} {
		t.Run(string(test.capability), func(t *testing.T) {
			decision := evaluate(t, inputFor(t, "trivial.engineering-work-contract.json", action, test.capability))
			if decision.Status != test.status {
				t.Fatalf("status = %q, want %q", decision.Status, test.status)
			}
		})
	}
}

func inputFor(t *testing.T, contract string, action domain.Action, capability domain.CapabilityStatus, bundles ...string) authority.Input {
	t.Helper()
	evidenceBundles := make(map[string]domain.EvidenceBundle, len(bundles))
	for _, name := range bundles {
		bundle := evidenceFixture(t, name)
		evidenceBundles[bundle.ID] = bundle
	}
	return authority.Input{
		DecisionID: "decision-test", DecisionRevision: "1", Contract: contractFixture(t, contract), Action: action,
		Capability: capability, ChangeProducer: changeProducer(), EvidenceBundles: evidenceBundles,
	}
}

func evaluate(t *testing.T, input authority.Input) domain.AuthorityDecision {
	t.Helper()
	decision, err := authority.Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func changeProducer() domain.EvidenceProducer {
	return domain.EvidenceProducer{ID: "change-executor", Type: domain.ProducerExecutionProvider}
}

func humanApproval(id string) domain.EvidenceItem {
	return domain.EvidenceItem{
		ClaimID:       "claim-security-owner-approval",
		EvidenceClass: "human_approval",
		Producer:      domain.EvidenceProducer{ID: id, Type: domain.ProducerHuman},
		Environment:   domain.EvidenceEnvironment{Type: "code_review", Identifier: "review-record-57"},
		Result:        domain.EvidenceResult{Status: domain.EvidencePassed},
		Lifecycle:     domain.EvidenceLifecycle{Status: domain.EvidenceValid},
		Provenance:    domain.EvidenceProvenance{Source: "review-record-57", RecordedAt: "2026-08-29T10:45:00Z"},
	}
}

func contractFixture(t *testing.T, name string) domain.EngineeringWorkContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[domain.EngineeringWorkContract](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func evidenceFixture(t *testing.T, name string) domain.EvidenceBundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[domain.EvidenceBundle](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
