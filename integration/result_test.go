package integration

import (
	"encoding/json"
	"fmt"
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

func mustConflict(t *testing.T, kind ConflictKind, detail string, paths []string) Conflict {
	t.Helper()
	c, err := NewConflict(kind, detail, paths)
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
	conflict := mustConflict(t, ConflictTextual, "merge conflict", []string{"a.go", "b.go"})
	if err := VerifyRemediationScope(conflict, []string{"a.go"}); err != nil {
		t.Fatalf("refused an in-scope remediation: %v", err)
	}
	if err := VerifyRemediationScope(conflict, []string{"a.go", "unrelated.go"}); err == nil {
		t.Fatal("accepted a remediation touching a path outside the conflict")
	}
	semantic := mustConflict(t, ConflictSemantic, "assurance failed", nil)
	if err := VerifyRemediationScope(semantic, []string{"a.go"}); err == nil {
		t.Fatal("checked remediation scope against a non-textual conflict")
	}
}

// TestNewConflictRefusesOverBoundRatherThanTruncating is a regression guard:
// Paths is reused by VerifyRemediationScope as the EXACT in-scope material a
// remediation may touch, so silently dropping entries past a bound would
// silently narrow that scope and reject a legitimate fix, or silently claim
// a conflict was fully described when it was not. Over-bound input must be
// refused, never cut down and returned as if it were complete.
func TestNewConflictRefusesOverBoundRatherThanTruncating(t *testing.T) {
	if _, err := NewConflict(ConflictTextual, strings.Repeat("x", maxConflictDetailBytes+100), nil); err == nil {
		t.Fatal("accepted an over-bound detail")
	}
	manyPaths := make([]string, maxConflictPaths+10)
	for i := range manyPaths {
		manyPaths[i] = fmt.Sprintf("path-%d.go", i)
	}
	if _, err := NewConflict(ConflictTextual, "conflict", manyPaths); err == nil {
		t.Fatal("accepted a conflict with more paths than the bound")
	}
	if _, err := NewConflict(ConflictTextual, "conflict", []string{strings.Repeat("p", maxConflictPathBytes+10)}); err == nil {
		t.Fatal("accepted an over-bound path")
	}
	// Within bound, every path is kept verbatim - none silently dropped.
	ok, err := NewConflict(ConflictTextual, "conflict", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ok.Paths) != 2 || ok.Paths[0] != "a.go" || ok.Paths[1] != "b.go" {
		t.Fatalf("paths within bound were altered: %v", ok.Paths)
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
	blocked := Blocked(contract, mustConflict(t, ConflictTextual, "conflict", []string{"a.go"}))
	if blocked.Status != StatusBlocked || blocked.Conflict == nil || blocked.Candidate != nil || blocked.Reason != "" {
		t.Fatalf("Blocked() result is malformed: %+v", blocked)
	}
	invalidated := Invalidated(contract, "upstream replaced")
	if invalidated.Status != StatusInvalidated || invalidated.Reason == "" || invalidated.Candidate != nil || invalidated.Conflict != nil {
		t.Fatalf("Invalidated() result is malformed: %+v", invalidated)
	}
}
