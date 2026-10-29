package review

import (
	"testing"
	"time"
)

func validDecision(t *testing.T) Decision {
	t.Helper()
	subject := Subject{Repository: "zenchron/fixture", PRNumber: 7, HeadSHA: "abc123"}
	id, err := DecisionID(subject, "claude")
	if err != nil {
		t.Fatalf("DecisionID: %v", err)
	}
	return Decision{
		SchemaVersion:   SchemaVersion,
		ID:              id,
		Subject:         subject,
		RunID:           "run-1",
		ReviewerAgentID: "claude",
		Verdict:         VerdictApprove,
		CreatedAt:       time.Unix(1700000000, 0).UTC(),
	}
}

// Mutation check (#233 B6): Decision.Validate must refuse an ID that does not
// match its canonical DecisionID for the stated subject and reviewer, which is
// the one thing that keeps DecisionID's determinism an enforced invariant
// rather than a convention a caller could silently violate. Removing the
// canonical-ID check in Validate must make this test fail.
func TestDecisionValidateRefusesANonCanonicalID(t *testing.T) {
	d := validDecision(t)
	d.ID = "review-arbitrary-id-chosen-by-a-second-caller"
	if err := d.Validate(); err == nil {
		t.Fatal("expected a non-canonical decision id to be refused")
	}
}

func TestDecisionValidateAcceptsApproveWithNoBlockingFinding(t *testing.T) {
	d := validDecision(t)
	d.Findings = []Finding{{Severity: SeverityNonBlocking, Signature: "naming nit"}}
	if err := d.Validate(); err != nil {
		t.Fatalf("expected a valid decision, got %v", err)
	}
}

// Mutation check: an APPROVE that also names a blocking finding is two
// answers, and Validate must refuse it. Flipping the guard below (deleting
// this case) must make the test fail.
func TestDecisionValidateRefusesApproveWithBlockingFinding(t *testing.T) {
	d := validDecision(t)
	d.Findings = []Finding{{Severity: SeverityBlocking, Signature: "real defect"}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected approve+blocking-finding to be refused")
	}
}

func TestDecisionValidateRefusesRequestChangesWithNoBlockingFinding(t *testing.T) {
	d := validDecision(t)
	d.Verdict = VerdictRequestChanges
	d.Findings = []Finding{{Severity: SeverityNonBlocking, Signature: "cleanup"}}
	if err := d.Validate(); err == nil {
		t.Fatal("expected request_changes with no blocking finding to be refused")
	}
}

func TestDecisionValidateAcceptsRequestChangesWithBlockingFinding(t *testing.T) {
	d := validDecision(t)
	d.Verdict = VerdictRequestChanges
	d.Findings = []Finding{{Severity: SeverityBlocking, Signature: "real defect"}}
	if err := d.Validate(); err != nil {
		t.Fatalf("expected a valid decision, got %v", err)
	}
}

func TestDecisionIDIsDeterministicAndBoundToExactSubjectAndReviewer(t *testing.T) {
	subject := Subject{Repository: "zenchron/fixture", PRNumber: 7, HeadSHA: "head1"}
	id1, err := DecisionID(subject, "claude")
	if err != nil {
		t.Fatalf("DecisionID: %v", err)
	}
	id2, err := DecisionID(subject, "claude")
	if err != nil || id1 != id2 {
		t.Fatalf("expected the same subject+reviewer to produce the same id, got %q and %q (err %v)", id1, id2, err)
	}
	movedHead := subject
	movedHead.HeadSHA = "head2"
	id3, err := DecisionID(movedHead, "claude")
	if err != nil || id3 == id1 {
		t.Fatalf("expected a moved head to produce a different id, got %q", id3)
	}
	otherReviewer, err := DecisionID(subject, "codex")
	if err != nil || otherReviewer == id1 {
		t.Fatalf("expected a different reviewer to produce a different id, got %q", otherReviewer)
	}
}

func TestDecisionStaleAgainstDetectsAMovedHead(t *testing.T) {
	d := validDecision(t)
	if d.StaleAgainst("abc123") {
		t.Fatal("a decision must not be stale against its own exact head")
	}
	if !d.StaleAgainst("def456") {
		t.Fatal("a decision bound to head1 must be stale against a different observed head")
	}
}
