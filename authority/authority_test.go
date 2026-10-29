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
			bundles: []string{"matching-stale-evidence.evidence-bundle.json"}, want: domain.AuthorityStale,
		},
		{
			name: "inapplicable stale evidence is missing", contract: "security-sensitive.engineering-work-contract.json",
			action: domain.Action{Type: "git.merge", Target: "main"}, capability: domain.CapabilityAvailable,
			bundles: []string{"stale-evidence.evidence-bundle.json"}, want: domain.AuthorityIncomplete,
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
	for _, test := range []struct {
		name         string
		producerType domain.ProducerType
		result       domain.EvidenceResultStatus
		wantStatus   domain.AuthorityStatus
		wantMissing  bool
		wantBlocking bool
	}{
		{
			name: "deterministic tool passing approval remains missing", producerType: domain.ProducerDeterministicTool,
			result: domain.EvidencePassed, wantStatus: domain.AuthorityAwaitingAuthority, wantMissing: true,
		},
		{
			name: "deterministic tool failing approval remains missing", producerType: domain.ProducerDeterministicTool,
			result: domain.EvidenceFailed, wantStatus: domain.AuthorityAwaitingAuthority, wantMissing: true,
		},
		{
			name: "human failing approval blocks", producerType: domain.ProducerHuman,
			result: domain.EvidenceFailed, wantStatus: domain.AuthorityBlocked, wantBlocking: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
			approval := humanApproval("approval-producer")
			approval.Producer.Type = test.producerType
			approval.Result.Status = test.result
			bundle.Evidence["evidence-security-owner-approval"] = approval

			decision := evaluate(t, authority.Input{
				DecisionID: "decision-human-approval", DecisionRevision: "1", Contract: contract,
				Action: domain.Action{Type: "git.merge", Target: "main"}, Capability: domain.CapabilityAvailable,
				ChangeProducer: changeProducer(), EvidenceBundles: map[string]domain.EvidenceBundle{bundle.ID: bundle},
			})
			if decision.Status != test.wantStatus || contains(decision.Missing, "claim-security-owner-approval") != test.wantMissing || contains(decision.Blocking, "claim-security-owner-approval") != test.wantBlocking {
				t.Fatalf("decision = %#v", decision)
			}
		})
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

func TestEvaluateDoesNotMutateContractRequiredClaims(t *testing.T) {
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	unsorted := []string{"claim-security-review", "claim-auth-regression-tests"}
	contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
		Action:         action,
		RequiredClaims: unsorted,
	})
	want := append([]string(nil), unsorted...)

	evaluate(t, authority.Input{
		DecisionID: "decision-no-mutation", DecisionRevision: "1", Contract: contract, Action: action,
		Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer(), EvidenceBundles: nil,
	})

	if !reflect.DeepEqual(unsorted, want) {
		t.Fatalf("caller's RequiredClaims slice was mutated: got %v, want %v", unsorted, want)
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

func TestEvaluateNonMatchingEvidenceIsMissing(t *testing.T) {
	for _, binding := range []string{"subject", "contract", "policy"} {
		for _, lifecycle := range []domain.EvidenceLifecycleStatus{domain.EvidenceValid, domain.EvidenceStale, domain.EvidenceInvalid, domain.EvidenceIncomplete} {
			t.Run(binding+"/"+string(lifecycle), func(t *testing.T) {
				input := inputFor(t, "security-sensitive.engineering-work-contract.json", domain.Action{Type: "git.merge", Target: "main"}, domain.CapabilityAvailable, "security-sensitive.evidence-bundle.json")
				for id, bundle := range input.EvidenceBundles {
					switch binding {
					case "subject":
						bundle.Subject.Revision = "other"
					case "contract":
						bundle.Contract.Revision = "other"
					case "policy":
						bundle.Policy.Revision = "other"
					}
					for itemID, item := range bundle.Evidence {
						item.Lifecycle.Status = lifecycle
						reason := "Regression test lifecycle"
						item.Lifecycle.Reason = &reason
						bundle.Evidence[itemID] = item
					}
					input.EvidenceBundles[id] = bundle
				}
				decision := evaluate(t, input)
				if decision.Status != domain.AuthorityIncomplete || len(decision.Stale) != 0 || !contains(decision.Missing, "claim-auth-regression-tests") || !contains(decision.Missing, "claim-security-review") {
					t.Fatalf("decision = %#v, want missing technical evidence without stale claims", decision)
				}
			})
		}
	}
}

// nonMatchingClone is a copy of bundle bound to another subject revision, so the
// evaluator must treat every item in it as absent whatever the item says.
func nonMatchingClone(bundle domain.EvidenceBundle, id string, edit func(domain.EvidenceItem) domain.EvidenceItem) domain.EvidenceBundle {
	clone := bundle
	clone.ID = id
	clone.Subject.Revision = "other"
	clone.Evidence = make(map[string]domain.EvidenceItem, len(bundle.Evidence))
	for itemID, item := range bundle.Evidence {
		clone.Evidence[itemID] = edit(item)
	}
	return clone
}

func mergeInput(t *testing.T, bundles ...domain.EvidenceBundle) authority.Input {
	t.Helper()
	input := inputFor(t, "security-sensitive.engineering-work-contract.json", domain.Action{Type: "git.merge", Target: "main"}, domain.CapabilityAvailable)
	for _, bundle := range bundles {
		input.EvidenceBundles[bundle.ID] = bundle
	}
	return input
}

func TestEvaluateNonMatchingFailureDoesNotBlockMatchingEvidence(t *testing.T) {
	matching := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	failing := nonMatchingClone(matching, "evidence-other-failing", func(item domain.EvidenceItem) domain.EvidenceItem {
		item.Result.Status = domain.EvidenceFailed
		return item
	})
	alone, together := evaluate(t, mergeInput(t, matching)), evaluate(t, mergeInput(t, matching, failing))
	if together.Status != alone.Status || len(together.Blocking) != 0 || len(together.Stale) != 0 {
		t.Fatalf("with a non-matching failing bundle: %#v; matching alone: %#v", together, alone)
	}
}

func TestEvaluateNonMatchingPassCannotSatisfyOrUnblockAClaim(t *testing.T) {
	passing := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	other := nonMatchingClone(passing, "evidence-other-passing", func(item domain.EvidenceItem) domain.EvidenceItem { return item })

	failedHere := passing
	failedHere.Evidence = map[string]domain.EvidenceItem{}
	for id, item := range passing.Evidence {
		if item.ClaimID == "claim-auth-regression-tests" {
			item.Result.Status = domain.EvidenceFailed
		}
		failedHere.Evidence[id] = item
	}
	blocked := evaluate(t, mergeInput(t, failedHere, other))
	if blocked.Status != domain.AuthorityBlocked || !contains(blocked.Blocking, "claim-auth-regression-tests") {
		t.Fatalf("a matching failure must still block despite a non-matching pass: %#v", blocked)
	}

	stale := evaluate(t, mergeInput(t, evidenceFixture(t, "matching-stale-evidence.evidence-bundle.json"), other))
	if stale.Status != domain.AuthorityStale || !contains(stale.Stale, "claim-auth-regression-tests") {
		t.Fatalf("matching stale evidence must stay stale and not be satisfied by a non-matching pass: %#v", stale)
	}
}

func TestEvaluateNonMatchingHumanApprovalIsStillAwaitingAuthority(t *testing.T) {
	matching := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	approval := nonMatchingClone(matching, "evidence-other-approval", func(domain.EvidenceItem) domain.EvidenceItem { return humanApproval("approver") })
	decision := evaluate(t, mergeInput(t, matching, approval))
	if decision.Status != domain.AuthorityAwaitingAuthority || !contains(decision.Missing, "claim-security-owner-approval") {
		t.Fatalf("an approval bound to another subject must not count: %#v", decision)
	}
}

// oldClaimState is the classification before a non-matching bundle was ignored:
// such a bundle's items set stale and were never read for pass or fail. It is
// the oracle for TestEvaluateReclassificationNeverChangesWhatIsAuthorized.
func oldClaimState(bundles []domain.EvidenceBundle, contract domain.EngineeringWorkContract, claimID string, claim domain.RequiredClaim, producer domain.EvidenceProducer) string {
	var hasStale, hasPassing, hasFailed bool
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			if item.ClaimID != claimID || item.EvidenceClass != claim.EvidenceClass {
				continue
			}
			if claim.EvidenceClass == "human_approval" && item.Producer.Type != domain.ProducerHuman {
				continue
			}
			matches := bundle.Subject == contract.Subject &&
				bundle.Contract == (domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision}) &&
				bundle.Policy == contract.Provenance.Policy
			if !matches || item.Lifecycle.Status == domain.EvidenceStale {
				hasStale = true
				continue
			}
			if item.Lifecycle.Status != domain.EvidenceValid {
				continue
			}
			if item.Result.Status == domain.EvidenceFailed {
				hasFailed = true
			}
			if item.Result.Status == domain.EvidencePassed && (!claim.IndependentFromChangeProducer || item.Producer.ID != producer.ID) {
				hasPassing = true
			}
		}
	}
	switch {
	case hasFailed:
		return "blocked"
	case hasPassing:
		return "satisfied"
	case hasStale:
		return "stale"
	}
	return "missing"
}

// Ignoring a non-matching bundle moves a claim only from stale to missing. Whether
// it is satisfied or blocked, and so whether any action is authorized, is the same
// for every combination of a matching and a non-matching item.
func TestEvaluateReclassificationNeverChangesWhatIsAuthorized(t *testing.T) {
	const claimID = "claim-auth-regression-tests"
	contract := contractFixture(t, "security-sensitive.engineering-work-contract.json")
	claim := contract.RequiredClaims[claimID]
	base := evidenceFixture(t, "security-sensitive.evidence-bundle.json")
	template := base.Evidence["evidence-auth-tests-passed"]
	itemWith := func(lifecycle domain.EvidenceLifecycleStatus, result domain.EvidenceResultStatus) domain.EvidenceItem {
		item := template
		item.Lifecycle = domain.EvidenceLifecycle{Status: lifecycle}
		if lifecycle != domain.EvidenceValid {
			reason := "regression"
			item.Lifecycle.Reason = &reason
		}
		item.Result.Status = result
		return item
	}
	states := map[string]*domain.EvidenceItem{"absent": nil}
	for _, lifecycle := range []domain.EvidenceLifecycleStatus{domain.EvidenceValid, domain.EvidenceStale, domain.EvidenceInvalid, domain.EvidenceIncomplete} {
		for _, result := range []domain.EvidenceResultStatus{domain.EvidencePassed, domain.EvidenceFailed} {
			item := itemWith(lifecycle, result)
			states[string(lifecycle)+"/"+string(result)] = &item
		}
	}
	build := func(id string, matching bool, item *domain.EvidenceItem) (domain.EvidenceBundle, bool) {
		if item == nil {
			return domain.EvidenceBundle{}, false
		}
		bundle := base
		bundle.ID = id
		if !matching {
			bundle.Subject.Revision = "other"
		}
		bundle.Evidence = map[string]domain.EvidenceItem{"item-" + id: *item}
		return bundle, true
	}
	for matchName, matchItem := range states {
		for otherName, otherItem := range states {
			t.Run(matchName+" + non-matching "+otherName, func(t *testing.T) {
				var bundles []domain.EvidenceBundle
				if bundle, ok := build("matching", true, matchItem); ok {
					bundles = append(bundles, bundle)
				}
				if bundle, ok := build("other", false, otherItem); ok {
					bundles = append(bundles, bundle)
				}
				input := inputFor(t, "security-sensitive.engineering-work-contract.json", domain.Action{Type: "git.merge", Target: "main"}, domain.CapabilityAvailable)
				input.EvidenceBundles = map[string]domain.EvidenceBundle{}
				for _, bundle := range bundles {
					input.EvidenceBundles[bundle.ID] = bundle
				}
				decision := evaluate(t, input)
				got := "satisfied"
				switch {
				case contains(decision.Blocking, claimID):
					got = "blocked"
				case contains(decision.Stale, claimID):
					got = "stale"
				case contains(decision.Missing, claimID):
					got = "missing"
				}
				want := oldClaimState(bundles, contract, claimID, claim, changeProducer())
				if got != want && !(want == "stale" && got == "missing") {
					t.Fatalf("claim state %s, previously %s: only stale to missing may change", got, want)
				}
				if (got == "satisfied") != (want == "satisfied") || (got == "blocked") != (want == "blocked") {
					t.Fatalf("authorization-relevant state changed: %s, previously %s", got, want)
				}
			})
		}
	}
}
