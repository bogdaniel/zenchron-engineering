package runtime

import (
	"os"
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// #623: a contract that cannot be rebuilt is not a contract that requires no
// semantic claim. The workspace is taken away to make the rebuild fail the way
// a transient git or filesystem fault does, then given back.
func TestAContractRebuildFailureNeverWaivesRequiredSemanticAssurance(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)

	healthy, err := fixture.state(runID).semanticClaims()
	if err != nil || len(healthy) == 0 {
		t.Fatalf("this scenario needs a contract that requires semantic claims: %v claims=%d", err, len(healthy))
	}

	dir := candidateDir(fixture.runtime.deps.StateDir, runID)
	if err := os.Rename(dir, dir+".away"); err != nil {
		t.Fatal(err)
	}
	broken := fixture.state(runID)

	if claims, err := broken.semanticClaims(); err == nil || len(claims) != 0 {
		t.Fatalf("a failed rebuild was reported as an answer: claims=%d err=%v", len(claims), err)
	}
	if key, wanted := bindAssuranceSemantic(broken); !wanted || key == "" {
		t.Fatal("a failed contract rebuild waived the semantic assurance operation")
	}
	if _, err := fixture.runtime.failureBinding(broken, RunOperation{}, "contract.semantic_claim_missing"); err == nil {
		t.Fatal("a failed rebuild was digested as an empty claim set, binding a permanent refusal to it")
	}
	// An integration unit's handoff must keep waiting for semantic evidence it
	// cannot prove is unnecessary.
	broken.origin = &orchestration.BatchOrigin{ExecutionKind: orchestration.ExecutionKindIntegrationCompose}
	semantic := broken.projection.SemanticAssurance
	broken.projection.SemanticAssurance = nil
	if key, wanted := bindIntegrationHandoffAdmit(broken); wanted || key != "" {
		t.Fatal("an integration handoff was admitted past a contract that could not be rebuilt")
	}
	broken.projection.SemanticAssurance = semantic

	if err := os.Rename(dir+".away", dir); err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.state(runID).semanticClaims()
	if err != nil || !reflect.DeepEqual(recovered, healthy) {
		t.Fatalf("the claims did not recover with the workspace: %v", err)
	}
}
