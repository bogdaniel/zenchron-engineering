package runtime

// #474: the admission table's own binding-invariant defenses - PR ownership,
// forged/substituted findings, reviewer independence, and storage-key
// integrity - split from review_remediation_refusal_test.go's ordinary
// refusal reasons because this is a cohesive concern of its own: every test
// here is about the shared reviewRemediationBindingInvariants function and
// the two store methods (CreateReviewRemediationAdmission,
// ReviewRemediationAdmissionsForRun) that call it, not about
// AdmitReviewRemediation's own simpler per-field checks.

import (
	"context"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/review"
)

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

// Independence, re-checked at the shared invariant function: a self-review
// decision - reviewer == the run's own actual agent - submitted directly to
// the store API must be refused at the write, exactly as #233's own
// CheckReviewIndependence would refuse it through the ordinary review path
// it never went through here.
func TestCreateReviewRemediationAdmissionRefusesASelfReviewDecision(t *testing.T) {
	f := newAdmissionFixture(t)
	subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
	// "codex" is both the run's own agent (seedRun) and, here, the reviewer -
	// a decision review.Decision.Validate itself does not reject, and #233's
	// CheckReviewIndependence is never consulted for a row built this way.
	id, err := review.DecisionID(subject, "codex")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: "codex", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	forged := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewRemediationAdmission(forged); err == nil {
		t.Fatal("expected the store to refuse an admission whose decision names the run's own agent as both producer and reviewer")
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
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: the decision's reviewer is not independent of the run's own agent", pending)
	}
}

// The same self-review decision, injected directly via raw SQL around the
// write guard entirely: the read side must independently refuse it too.
func TestPendingReviewRemediationKeysRefusesASelfReviewRowWrittenAroundTheWriteGuard(t *testing.T) {
	f := newAdmissionFixture(t)
	subject := review.Subject{Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA}
	id, err := review.DecisionID(subject, "codex")
	if err != nil {
		t.Fatal(err)
	}
	decision := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject, RunID: f.runID,
		ProducerAgentID: "codex", ReviewerAgentID: "codex", Verdict: review.VerdictRequestChanges,
		Findings: []review.Finding{blockingFinding("f1")}, CreatedAt: f.clock.Now(),
	}
	if _, _, err := f.store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("pendingReviewRemediationKeys() = %v, want none: the decision's reviewer is not independent of the run's own agent", pending)
	}
}

// Storage integrity: ReviewRemediationAdmission's own row key and its
// document's claimed DecisionID must agree - the same check already applied
// to ReviewRemediationAdmissionsForRun's run_id, now proven for the
// decision_id lookup path.
func TestReviewRemediationAdmissionRefusesAKeyDocumentDisagreement(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	// A second, distinct decision so the FK target exists: the row's indexed
	// decision_id must reference a real review_decisions row, even though
	// the document inside claims a DIFFERENT (also real) decision's identity.
	other := f.seedDecision(f.headSHA, "claude-other", review.VerdictRequestChanges, blockingFinding("f1"))
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decision.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`INSERT INTO review_remediation_admissions (decision_id, run_id, admitted_unix_nano, document) VALUES (?, ?, ?, ?)`,
		other.ID, admission.RunID, admission.AdmittedAt.UnixNano(), string(document)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.store.ReviewRemediationAdmission(other.ID); err == nil {
		t.Fatal("expected a refusal for a row whose indexed decision_id disagrees with its own document")
	}
}

// Storage integrity: #233's ReviewDecision must refuse a row whose own key
// disagrees with its document's claimed ID - this is the one lookup
// reviewRemediationBindingInvariants trusts as authoritative for the
// decision it is given.
func TestReviewDecisionRefusesAKeyDocumentDisagreement(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	document, err := CanonicalJSON(decision)
	if err != nil {
		t.Fatal(err)
	}
	const wrongID = "review-1111111111111111111111"
	if _, err := f.store.db.Exec(`INSERT INTO review_decisions (id, repository, pr_number, head_sha, run_id, created_unix_nano, document) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		wrongID, decision.Subject.Repository, decision.Subject.PRNumber, decision.Subject.HeadSHA, decision.RunID, decision.CreatedAt.UnixNano(), string(document)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.store.ReviewDecision(wrongID); err == nil {
		t.Fatal("expected a refusal for a row whose indexed id disagrees with its own document")
	}
}

// Storage integrity in the ACTUAL planner read path: ReviewRemediationAdmissionsForRun
// (what pendingReviewRemediationKeys calls) must refuse a row whose indexed
// decision_id names a DIFFERENT decision than its own document claims - not
// merely the single-row ReviewRemediationAdmission lookup, which the planner
// never calls. Without this, a row indexed under decision B but whose
// document names decision A would read back as authorization for A.
func TestPendingReviewRemediationKeysRefusesARowIndexedUnderADifferentDecision(t *testing.T) {
	f := newAdmissionFixture(t)
	decisionA := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	decisionB := f.seedDecision(f.headSHA, "claude-other", review.VerdictRequestChanges, blockingFinding("f1"))
	admission := ReviewRemediationAdmission{
		SchemaVersion: reviewRemediationAdmissionSchemaVersion, DecisionID: decisionA.ID, RunID: f.runID,
		Repository: testRepo.String(), PRNumber: admissionTestPRNumber, HeadSHA: f.headSHA,
		FindingSignatures: []string{"f1"}, AdmittedAt: f.clock.Now(),
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		t.Fatal(err)
	}
	// The row's indexed decision_id names decisionB; its own document claims
	// decisionA - unreachable through CreateReviewRemediationAdmission,
	// which always writes both from the same value.
	if _, err := f.store.db.Exec(`INSERT INTO review_remediation_admissions (decision_id, run_id, admitted_unix_nano, document) VALUES (?, ?, ?, ?)`,
		decisionB.ID, admission.RunID, admission.AdmittedAt.UnixNano(), string(document)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.ReviewRemediationAdmissionsForRun(f.runID); err == nil {
		t.Fatal("expected a refusal for a row whose indexed decision_id disagrees with its own document")
	}

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.pendingReviewRemediationKeys(); err == nil {
		t.Fatal("expected pendingReviewRemediationKeys to surface the integrity error, not silently report no pending keys")
	}
}

// A store read failure at the point conditions() checks for pending review
// remediation must surface as a visible, named wait - never let the run
// settle as goal_state_reached over work this pass could not prove was
// absent (#474 production wiring requirement).
func TestConditionsSurfacesAnUnreadableReviewRemediationTable(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	// A direct runState construction - the same shape conditionsFixture
	// uses elsewhere - isolates conditions()'s OWN check from unrelated
	// ones (controller succession, merge precedence) that a full
	// (*EngineeringRuntime).load would also have to satisfy first.
	run, found, err := f.store.Run(f.runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	events, err := f.store.Events(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{rt: f.runtime(), run: run, snapshot: snapshot, events: events, projection: projection}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	disposition, reason := state.conditions()
	if disposition != Waiting || reason != "review_remediation_unavailable" {
		t.Fatalf("conditions() = (%v, %q), want (Waiting, \"review_remediation_unavailable\") for an unreadable admission table", disposition, reason)
	}
}

// bindExecutionInvoke must likewise never promote a store read failure into
// "nothing wanted": it simply finds no binding this pass (conditions()
// above is what makes the failure visible at all), and must never panic or
// silently invent a binding from an unreadable table.
func TestBindExecutionInvokeToleratesAnUnreadableReviewRemediationTable(t *testing.T) {
	f := newAdmissionFixture(t)
	f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))

	state, err := f.runtime().load(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	if binding, wanted := bindExecutionInvoke(state); wanted {
		t.Fatalf("expected no binding from an unreadable admission table, got %q", binding)
	}
}
