package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/review"
)

// admissionFixture is everything AdmitReviewRemediation needs: a durable
// SQLiteOperationStore, a producing run with a compiled contract and a
// committed candidate (folded from its own journal, exactly as a real
// producer run's would be), and a FakeGitHubAdapter observing its PR. No real
// Git repository is involved: AdmitReviewRemediation never reads one, only
// the run's own journal and the review store.
type admissionFixture struct {
	t        *testing.T
	stateDir string
	store    *SQLiteOperationStore
	fake     *FakeGitHubAdapter
	clock    fixedClock
	runID    string
	headSHA  string
	baseSHA  string
}

const admissionTestPRNumber = 7

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	root := t.TempDir()
	store, err := OpenSQLiteOperationStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	f := &admissionFixture{
		t: t, stateDir: filepath.Join(root, "state"), store: store,
		clock: fixedClock{time.Unix(1700000200, 0).UTC()},
		runID: "run-474-1", headSHA: strings.Repeat("a", 40), baseSHA: strings.Repeat("b", 40),
	}
	f.seedRun()
	f.fake = NewFakeGitHubAdapter()
	f.fake.PullRequests[admissionTestPRNumber] = GitHubPullRequest{
		Number: admissionTestPRNumber, URL: "https://github.com/zenchron/fixture/pull/7",
		HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", State: GitHubOpen,
	}
	return f
}

// seedRun writes a run whose PROJECTION (folded purely from its own journal,
// never from the EngineeringRun row's own Contract/Candidate fields, which
// Project never reads) shows a compiled contract and a complete candidate at
// f.headSHA - the exact state a real producer run reaches once its candidate
// is committed and published.
func (f *admissionFixture) seedRun() {
	f.t.Helper()
	run := newJournalRun(f.runID)
	run.Repository = testRepo.String()
	run.AgentID = "codex"
	if err := f.store.PutRun(run); err != nil {
		f.t.Fatal(err)
	}
	f.appendEvent(EventContractCompiled, ContractCompiledPayload{
		Contract: Ref{ID: "contract-474", Revision: "1"},
		Subject:  domain.Subject{Repository: testRepo.String(), Revision: f.baseSHA},
	})
	f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: f.headSHA, Tree: "tree-1", PathCount: 1, PathsDigest: "d1"})
	f.appendEvent(EventGitHubPRObserved, GitHubPRObservedPayload{Number: admissionTestPRNumber, HeadRevision: f.headSHA, BaseRevision: f.baseSHA, State: "open"})
}

func (f *admissionFixture) appendEvent(eventType string, payload any) {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: newEventID(f.runID), RunID: f.runID,
		Type: eventType, OccurredAt: f.clock.Now(), Payload: raw,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *admissionFixture) runtime() *EngineeringRuntime {
	return &EngineeringRuntime{deps: Dependencies{Store: f.store, Clock: f.clock}}
}

func (f *admissionFixture) port() ReviewPort {
	return &SupervisorReviewPort{Store: f.store, GitHub: f.fake}
}

// seedDecision writes a durable review.Decision directly - the state
// AdmitReviewRemediation always starts from - without performing a real
// reviewer invocation (#233's own RunIndependentReview is tested elsewhere).
func (f *admissionFixture) seedDecision(headSHA, reviewerID string, verdict review.Verdict, findings ...review.Finding) review.Decision {
	f.t.Helper()
	subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: headSHA}
	id, err := review.DecisionID(subject, reviewerID)
	if err != nil {
		f.t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: reviewerID, Verdict: verdict, Findings: findings,
		CreatedAt: f.clock.Now(),
	}
	stored, _, err := f.store.CreateReviewDecision(decision)
	if err != nil {
		f.t.Fatal(err)
	}
	return stored
}

func blockingFinding(signature string) review.Finding {
	return review.Finding{Severity: review.SeverityBlocking, Signature: signature}
}

func refusalReason(t *testing.T, err error) ReviewRemediationRefusalReason {
	t.Helper()
	refused, ok := err.(*ReviewRemediationRefusedError)
	if !ok {
		t.Fatalf("expected a *ReviewRemediationRefusedError, got %T: %v", err, err)
	}
	return refused.Reason
}

// ---------------------------------------------------------------------------
// Positive path
// ---------------------------------------------------------------------------

func TestAdmitReviewRemediationAdmitsABlockingDecision(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("missing cancellation handling"))

	admission, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil {
		t.Fatalf("AdmitReviewRemediation: %v", err)
	}
	if !created {
		t.Fatal("expected a freshly created admission")
	}
	if admission.DecisionID != decision.ID || admission.RunID != f.runID || admission.HeadSHA != f.headSHA {
		t.Fatalf("admission = %+v, want it bound to decision %s / run %s / head %s", admission, decision.ID, f.runID, f.headSHA)
	}
	if len(admission.FindingSignatures) != 1 || admission.FindingSignatures[0] != "missing cancellation handling" {
		t.Fatalf("admission.FindingSignatures = %v, want the one blocking finding", admission.FindingSignatures)
	}

	// The fact is visible on the run's OWN journal, which is what lets the
	// deferred reconciler wiring plan from already-loaded events alone.
	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending := state.pendingReviewRemediationKeys()
	if len(pending) != 1 || pending[0] != decision.ID {
		t.Fatalf("pendingReviewRemediationKeys() = %v, want [%s]", pending, decision.ID)
	}
}

func TestAdmitReviewRemediationIsIdempotent(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	first, created1, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil || !created1 {
		t.Fatalf("first admission: %+v %v", first, err)
	}
	second, created2, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if created2 {
		t.Fatal("expected the second call to find the existing admission, not create a second one")
	}
	if second.DecisionID != first.DecisionID || second.AdmittedAt != first.AdmittedAt {
		t.Fatalf("second admission %+v disagrees with the first %+v", second, first)
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range state.events {
		if e.Type == EventReviewRemediationAdmitted {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("run journal carries %d review.remediation_admitted events, want exactly 1", count)
	}
}

// Two independent store handles racing the same admission must still admit
// it exactly once - the two-SQLite-handle shape AGENTS.md's concurrency
// guidance requires, not a same-process goroutine race that shares a cache.
func TestAdmitReviewRemediationConcurrentCallersAdmitExactlyOnce(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	secondStore, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatalf("opening a second handle on the same database: %v", err)
	}
	t.Cleanup(func() { secondStore.Close() })
	runtimeA := f.runtime()
	runtimeB := &EngineeringRuntime{deps: Dependencies{Store: secondStore, Clock: f.clock}}
	portB := &SupervisorReviewPort{Store: secondStore, GitHub: f.fake}

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, created, err := runtimeA.AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
		results[0], errs[0] = created, err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, created, err := runtimeB.AdmitReviewRemediation(context.Background(), portB, testRepo, admissionTestPRNumber)
		results[1], errs[1] = created, err
	}()
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	createdCount := 0
	for _, created := range results {
		if created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected exactly one caller to create the admission, got %v", results)
	}

	stored, found, err := f.store.ReviewRemediationAdmission(mustDecisionID(t, f.headSHA, "claude"))
	if err != nil || !found {
		t.Fatalf("expected exactly one durable admission row, found=%v err=%v", found, err)
	}
	_ = stored
}

func mustDecisionID(t *testing.T, headSHA, reviewerID string) string {
	t.Helper()
	id, err := review.DecisionID(review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: headSHA}, reviewerID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A restart (fresh process, fresh store handle, same database file) must find
// the same durable admission rather than creating a second one.
func TestAdmitReviewRemediationSurvivesRestart(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	first, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err != nil || !created {
		t.Fatalf("first admission: %+v %v", first, err)
	}

	restarted, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedRuntime := &EngineeringRuntime{deps: Dependencies{Store: restarted, Clock: f.clock}}
	restartedPort := &SupervisorReviewPort{Store: restarted, GitHub: f.fake}

	second, created, err := restartedRuntime.AdmitReviewRemediation(context.Background(), restartedPort, testRepo, admissionTestPRNumber)
	if err != nil {
		t.Fatalf("post-restart admission: %v", err)
	}
	if created {
		t.Fatal("expected the post-restart call to find the existing admission")
	}
	if second.DecisionID != first.DecisionID {
		t.Fatalf("post-restart admission %+v disagrees with the pre-restart one %+v", second, first)
	}
}

// Admission is a fact, never a scheduling or budget side effect: it must not
// create or touch any RunOperation row.
func TestAdmitReviewRemediationTouchesNoOperation(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	before, err := f.store.Operations(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.Operations(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("admission changed the run's operations from %d to %d rows", len(before), len(after))
	}
}

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
	if pending := state.pendingReviewRemediationKeys(); len(pending) != 0 {
		t.Fatalf("pendingReviewRemediationKeys() = %v after the head moved past the admitted decision, want none", pending)
	}
}
