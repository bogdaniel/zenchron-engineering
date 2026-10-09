package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// ---------------------------------------------------------------------------
// Negative / fail-closed paths
// ---------------------------------------------------------------------------

func TestAdmitReviewRemediationRefusesWhenNoDecisionExists(t *testing.T) {
	f := newAdmissionFixture(t)
	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal with no decision on record")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedNoDecision {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedNoDecision)
	}
}

func TestAdmitReviewRemediationRefusesNonBlockingVerdicts(t *testing.T) {
	for _, verdict := range []review.Verdict{review.VerdictApprove, review.VerdictCommentOnly} {
		t.Run(string(verdict), func(t *testing.T) {
			f := newAdmissionFixture(t)
			f.seedDecision(f.headSHA, "claude", verdict)
			_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
			if err == nil {
				t.Fatalf("expected %q to be refused as not blocking", verdict)
			}
			if reason := refusalReason(t, err); reason != ReviewRemediationRefusedNotBlocking {
				t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedNotBlocking)
			}
		})
	}
}

// Local BLOCK with GitHub publication disabled or uncertain must still admit:
// publication state is never consulted (docs/review.md, "GitHub publication
// is not the authorization source").
func TestAdmitReviewRemediationIgnoresPublicationState(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	// No review.Publication row is ever written for decision.ID - models
	// "publication wasn't requested, or is not yet authorized" (ReviewOutcome
	// doc comment) - and admission must still succeed.
	_, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil || !created {
		t.Fatalf("expected admission to succeed with no publication on record, got created=%v err=%v", created, err)
	}
	if _, found, err := f.store.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("test invariant broken: a publication exists (found=%v err=%v)", found, err)
	}
}

func TestAdmitReviewRemediationRefusesAStaleSubject(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	// The PR has moved past the exact head the decision was reached against.
	pr := f.fake.PullRequests[admissionTestPRNumber]
	pr.HeadSHA = strings.Repeat("c", 40)
	f.fake.PullRequests[admissionTestPRNumber] = pr

	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal for a decision bound to a superseded head")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedStaleSubject {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedStaleSubject)
	}
}

// Defense in depth (#474 requirement 2): #233 refuses to construct a Decision
// at all when producer and reviewer collapse, but a corrupted or
// hand-crafted row in the durable store must still never buy remediation
// authorization merely by existing.
func TestAdmitReviewRemediationRefusesAForgedIndependenceCollapse(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "codex", review.VerdictRequestChanges, blockingFinding("f1")) // reviewer == producer ("codex")

	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal when the decision names the same agent as producer and reviewer")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedNotIndependent {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedNotIndependent)
	}
}

func TestAdmitReviewRemediationRefusesATerminalRun(t *testing.T) {
	for _, disposition := range []Disposition{Completed, Failed, Cancelled} {
		t.Run(string(disposition), func(t *testing.T) {
			f := newAdmissionFixture(t)
			run, found, err := f.store.Run(f.runID)
			if err != nil || !found {
				t.Fatal(err)
			}
			run.Disposition = disposition
			if err := f.store.PutRun(run); err != nil {
				t.Fatal(err)
			}
			f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

			_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
			if err == nil {
				t.Fatalf("expected a %s run to be refused", disposition)
			}
			if reason := refusalReason(t, err); reason != ReviewRemediationRefusedRunTerminal {
				t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedRunTerminal)
			}
		})
	}
}

func TestAdmitReviewRemediationRefusesAnUnknownRun(t *testing.T) {
	f := newAdmissionFixture(t)
	subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: "run-does-not-exist",
		ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}

	_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal for a decision naming an unknown run")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedUnknownRun {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedUnknownRun)
	}
}

func TestAdmitReviewRemediationRefusesWithNoCompiledContract(t *testing.T) {
	f := newAdmissionFixture(t)
	// A run with no EventContractCompiled at all: seed it fresh, bypassing
	// seedRun's event set.
	run := newJournalRun("run-no-contract")
	run.Repository = testRepo.String()
	run.AgentID = "codex"
	run.CreatedAt = f.clock.Now().Add(-time.Minute)
	if err := f.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	f.runID = "run-no-contract"
	f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: f.headSHA, Tree: "tree-1", PathsDigest: "d1"})
	f.appendEvent(EventGitHubPRObserved, GitHubPRObservedPayload{Number: admissionTestPRNumber, HeadRevision: f.headSHA, BaseRevision: f.baseSHA, State: "open"})
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal for a run with no compiled contract")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedNoContract {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedNoContract)
	}
}

// The run's own candidate moved past the reviewed head (a later commit this
// run made independent of any new PR observation) - the decision is for a
// subject this run has already superseded, even though the PR's own reported
// head still matches it.
func TestAdmitReviewRemediationRefusesASupersededCandidate(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: strings.Repeat("d", 40), Tree: "tree-2", PathsDigest: "d2"})

	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal when the run's own candidate has moved past the reviewed head")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedCandidateMoved {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedCandidateMoved)
	}
}

// H1's decision can never satisfy H2: a fresh decision for the NEW head is a
// different DecisionID entirely, admitted (or refused) independently.
func TestStaleDecisionNeverSatisfiesANewerHead(t *testing.T) {
	f := newAdmissionFixture(t)
	h1Decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	h2 := strings.Repeat("e", 40)
	f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: h2, Tree: "tree-2", PathsDigest: "d2"})
	pr := f.fake.PullRequests[admissionTestPRNumber]
	pr.HeadSHA = h2
	f.fake.PullRequests[admissionTestPRNumber] = pr

	// H1's own decision is now both stale (PR moved) and superseded
	// (candidate moved); LatestDecision still returns it until a NEW one is
	// reached, and it must never admit.
	if _, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber); err == nil {
		t.Fatal("expected H1's decision to be refused once H2 is the current head")
	}

	h2Decision := f.seedDecision(h2, "claude", review.VerdictRequestChanges, blockingFinding("f2"))
	if h2Decision.ID == h1Decision.ID {
		t.Fatal("test invariant broken: H1 and H2 decisions must have distinct identities")
	}
	admission, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil || !created || admission.DecisionID != h2Decision.ID {
		t.Fatalf("expected H2's own decision to admit cleanly, got %+v created=%v err=%v", admission, created, err)
	}
}

// pendingReviewRemediationKeys folds purely from the run's own already-loaded
// events: an admission bound to an OLDER head than the run's current one
// must never appear as pending for the current head.
func TestPendingReviewRemediationKeysIgnoresAdmissionsForAnOlderHead(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	if _, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber); err != nil || !created {
		t.Fatalf("admitting H1's remediation: created=%v err=%v", created, err)
	}

	h2 := strings.Repeat("e", 40)
	f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: h2, Tree: "tree-2", PathsDigest: "d2"})

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v after the head moved past the admitted decision, want none", pending)
	}
}
