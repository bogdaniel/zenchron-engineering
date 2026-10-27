package runtime

// The operator activates browser acceptance with a project-model boundary and one
// policy rule (docs/browser-acceptance.md). This drives the chain that activation
// creates, end to end and in isolation: an OBSERVED change to the user interface
// compiles to a contract that requires the claim, and authority holds publication
// until a current, complete, passing browser record exists. The record-level cases
// are in browser_acceptance_test.go; this proves they reach the authority decision.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/authority"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

func decodeActivationFixture[T domain.Contract](t *testing.T, name string) T {
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

// activationInputs are the fixture model and policy with the operator's two
// additions applied: the user_interface boundary and USER-INTERFACE-TOUCHED-001.
func activationInputs(t *testing.T) (domain.ProjectModel, domain.EngineeringPolicy) {
	t.Helper()
	model := decodeActivationFixture[domain.ProjectModel](t, "security-sensitive.project-model.json")
	boundaries := map[string]domain.CriticalBoundary{}
	if model.CriticalBoundaries != nil {
		for name, boundary := range *model.CriticalBoundaries {
			boundaries[name] = boundary
		}
	}
	boundaries["user_interface"] = domain.CriticalBoundary{Type: "user_interface", Paths: []string{"frontend/**", "controlplane/**"}}
	model.CriticalBoundaries = &boundaries

	activated := decodeActivationFixture[domain.EngineeringPolicy](t, "security-sensitive.engineering-policy.json")
	rules := map[string]domain.PolicyRule{}
	for id, rule := range activated.Rules {
		rules[id] = rule
	}
	claims := map[string]domain.RequiredClaim{
		BrowserAcceptanceClaimID: {EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true},
	}
	obligations := map[string]domain.PolicyRequirement{
		"browser-acceptance": {
			Statement:      "The dashboard must pass its real-browser acceptance tests on the exact candidate.",
			RequiredClaims: &[]string{BrowserAcceptanceClaimID},
			Material:       true,
		},
	}
	rules["USER-INTERFACE-TOUCHED-001"] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "user_interface.boundary_modified", Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{RequiredClaims: &claims, Obligations: &obligations},
	}
	activated.Rules = rules
	activated.Revision += "-browser"
	return model, activated
}

// observedContract compiles the contract a run would have once its candidate is
// known to touch exactly these paths.
func observedContract(t *testing.T, paths []string) domain.EngineeringWorkContract {
	t.Helper()
	model, activated := activationInputs(t)
	subject := domain.Subject{Repository: model.Subject.Repository, Revision: dischargeCommit}
	facts, err := analysis.NewAnalyzer().Observe(model, subject, analysis.ObservedChange{Paths: paths, PathsKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := policy.Compile(policy.CompileInput{
		ContractID: "contract-run-d", ContractRevision: "1-" + dischargeCommit, Objective: "Change the dashboard.",
		AcceptanceIntent: []string{"Works."}, Subject: subject,
		Scope:        domain.ContractScope{Stage: domain.StageObserved, AllowedPaths: paths},
		ProjectModel: model, Policy: activated, Facts: facts.Sorted(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

// publicationDecision is what authority says about publishing the candidate,
// given the journal the run recorded.
func publicationDecision(t *testing.T, contract domain.EngineeringWorkContract, events []EngineeringEvent) domain.AuthorityDecision {
	t.Helper()
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{projection: projection, events: events, run: EngineeringRun{ID: "run-d"}, sources: []sourceRecord{{BaseRevision: dischargeBase}}}
	runtime := &EngineeringRuntime{deps: Dependencies{Assurance: BaselineGoVerifier{}}}
	bundles, err := runtime.evidenceBundles(state, contract)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authority.Evaluate(authority.Input{
		DecisionID: "decision-d", DecisionRevision: "1", Contract: contract,
		Action:          domain.Action{Type: "git.pull_request.create", Target: "main"},
		Capability:      domain.CapabilityAvailable,
		ChangeProducer:  domain.EvidenceProducer{ID: "change-producer", Type: domain.ProducerExecutionProvider},
		EvidenceBundles: bundles,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// headMovedAfter appends a candidate commit after the journal's assurance, so
// that observation is no longer about the current head.
func headMovedAfter(t *testing.T, events []EngineeringEvent) []EngineeringEvent {
	t.Helper()
	raw, err := json.Marshal(CandidateCommittedPayload{Commit: "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5", Tree: "9999999999999999999999999999999999999999"})
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	return append(slices.Clone(events), EngineeringEvent{
		SchemaVersion: last.SchemaVersion, ID: "candidate-moved", RunID: last.RunID, Sequence: last.Sequence + 1,
		Type: EventCandidateCommitted, OccurredAt: last.OccurredAt.Add(time.Minute), Payload: raw,
	})
}

func TestAnObservedInterfaceChangeIsHeldUntilCurrentBrowserEvidenceExists(t *testing.T) {
	for _, paths := range [][]string{{"frontend/src/app.tsx"}, {"controlplane/assets/design/react.js"}, {"controlplane/objective_report.go"}} {
		contract := observedContract(t, paths)
		if _, required := contract.RequiredClaims[BrowserAcceptanceClaimID]; !required {
			t.Fatalf("%v: an observed interface change did not require browser acceptance: %+v", paths, contract.RequiredClaims)
		}
		if obligation := contract.Obligations["browser-acceptance"]; !obligation.Material {
			t.Fatalf("%v: the browser obligation is not material: %+v", paths, contract.Obligations)
		}
	}

	contract := observedContract(t, []string{"frontend/src/app.tsx"})
	held := map[string][]EngineeringEvent{
		"no browser record (a generic green suite)": journalFor(t, nil),
		"a failed browser run": journalFor(t, []VerificationCheck{func() VerificationCheck {
			check := goodBrowserCheck()
			check.Outcome = CheckOutcomeFailed
			return check
		}()}),
		"a record for another commit": journalFor(t, []VerificationCheck{func() VerificationCheck {
			check := goodBrowserCheck()
			check.Commit = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2"
			return check
		}()}),
		"a record for another base": journalFor(t, []VerificationCheck{func() VerificationCheck {
			check := goodBrowserCheck()
			check.BaseRevision = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
			return check
		}()}),
	}
	held["a new head after the browser run"] = headMovedAfter(t, journalFor(t, []VerificationCheck{goodBrowserCheck()}))
	for name, events := range held {
		decision := publicationDecision(t, contract, events)
		if decision.Status == domain.AuthorityAuthorized || !slices.Contains(decision.Missing, BrowserAcceptanceClaimID) {
			t.Errorf("%s did not hold publication on the browser claim: %+v", name, decision)
		}
	}

	decision := publicationDecision(t, contract, journalFor(t, []VerificationCheck{goodBrowserCheck()}))
	if slices.Contains(decision.Missing, BrowserAcceptanceClaimID) || !slices.Contains(decision.Satisfied, BrowserAcceptanceClaimID) {
		t.Fatalf("a current, complete, passing browser record did not satisfy the claim: %+v", decision)
	}
}

// Work that never touches the interface is governed exactly as before the
// activation: no claim, so nothing waits on a browser.
func TestAGoOnlyChangeIsNotHeldForBrowserEvidence(t *testing.T) {
	contract := observedContract(t, []string{"runtime/browser_acceptance.go"})
	if _, required := contract.RequiredClaims[BrowserAcceptanceClaimID]; required {
		t.Fatalf("a change outside the interface required browser acceptance: %+v", contract.RequiredClaims)
	}
	if decision := publicationDecision(t, contract, journalFor(t, nil)); slices.Contains(decision.Missing, BrowserAcceptanceClaimID) {
		t.Fatalf("publication waits on a claim the contract does not require: %+v", decision)
	}
}
