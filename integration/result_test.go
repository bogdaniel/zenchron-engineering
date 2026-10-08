package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func fixtureContract(t *testing.T) Contract {
	t.Helper()
	c, err := NewContract("graph-1", "integrate", "base-rev",
		orchestration.WorkUnitInputs{input("a", "rev-a"), input("b", "rev-b")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassifyAssuranceFailureRejectsTextual(t *testing.T) {
	_, err := ClassifyAssuranceFailure(fixtureContract(t), IntegratedCandidate{Revision: "r", Tree: "t"}, ConflictTextual, "detail")
	if err == nil {
		t.Fatal("accepted a textual classification from an assurance failure")
	}
}

func TestClassifyAssuranceFailureSemantic(t *testing.T) {
	candidate := IntegratedCandidate{Revision: "merged-rev", Tree: "merged-tree", InputsDigest: "d"}
	result, err := ClassifyAssuranceFailure(fixtureContract(t), candidate, ConflictSemantic, "tests broke after composing both changes")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusBlocked {
		t.Fatalf("status = %s, want %s", result.Status, StatusBlocked)
	}
	if result.Candidate == nil || *result.Candidate != candidate {
		t.Fatalf("semantic conflict dropped the clean candidate it was found against: %+v", result.Candidate)
	}
	if result.Conflict == nil || result.Conflict.Kind != ConflictSemantic {
		t.Fatalf("conflict = %+v, want kind %s", result.Conflict, ConflictSemantic)
	}
}

func TestClassifyAssuranceFailureUncertainDefaultsDetail(t *testing.T) {
	candidate := IntegratedCandidate{Revision: "r", Tree: "t"}
	result, err := ClassifyAssuranceFailure(fixtureContract(t), candidate, ConflictUncertain, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflict == nil || strings.TrimSpace(result.Conflict.Detail) == "" {
		t.Fatalf("uncertain conflict carries no detail: %+v", result.Conflict)
	}
}

func TestVerifyRemediationScope(t *testing.T) {
	conflict := NewConflict(ConflictTextual, "merge conflict", []string{"a.go", "b.go"})
	if err := VerifyRemediationScope(conflict, []string{"a.go"}); err != nil {
		t.Fatalf("refused an in-scope remediation: %v", err)
	}
	if err := VerifyRemediationScope(conflict, []string{"a.go", "unrelated.go"}); err == nil {
		t.Fatal("accepted a remediation touching a path outside the conflict")
	}
	semantic := NewConflict(ConflictSemantic, "assurance failed", nil)
	if err := VerifyRemediationScope(semantic, []string{"a.go"}); err == nil {
		t.Fatal("checked remediation scope against a non-textual conflict")
	}
}

func TestNewConflictBoundsDetailAndPaths(t *testing.T) {
	longDetail := strings.Repeat("x", maxConflictDetailBytes+100)
	manyPaths := make([]string, maxConflictPaths+10)
	for i := range manyPaths {
		manyPaths[i] = strings.Repeat("p", maxConflictPathBytes+10)
	}
	conflict := NewConflict(ConflictTextual, longDetail, manyPaths)
	if len(conflict.Detail) > maxConflictDetailBytes {
		t.Fatalf("detail not bounded: %d bytes", len(conflict.Detail))
	}
	if len(conflict.Paths) > maxConflictPaths {
		t.Fatalf("paths not bounded: %d entries", len(conflict.Paths))
	}
	for _, p := range conflict.Paths {
		if len(p) > maxConflictPathBytes {
			t.Fatalf("path not bounded: %d bytes", len(p))
		}
	}
}

// TestIntegratedCandidateCarriesNoInheritedEvidence is a regression guard for
// requirement G: evidence or review attached to an upstream input must never
// automatically prove the integrated output correct. The type itself must
// never grow a field that could be read as such a verdict.
func TestIntegratedCandidateCarriesNoInheritedEvidence(t *testing.T) {
	document, err := json.Marshal(IntegratedCandidate{Revision: "r", Tree: "t", InputsDigest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"evidence", "review", "approved", "accepted", "authoriz"} {
		if strings.Contains(strings.ToLower(string(document)), forbidden) {
			t.Fatalf("IntegratedCandidate carries a %q-shaped field: %s", forbidden, document)
		}
	}
}

func TestResultConstructorsSetExactlyOneOutcome(t *testing.T) {
	contract := fixtureContract(t)
	candidate := IntegratedCandidate{Revision: "r", Tree: "t", InputsDigest: "d"}
	integrated := Integrated(contract, candidate)
	if integrated.Status != StatusIntegrated || integrated.Candidate == nil || integrated.Conflict != nil || integrated.Reason != "" {
		t.Fatalf("Integrated() result is malformed: %+v", integrated)
	}
	blocked := Blocked(contract, NewConflict(ConflictTextual, "conflict", []string{"a.go"}))
	if blocked.Status != StatusBlocked || blocked.Conflict == nil || blocked.Candidate != nil || blocked.Reason != "" {
		t.Fatalf("Blocked() result is malformed: %+v", blocked)
	}
	invalidated := Invalidated(contract, "upstream replaced")
	if invalidated.Status != StatusInvalidated || invalidated.Reason == "" || invalidated.Candidate != nil || invalidated.Conflict != nil {
		t.Fatalf("Invalidated() result is malformed: %+v", invalidated)
	}
}
