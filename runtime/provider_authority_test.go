package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// TestProviderExecutionResultCarriesNoAuthorityBearingField locks
// ExecutionResult (adapters.go) to an explicit allowlist of observation-only
// fields (P2: execution is not authority). It fails the moment a field name
// is added, removed, or renamed, so it fails in particular if someone later
// bolts an authority or acceptance signal (e.g. "Authorized", "Approved",
// "Evidence") straight onto a provider's own result.
func TestProviderExecutionResultCarriesNoAuthorityBearingField(t *testing.T) {
	observationOnly := []string{
		"ProviderID",
		"Model",
		"AuthMode",
		"Attempt",
		"Outcome",
		"Tokens",
		"CostMicros",
		"Artifacts",
		"ChangeSummary",
		"ChangedPaths",
		"Failure",
	}
	typ := reflect.TypeOf(ExecutionResult{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, observationOnly) {
		t.Fatalf("ExecutionResult fields = %v, want exactly %v (a provider result must stay an observation; if this field is legitimately observation-only, update this allowlist deliberately)", got, observationOnly)
	}
}

// TestProviderResultCannotSelfSatisfyIndependentClaim proves P12: a required
// claim marked IndependentFromChangeProducer is not satisfied when the only
// passing evidence for it was produced by the same identity as the change
// producer -- i.e. an execution provider's own result about its own change.
func TestProviderResultCannotSelfSatisfyIndependentClaim(t *testing.T) {
	contract := providerAuthorityContract(t)
	bundle := providerAuthorityEvidence(t)
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
		Action:         action,
		RequiredClaims: []string{"claim-auth-regression-tests"},
	})

	// The evidence item's producer is the same execution provider that made
	// the change: a provider result standing in as its own evidence.
	providerResult := ExecutionResult{ProviderID: "codex-provider-1"}
	changeProducer := domain.EvidenceProducer{ID: providerResult.ProviderID, Type: domain.ProducerExecutionProvider}
	item := bundle.Evidence["evidence-auth-tests-passed"]
	item.Producer = changeProducer
	bundle.Evidence["evidence-auth-tests-passed"] = item

	flow := KernelFlow{}
	state := KernelState{Contract: contract, Evidence: map[string]domain.EvidenceBundle{bundle.ID: bundle}}
	state, err := flow.Decide(state, action, changeProducer)
	if err != nil {
		t.Fatal(err)
	}
	if state.Decision.Status == domain.AuthorityAuthorized {
		t.Fatalf("provider's own result satisfied an independence-required claim: %#v", state.Decision)
	}
	if !contains(state.Decision.Missing, "claim-auth-regression-tests") {
		t.Fatalf("decision = %#v, want claim-auth-regression-tests reported missing", state.Decision)
	}
}

// TestProviderResultFromIndependentProducerCanSatisfyClaim is the control for
// the previous test: the same claim, same action, same contract shape is
// satisfiable once the passing evidence comes from a producer genuinely
// distinct from the change producer. Without this, the rejection above could
// pass for the wrong reason (e.g. broken fixture, unrelated validation
// failure) rather than because of the independence rule specifically.
func TestProviderResultFromIndependentProducerCanSatisfyClaim(t *testing.T) {
	contract := providerAuthorityContract(t)
	bundle := providerAuthorityEvidence(t)
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
		Action:         action,
		RequiredClaims: []string{"claim-auth-regression-tests"},
	})

	// Evidence producer ("ci-go-tests", from the fixture) is independent of
	// the change producer (the execution provider that made the change).
	providerResult := ExecutionResult{ProviderID: "codex-provider-1"}
	changeProducer := domain.EvidenceProducer{ID: providerResult.ProviderID, Type: domain.ProducerExecutionProvider}
	if bundle.Evidence["evidence-auth-tests-passed"].Producer.ID == changeProducer.ID {
		t.Fatal("fixture producer unexpectedly matches the change producer; test would prove nothing")
	}

	flow := KernelFlow{}
	state := KernelState{Contract: contract, Evidence: map[string]domain.EvidenceBundle{bundle.ID: bundle}}
	state, err := flow.Decide(state, action, changeProducer)
	if err != nil {
		t.Fatal(err)
	}
	if state.Decision.Status != domain.AuthorityAuthorized {
		t.Fatalf("independent evidence should authorize, got %#v", state.Decision)
	}
	if !contains(state.Decision.Satisfied, "claim-auth-regression-tests") {
		t.Fatalf("decision = %#v, want claim-auth-regression-tests satisfied", state.Decision)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func providerAuthorityContract(t *testing.T) domain.EngineeringWorkContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", "security-sensitive.engineering-work-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[domain.EngineeringWorkContract](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func providerAuthorityEvidence(t *testing.T) domain.EvidenceBundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", "security-sensitive.evidence-bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.Decode[domain.EvidenceBundle](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
