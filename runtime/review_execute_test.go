package runtime

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func admitPRReviewFixture() (ReviewInvocationInput, ReviewInvocationOutput) {
	in := ReviewInvocationInput{
		ReviewID: "review-1",
		Packet: ReviewPacket{
			Trusted: ReviewTrustedFacts{
				RunID: "run-1", Repository: "zenchron/fixture", PRNumber: 7,
				HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, ProducerAgentID: "codex",
			},
		},
		Workspace: &PlanningWorkspace{Commit: testHeadSHA, Tree: "tree1"},
		Agent:     ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode},
	}
	return in, ReviewInvocationOutput{}
}

func TestAdmitPRReviewAcceptWithNoFindingsIsApprove(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted}
	decision, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()})
	if err != nil {
		t.Fatalf("AdmitPRReview: %v", err)
	}
	if decision.Verdict != review.VerdictApprove {
		t.Fatalf("expected VerdictApprove, got %q", decision.Verdict)
	}
}

func TestAdmitPRReviewAcceptWithOnlyNonBlockingFindingsIsCommentOnly(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
		Findings: []ReviewerFinding{{Signature: "naming nit", Severity: ReviewSeverityNonBlocking}},
	}
	decision, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()})
	if err != nil {
		t.Fatalf("AdmitPRReview: %v", err)
	}
	if decision.Verdict != review.VerdictCommentOnly {
		t.Fatalf("expected VerdictCommentOnly, got %q", decision.Verdict)
	}
}

// Mutation check: an accepting verdict that ALSO names a blocking finding is
// two answers and must be refused, not silently approved. Removing the
// hasBlocking guard in the StageReviewAccepted case must make this fail.
func TestAdmitPRReviewRefusesAcceptWithABlockingFinding(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted,
		Findings: []ReviewerFinding{{Signature: "real defect", Severity: ReviewSeverityBlocking}},
	}
	if _, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()}); err == nil {
		t.Fatal("expected accept+blocking-finding to be refused")
	}
}

func TestAdmitPRReviewBlockedWithBlockingFindingIsRequestChanges(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "real defect"}}, // absent severity reads as blocking
	}
	decision, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()})
	if err != nil {
		t.Fatalf("AdmitPRReview: %v", err)
	}
	if decision.Verdict != review.VerdictRequestChanges {
		t.Fatalf("expected VerdictRequestChanges, got %q", decision.Verdict)
	}
	if len(decision.Findings) != 1 || decision.Findings[0].Severity != review.SeverityBlocking {
		t.Fatalf("expected an absent severity to default to blocking, got %+v", decision.Findings)
	}
}

// Mutation check: a blocked verdict whose only findings are non-blocking
// names nothing for remediation to act on and must be refused.
func TestAdmitPRReviewRefusesBlockedWithOnlyNonBlockingFindings(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "cleanup", Severity: ReviewSeverityNonBlocking}},
	}
	if _, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()}); err == nil {
		t.Fatal("expected blocked-with-no-blocking-finding to be refused")
	}
}

// Mutation check: a result that restates a DIFFERENT candidate than the
// invocation actually materialized must be refused, never reconciled.
func TestAdmitPRReviewRefusesACandidateMismatch(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted, Candidate: testOtherSHA}
	if _, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()}); err == nil {
		t.Fatal("expected a restated candidate mismatch to be refused")
	}
}

func TestAdmitPRReviewRefusesANilVerdict(t *testing.T) {
	in, out := admitPRReviewFixture()
	if _, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()}); err == nil {
		t.Fatal("expected no result to be refused")
	}
}

func TestAdmitPRReviewRecordsReviewerProvenance(t *testing.T) {
	in, out := admitPRReviewFixture()
	out.Verdict = &ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted}
	decision, err := AdmitPRReview(in, out, fixedClock{time.Unix(1700000000, 0).UTC()})
	if err != nil {
		t.Fatalf("AdmitPRReview: %v", err)
	}
	if decision.ReviewerAgentID != "claude" || decision.ProducerAgentID != "codex" {
		t.Fatalf("expected producer/reviewer provenance to be recorded, got %+v", decision)
	}
	if decision.ReviewerVendorFamily != VendorFamilyFor(AgentKindClaudeCode) {
		t.Fatalf("expected reviewer vendor family to be recorded, got %q", decision.ReviewerVendorFamily)
	}
	if decision.Subject.HeadSHA != testHeadSHA || decision.Subject.PRNumber != 7 {
		t.Fatalf("expected the decision bound to the exact reviewed subject, got %+v", decision.Subject)
	}
}
