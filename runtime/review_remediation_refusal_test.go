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

// R1: a matching repository and producer agent are not sufficient - two
// different pull requests at the same repository and commit (a shared base,
// a cherry-pick, a coincidence) must never let a decision for one authorize
// remediation bound to the other. The run's OWN runtime-recorded publication
// (state.projection.PullRequest.Number, folded from its own journal) is what
// decides which PR this run actually published - never the decision's claim.
func TestAdmitReviewRemediationRefusesAPRTheRunNeverPublished(t *testing.T) {
	f := newAdmissionFixture(t)
	const otherPRNumber = 8
	subject := review.Subject{Repository: testRepo.String(), PRNumber: otherPRNumber, HeadSHA: f.headSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Hand-constructed exactly as #233's own resolveRunForPullRequest never
	// would: it binds a decision's RunID only to a run whose OWN journal
	// observed that exact PR number (review_packet.go), and this run's
	// journal (seedRun) observed only admissionTestPRNumber (7), never 8.
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	f.fake.PullRequests[otherPRNumber] = GitHubPullRequest{
		Number: otherPRNumber, HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", State: GitHubOpen,
	}

	_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, otherPRNumber)
	if err == nil {
		t.Fatal("expected a refusal: the run published PR #7, never PR #8, even though both share the same commit")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedSubjectMismatch {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedSubjectMismatch)
	}
}

// R2: the durable admission table is the sole authorization source the
// deferred bindExecutionInvoke wiring will trust, so it must refuse a
// structurally valid row that references a decision that never authorized
// blocking remediation - regardless of which caller tries to write it.
func TestCreateReviewRemediationAdmissionRefusesANonBlockingDecision(t *testing.T) {
	f := newAdmissionFixture(t)
	approved := f.seedDecision(f.headSHA, "claude", review.VerdictApprove)
	forged := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: approved.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"invented-finding"}, AdmittedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewRemediationAdmission(forged); err == nil {
		t.Fatal("expected the store to refuse an admission referencing a non-blocking (APPROVE) decision")
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: the referenced decision never authorized remediation", pending)
	}
}

// R2 (indexed/document integrity): a row whose own document disagrees with
// the run_id column that indexes it must never be returned as belonging to
// either run - it is evidence of corruption, not a tie to resolve silently.
func TestReviewRemediationAdmissionsForRunRefusesAnIndexDocumentDisagreement(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		t.Fatal(err)
	}
	// Writes the row directly: the indexed run_id column names a DIFFERENT
	// run than the document's own RunID field claims - unreachable through
	// CreateReviewRemediationAdmission, which always writes both from the
	// same value.
	if _, err := f.store.db.Exec(`INSERT INTO review_remediation_admissions (decision_id, run_id, admitted_unix_nano, document) VALUES (?, ?, ?, ?)`,
		admission.DecisionID, "some-other-run", admission.AdmittedAt.UnixNano(), string(document)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.ReviewRemediationAdmissionsForRun("some-other-run"); err == nil {
		t.Fatal("expected a refusal for a row whose indexed run_id disagrees with its own document")
	}
}

// Exact #7/#8 same-SHA direct-store bypass: a REQUEST_CHANGES decision for a
// PR the run never published, submitted directly to the store API rather
// than through AdmitReviewRemediation, must be refused at the WRITE - and,
// even if it somehow landed, must never surface as a pending key at the
// READ - because the run's own runtime-recorded PR is #7, not #8, matching
// commit and producer notwithstanding.
func TestCreateReviewRemediationAdmissionRefusesAPRTheRunNeverPublished(t *testing.T) {
	f := newAdmissionFixture(t)
	const otherPRNumber = 8
	subject := review.Subject{Repository: testRepo.String(), PRNumber: otherPRNumber, HeadSHA: f.headSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	forged := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: otherPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewRemediationAdmission(forged); err == nil {
		t.Fatal("expected the store to refuse an admission for a PR the run never published, even at the same commit and producer")
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: run %s never published PR #%d", pending, f.runID, otherPRNumber)
	}
}

// A genuine BLOCK decision's own findings are the only ones that may ever
// authorize remediation - an admission cannot substitute invented or
// unrelated finding signatures for the decision's actual blocking ones.
func TestCreateReviewRemediationAdmissionRefusesForgedFindingSignatures(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("the real blocking finding"))
	forged := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"an invented finding nobody ever reviewed"}, AdmittedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewRemediationAdmission(forged); err == nil {
		t.Fatal("expected the store to refuse an admission whose finding signatures disagree with the decision's actual blocking findings")
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: the forged finding signatures were never authorized", pending)
	}
}

// Read-side defense in depth, exercised independently of the write guard: a
// row that violates a binding invariant but exists anyway (written directly
// via raw SQL, bypassing CreateReviewRemediationAdmission entirely - the one
// path its own write-time check cannot see) must still never surface as a
// pending key.
func TestPendingReviewRemediationKeysRefusesARowWrittenAroundTheWriteGuard(t *testing.T) {
	f := newAdmissionFixture(t)
	const otherPRNumber = 8
	subject := review.Subject{Repository: testRepo.String(), PRNumber: otherPRNumber, HeadSHA: f.headSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: otherPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		t.Fatal(err)
	}
	// Bypasses CreateReviewRemediationAdmission's write-time check entirely -
	// the only way to model a row the write guard never saw.
	if _, err := f.store.db.Exec(`INSERT INTO review_remediation_admissions (decision_id, run_id, admitted_unix_nano, document) VALUES (?, ?, ?, ?)`,
		admission.DecisionID, admission.RunID, admission.AdmittedAt.UnixNano(), string(document)); err != nil {
		t.Fatal(err)
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: run %s never published PR #%d", pending, f.runID, otherPRNumber)
	}
}
