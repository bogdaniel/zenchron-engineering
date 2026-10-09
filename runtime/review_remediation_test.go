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
	// newJournalRun's fixed CreatedAt (1970) is unrelated to the wall-clock
	// AdmitReviewRemediation's freshness check (B4) evaluates against; keep
	// it recent relative to the fixture's own clock so an ordinary admission
	// is never mistaken for a wall-budget-exhausted run.
	run.CreatedAt = f.clock.Now().Add(-time.Minute)
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

	// The fact is visible through the one durable table the deferred
	// reconciler wiring will read from, with no further write required.
	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
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

	admissions, err := f.store.ReviewRemediationAdmissionsForRun(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 1 {
		t.Fatalf("run %s carries %d durable admissions, want exactly 1", f.runID, len(admissions))
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

// B1: there is no window to crash in between "the admission row committed"
// and "the fact is visible" any more, because there is no second write.
// Writing the row directly (never calling AdmitReviewRemediation at all)
// models exactly what a crash immediately after the real call's one commit
// would leave behind, and pendingReviewRemediationKeys must find it with no
// further action - proving the admission row IS the complete, self-contained
// fact, not half of a two-write protocol.
func TestReviewRemediationAdmissionIsCompleteOnItsOwnSingleWrite(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	if _, created, err := f.store.CreateReviewRemediationAdmission(admission); err != nil || !created {
		t.Fatalf("seeding the admission row directly: created=%v err=%v", created, err)
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != decision.ID {
		t.Fatalf("pendingReviewRemediationKeys() = %v immediately after the single write, want [%s] with no further action", pending, decision.ID)
	}
}

// B2: IsStale's own internal LatestDecision call can evaluate a DIFFERENT,
// newer decision than the one fetched at the top of AdmitReviewRemediation,
// if one is admitted in the narrow window between the two reads. The forced
// interleaving below reproduces exactly that: by the time the staleness
// check runs, decision D2 (bound to a newer head) already exists and IS
// current, so IsStale(D2) reports false - but D1 (the one this call is
// trying to authorize) must still be refused as superseded.
func TestAdmitReviewRemediationRefusesADecisionSupersededBetweenReads(t *testing.T) {
	f := newAdmissionFixture(t)
	h1Decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	h2 := strings.Repeat("e", 40)

	defer func() { reviewRemediationRaceTestHook = nil }()
	reviewRemediationRaceTestHook = func() {
		// Models a second, concurrent admission that races ahead between
		// this call's staleness read and its identity re-confirmation: a
		// fresh candidate head and a fresh, current decision for it.
		f.appendEvent(EventCandidateCommitted, CandidateCommittedPayload{Commit: h2, Tree: "tree-2", PathsDigest: "d2"})
		pr := f.fake.PullRequests[admissionTestPRNumber]
		pr.HeadSHA = h2
		f.fake.PullRequests[admissionTestPRNumber] = pr
		f.seedDecision(h2, "claude", review.VerdictRequestChanges, blockingFinding("f2"))
	}

	_, _, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected H1's decision to be refused once a newer decision supersedes it mid-admission")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedStaleSubject {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedStaleSubject)
	}
	if _, found, err := f.store.ReviewRemediationAdmission(h1Decision.ID); err != nil || found {
		t.Fatalf("H1 must never be admitted once superseded: found=%v err=%v", found, err)
	}
}

// B3: the gate must not trust a decision's claimed producer/subject merely
// because it is self-consistent - it must match the ACTUAL run and the
// ACTUAL requested subject.
func TestAdmitReviewRemediationRefusesSubjectAndProducerMismatches(t *testing.T) {
	t.Run("decision document disagrees with the indexed row that found it", func(t *testing.T) {
		f := newAdmissionFixture(t)
		// Writes a row directly, bypassing CreateReviewDecision entirely, so
		// its INDEXED (repository, pr_number) columns - what LatestDecision's
		// query matches on - say testRepo/admissionTestPRNumber, while its
		// embedded document's OWN claimed Subject.Repository disagrees. No
		// legitimate store API can produce this; it models #233's store
		// layer corrupting or disagreeing with itself, which this gate must
		// never trust merely because the query found a row.
		subject := review.Subject{Repository: "someone/else", PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
		id, err := review.DecisionID(subject, "claude")
		if err != nil {
			t.Fatal(err)
		}
		decision := review.Decision{
			SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
			ProducerAgentID: "codex", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
			Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
		}
		document, err := CanonicalJSON(decision)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`INSERT INTO review_decisions (id, repository, pr_number, head_sha, run_id, created_unix_nano, document)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			decision.ID, testRepo.String(), admissionTestPRNumber, decision.Subject.HeadSHA, decision.RunID, decision.CreatedAt.UnixNano(), string(document)); err != nil {
			t.Fatal(err)
		}

		_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
		if err == nil {
			t.Fatal("expected a refusal for a decision whose own subject disagrees with the row that found it")
		}
		if reason := refusalReason(t, err); reason != ReviewRemediationRefusedSubjectMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedSubjectMismatch)
		}
	})

	t.Run("run's actual agent disagrees with the decision's claimed producer", func(t *testing.T) {
		f := newAdmissionFixture(t)
		subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
		id, err := review.DecisionID(subject, "claude")
		if err != nil {
			t.Fatal(err)
		}
		// The run's actual agent is "codex" (seedRun), but this decision
		// claims an unrelated producer identity - independence (producer !=
		// reviewer) holds trivially, yet the claimed producer never produced
		// this run at all.
		decision := review.Decision{
			SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
			ProducerAgentID: "some-other-agent", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
			Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
		}
		if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
			t.Fatal(err)
		}
		_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
		if err == nil {
			t.Fatal("expected a refusal when the decision's claimed producer is not the run's actual agent")
		}
		if reason := refusalReason(t, err); reason != ReviewRemediationRefusedProducerMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedProducerMismatch)
		}
	})

	t.Run("empty claimed producer", func(t *testing.T) {
		f := newAdmissionFixture(t)
		subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
		id, err := review.DecisionID(subject, "claude")
		if err != nil {
			t.Fatal(err)
		}
		decision := review.Decision{
			SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
			ProducerAgentID: "", ReviewerAgentID: "claude", Verdict: review.VerdictRequestChanges,
			Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
		}
		if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
			t.Fatal(err)
		}
		_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
		if err == nil {
			t.Fatal("expected a refusal for an empty claimed producer identity")
		}
		if reason := refusalReason(t, err); reason != ReviewRemediationRefusedProducerMismatch {
			t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedProducerMismatch)
		}
	})
}

// B4: the run's PERSISTED disposition can lag reality - budget exhaustion is
// only journalled the next time conditions() runs during ordinary
// reconciliation. Admission must re-derive eligibility fresh rather than
// trust a snapshot that has not caught up yet.
func TestAdmitReviewRemediationRefusesARunWhoseBudgetIsExhaustedButNotYetJournalled(t *testing.T) {
	f := newAdmissionFixture(t)
	run, found, err := f.store.Run(f.runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	// The persisted disposition is still Active (newJournalRun's default) -
	// no reconciliation tick has run conditions() since the budget below was
	// exhausted.
	run.Budgets = &RunBudgets{WallLimit: time.Minute}
	run.CreatedAt = f.clock.Now().Add(-2 * time.Hour)
	if err := f.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	_, _, err = f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber)
	if err == nil {
		t.Fatal("expected a refusal for a run whose wall budget is exhausted, even though its persisted disposition is still Active")
	}
	if reason := refusalReason(t, err); reason != ReviewRemediationRefusedRunTerminal {
		t.Fatalf("reason = %q, want %q", reason, ReviewRemediationRefusedRunTerminal)
	}
}
