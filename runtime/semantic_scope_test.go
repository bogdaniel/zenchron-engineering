package runtime

import (
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// narrowedVerdict answers every claim it is asked about, but drops one
// obligation from each claim that has more than one. The TEST chooses to
// narrow; the runtime gate decides what that is worth.
func narrowedVerdict(request AssuranceRequest) string {
	type claim struct {
		ClaimID       string   `json:"claim_id"`
		ObligationIDs []string `json:"obligation_ids"`
		Status        string   `json:"status"`
		Rationale     string   `json:"rationale"`
	}
	var claims []claim
	for _, asked := range request.SemanticClaims {
		ids := asked.ObligationIDs
		if len(ids) > 1 {
			ids = ids[:len(ids)-1]
		}
		claims = append(claims, claim{asked.ClaimID, ids, "pass", "the part I looked at is done"})
	}
	body, _ := json.Marshal(map[string]any{"claims": claims})
	return string(body)
}

// TestNarrowedSemanticVerdictNeitherDischargesNorAuthorizes is the end-to-end
// half of defect U, through the reconciler rather than the decoder.
//
// The governing policy gates two material acceptance obligations behind ONE
// semantic claim. Authority is satisfied per claim, so before the repair a
// model that answered that claim while naming only one obligation discharged
// both - including the one it never judged. The run then published under an
// authorized decision that rested on an obligation nobody had assessed.
//
// The requirement is not that the partial answer be recorded as partial
// evidence. It is that it produce NO applicable semantic evidence at all: an
// answer to a question the runtime did not ask is not a smaller answer, it is
// not an answer.
func TestNarrowedSemanticVerdictNeitherDischargesNorAuthorizes(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.SemanticAssurance = &FakeSemanticAssuranceProvider{Answer: narrowedVerdict}
	fixture.runtime = fixture.newRuntime(fixture.deps)

	runID := fixture.start()
	fixture.reconcile(runID)
	state := fixture.state(runID)

	// The scenario only means anything if the contract really does share one
	// semantic claim across several material obligations.
	contract, err := fixture.runtime.contractFor(state)
	if err != nil {
		t.Fatal(err)
	}
	var semanticClaims []string
	for id, claim := range contract.RequiredClaims {
		if claim.EvidenceClass == SemanticEvidenceClass {
			semanticClaims = append(semanticClaims, id)
		}
	}
	if len(semanticClaims) != 1 {
		t.Fatalf("this scenario needs exactly one semantic claim, contract has %d", len(semanticClaims))
	}
	shared := 0
	for _, obligation := range contract.Obligations {
		if !obligation.Material {
			continue
		}
		for _, discharge := range obligation.RequiredClaims {
			if discharge == semanticClaims[0] {
				shared++
			}
		}
	}
	if shared < 2 {
		t.Fatalf("this scenario needs at least two material obligations behind %q, found %d", semanticClaims[0], shared)
	}

	// No semantic verdict was reached, so no claim result was journalled.
	projection := state.projection
	if observed := projection.SemanticAssurance; observed != nil {
		if observed.Passed {
			t.Fatalf("a narrowed verdict was recorded as passing: %#v", observed)
		}
		if len(observed.ClaimResults) != 0 {
			t.Fatalf("a narrowed verdict produced claim results: %#v", observed.ClaimResults)
		}
		if observed.Bundle != (Ref{}) {
			t.Fatalf("a narrowed verdict produced evidence bundle %#v", observed.Bundle)
		}
	}

	// The rebuilt kernel therefore holds no semantic evidence item, so the
	// shared claim is not satisfied and neither obligation is discharged.
	kernel, err := fixture.runtime.buildKernel(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range kernel.Evidence {
		for id, item := range bundle.Evidence {
			if item.EvidenceClass == SemanticEvidenceClass {
				t.Fatalf("a narrowed verdict became semantic evidence %q: %#v", id, item)
			}
		}
	}

	// And the protected action is not authorized.
	action, ok := fixture.runtime.publicationAction()
	if !ok {
		t.Fatal("the fixture has no publication action")
	}
	decided, err := fixture.runtime.decide(state)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Decision.Status == domain.AuthorityAuthorized {
		t.Fatalf("a narrowed semantic verdict authorized %s:%s", action.Type, action.Target)
	}

	// The run must not have published anything on the strength of it.
	if projection.PullRequest != nil {
		t.Fatalf("a run published under a narrowed verdict: PR #%d", projection.PullRequest.Number)
	}
}

// TestCompleteSemanticVerdictStillDischargesEveryObligation is the other half:
// the repair must refuse narrowing without refusing the correct answer. The
// same fixture, answering exactly what it was asked, still reaches authority.
func TestCompleteSemanticVerdictStillDischargesEveryObligation(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.SemanticAssurance = &FakeSemanticAssuranceProvider{
		Answer: func(request AssuranceRequest) string {
			type claim struct {
				ClaimID       string   `json:"claim_id"`
				ObligationIDs []string `json:"obligation_ids"`
				Status        string   `json:"status"`
				Rationale     string   `json:"rationale"`
			}
			var claims []claim
			for _, asked := range request.SemanticClaims {
				claims = append(claims, claim{asked.ClaimID, asked.ObligationIDs, "pass", "all of them are done"})
			}
			body, _ := json.Marshal(map[string]any{"claims": claims})
			return string(body)
		},
	}
	fixture.runtime = fixture.newRuntime(fixture.deps)

	runID := fixture.start()
	fixture.reconcile(runID)
	state := fixture.state(runID)

	observed := state.projection.SemanticAssurance
	if observed == nil || !observed.Passed || len(observed.ClaimResults) == 0 {
		t.Fatalf("a complete verdict produced no semantic observation: %#v", observed)
	}
	kernel, err := fixture.runtime.buildKernel(state)
	if err != nil {
		t.Fatal(err)
	}
	semantic := 0
	for _, bundle := range kernel.Evidence {
		for _, item := range bundle.Evidence {
			if item.EvidenceClass == SemanticEvidenceClass && item.Result.Status == domain.EvidencePassed {
				semantic++
			}
		}
	}
	if semantic == 0 {
		t.Fatalf("a complete verdict produced no semantic evidence: %#v", kernel.Evidence)
	}
	action, _ := fixture.runtime.publicationAction()
	decided, err := fixture.runtime.decide(state)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Decision.Status != domain.AuthorityAuthorized {
		t.Fatalf("a complete verdict did not authorize %s:%s: %s", action.Type, action.Target, decided.Decision.Status)
	}
}
