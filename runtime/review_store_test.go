package runtime

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

func testReviewDecision(t *testing.T, headSHA string) review.Decision {
	t.Helper()
	subject := review.Subject{Repository: "zenchron/fixture", PRNumber: 7, HeadSHA: headSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatalf("DecisionID: %v", err)
	}
	return review.Decision{
		SchemaVersion:   review.SchemaVersion,
		ID:              id,
		Subject:         subject,
		RunID:           "run-1",
		ProducerAgentID: "codex",
		ReviewerAgentID: "claude",
		Verdict:         review.VerdictApprove,
		CreatedAt:       time.Unix(1700000000, 0).UTC(),
	}
}

func TestCreateReviewDecisionIsIdempotentForTheSameIdentity(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	decision := testReviewDecision(t, "head1")
	first, created, err := store.CreateReviewDecision(decision)
	if err != nil || !created {
		t.Fatalf("first create: %+v %v created=%v", first, err, created)
	}
	// A different Reason on the second write must NOT overwrite the first: the
	// decision for this exact (subject, reviewer) identity was already made,
	// and a retry after a lost reply must find it, not supersede it.
	retried := decision
	retried.Reason = "a different write that must not land"
	second, createdAgain, err := store.CreateReviewDecision(retried)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if createdAgain {
		t.Fatal("expected the second create to find the existing decision, not insert a new one")
	}
	if second.Reason != "" {
		t.Fatalf("expected the FIRST write to stand, got reason %q", second.Reason)
	}

	moved := testReviewDecision(t, "head2")
	third, createdThird, err := store.CreateReviewDecision(moved)
	if err != nil || !createdThird {
		t.Fatalf("a moved head must produce a new decision: %+v %v created=%v", third, err, createdThird)
	}

	latest, found, err := store.LatestReviewDecision("zenchron/fixture", 7)
	if err != nil || !found || latest.Subject.HeadSHA != "head2" {
		t.Fatalf("LatestReviewDecision: %+v found=%v err=%v, want head2", latest, found, err)
	}

	history, err := store.ReviewDecisionsForPullRequest("zenchron/fixture", 7)
	if err != nil || len(history) != 2 {
		t.Fatalf("ReviewDecisionsForPullRequest: %+v %v", history, err)
	}
	if history[0].Subject.HeadSHA != "head1" || history[1].Subject.HeadSHA != "head2" {
		t.Fatalf("expected history oldest-first, got %+v", history)
	}
}

func TestRecordReviewPublicationIsInsertOnly(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	decision := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatalf("CreateReviewDecision: %v", err)
	}

	if _, found, err := store.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("expected no publication yet, found=%v err=%v", found, err)
	}

	first := review.Publication{DecisionID: decision.ID, Published: true, GitHubReviewID: 101, PublishedVerdict: review.VerdictApprove}
	inserted, err := store.RecordReviewPublication(first)
	if err != nil || !inserted {
		t.Fatalf("first RecordReviewPublication: inserted=%v err=%v", inserted, err)
	}

	// A second, DIFFERENT publication attempt for the same decision (as a crash
	// retry would produce) must not overwrite the first recorded outcome -
	// otherwise a successful publication's GitHubReviewID could be clobbered by
	// a confused retry that thinks it is publishing for the first time.
	second := review.Publication{DecisionID: decision.ID, Published: true, GitHubReviewID: 999}
	insertedAgain, err := store.RecordReviewPublication(second)
	if err != nil {
		t.Fatalf("second RecordReviewPublication: %v", err)
	}
	if insertedAgain {
		t.Fatal("expected the second publication write to be a no-op")
	}

	stored, found, err := store.ReviewPublication(decision.ID)
	if err != nil || !found || stored.GitHubReviewID != 101 {
		t.Fatalf("expected the FIRST publication to stand, got %+v found=%v err=%v", stored, found, err)
	}
}

func TestClaimReviewIsExclusiveUntilReleasedOrStale(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	now := time.Unix(1700000000, 0).UTC()
	claimed, tokenA, err := store.ClaimReview("key-1", "owner-a", now, time.Minute)
	if err != nil || !claimed || tokenA == "" {
		t.Fatalf("first claim: claimed=%v token=%q err=%v", claimed, tokenA, err)
	}
	// A second owner, same instant: must NOT also claim it.
	claimed, _, err = store.ClaimReview("key-1", "owner-b", now, time.Minute)
	if err != nil || claimed {
		t.Fatalf("expected the second concurrent claim to be refused, got claimed=%v err=%v", claimed, err)
	}
	// Released, so a new claimant may proceed immediately.
	if err := store.ReleaseReviewClaim("key-1", tokenA); err != nil {
		t.Fatalf("ReleaseReviewClaim: %v", err)
	}
	claimed, tokenB, err := store.ClaimReview("key-1", "owner-b", now, time.Minute)
	if err != nil || !claimed || tokenB == "" || tokenB == tokenA {
		t.Fatalf("expected a released claim to be immediately claimable with a fresh token, got claimed=%v token=%q err=%v", claimed, tokenB, err)
	}
}

// Mutation check: an abandoned claim (the owning process crashed) must
// eventually be reclaimable, never stuck forever. Removing the staleness
// WHERE clause in ClaimReview's upsert must make this test fail.
func TestClaimReviewReclaimsAStaleClaim(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	start := time.Unix(1700000000, 0).UTC()
	if claimed, _, err := store.ClaimReview("key-1", "owner-a", start, time.Minute); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	// Still within the staleness window: not reclaimable.
	if claimed, _, err := store.ClaimReview("key-1", "owner-b", start.Add(30*time.Second), time.Minute); err != nil || claimed {
		t.Fatalf("expected the claim to still be active, got claimed=%v err=%v", claimed, err)
	}
	// Past the staleness window: the abandoned claim is reclaimed.
	claimed, _, err := store.ClaimReview("key-1", "owner-b", start.Add(2*time.Minute), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("expected the stale claim to be reclaimed, got claimed=%v err=%v", claimed, err)
	}
}

// Mutation check (#233 B4-2): a claim stolen for staleness must not be
// deletable or extendable by the superseded owner's old token. Without the
// fencing token, a stale owner A finishing after B's steal would delete B's
// LIVE claim by key alone, letting a third caller C start while B is still
// active - exactly the exclusivity violation B4-2 reported. Removing the
// token match from ReleaseReviewClaim/RenewReviewClaim's WHERE clause must
// make this test fail.
func TestClaimReviewFencingTokenPreventsASupersededOwnerFromActing(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	start := time.Unix(1700000000, 0).UTC()
	_, tokenA, err := store.ClaimReview("key-1", "owner-a", start, time.Minute)
	if err != nil {
		t.Fatalf("A's claim: %v", err)
	}
	// B steals it after A goes stale.
	claimedB, tokenB, err := store.ClaimReview("key-1", "owner-b", start.Add(2*time.Minute), time.Minute)
	if err != nil || !claimedB || tokenB == tokenA {
		t.Fatalf("B's steal: claimed=%v tokenB=%q tokenA=%q err=%v", claimedB, tokenB, tokenA, err)
	}

	// A's old token can neither renew nor release B's now-live claim.
	if renewed, err := store.RenewReviewClaim("key-1", tokenA, start.Add(2*time.Minute+time.Second)); err != nil || renewed {
		t.Fatalf("expected A's superseded token to fail to renew, got renewed=%v err=%v", renewed, err)
	}
	if err := store.ReleaseReviewClaim("key-1", tokenA); err != nil {
		t.Fatalf("ReleaseReviewClaim(stale token): %v", err)
	}
	// B's claim must still be in force - a THIRD caller must still be refused.
	claimedC, _, err := store.ClaimReview("key-1", "owner-c", start.Add(2*time.Minute+2*time.Second), time.Minute)
	if err != nil || claimedC {
		t.Fatalf("expected B's claim to survive A's stale release, but C claimed=%v err=%v", claimedC, err)
	}

	// B's own token DOES renew and release it.
	if renewed, err := store.RenewReviewClaim("key-1", tokenB, start.Add(2*time.Minute+3*time.Second)); err != nil || !renewed {
		t.Fatalf("expected B's own token to renew, got renewed=%v err=%v", renewed, err)
	}
	if err := store.ReleaseReviewClaim("key-1", tokenB); err != nil {
		t.Fatalf("ReleaseReviewClaim(B's token): %v", err)
	}
	claimedD, _, err := store.ClaimReview("key-1", "owner-d", start.Add(2*time.Minute+4*time.Second), time.Minute)
	if err != nil || !claimedD {
		t.Fatalf("expected the claim to be free after B's own release, got claimed=%v err=%v", claimedD, err)
	}
}

// Mutation check: a publication row whose document disagrees with its own
// primary key must be refused, never silently handed back as if it named the
// requested decision. Removing the row/document agreement check in
// ReviewPublication must make this test fail.
func TestReviewPublicationRefusesARowThatDisagreesWithItsDocument(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()

	requested := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(requested); err != nil {
		t.Fatal(err)
	}
	mismatched := review.Publication{DecisionID: "review-other", Published: true, GitHubReviewID: 1}
	document, err := CanonicalJSON(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO review_publications (decision_id, document) VALUES (?, ?)`, requested.ID, string(document)); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.ReviewPublication(requested.ID); err == nil || found {
		t.Fatalf("expected a row/document mismatch to be refused, got found=%v err=%v", found, err)
	}
}

// TestClaimReviewIsExclusiveAcrossIndependentStoreHandles proves the claim is
// exclusive across two SEPARATE SQLiteOperationStore handles on the same
// database file (openPair), not merely within one shared Go struct - the
// closer simulation of two independent controller processes racing to claim
// the same review (#233 B4 re-review: "run load-bearing cross-store-handle
// ... tests").
func TestClaimReviewIsExclusiveAcrossIndependentStoreHandles(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()

	claimed, token, err := first.ClaimReview("key-1", "process-a", now, time.Minute)
	if err != nil || !claimed || token == "" {
		t.Fatalf("process A's claim (handle 1): claimed=%v err=%v", claimed, err)
	}
	// A genuinely independent connection must see the claim as held.
	claimed, _, err = second.ClaimReview("key-1", "process-b", now, time.Minute)
	if err != nil || claimed {
		t.Fatalf("expected process B (handle 2) to be refused, got claimed=%v err=%v", claimed, err)
	}
	// Released from handle 1; handle 2 can now claim it.
	if err := first.ReleaseReviewClaim("key-1", token); err != nil {
		t.Fatalf("ReleaseReviewClaim: %v", err)
	}
	claimed, _, err = second.ClaimReview("key-1", "process-b", now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("expected process B to claim it once released, got claimed=%v err=%v", claimed, err)
	}
}

// Mutation check (#233 P1-A): AdmitReviewPublicationAttempt must refuse when
// the token named does not match the claim's CURRENT holder - not merely
// when the claim is altogether absent. Removing the token-fence condition
// from its SQL (the `EXISTS (... AND token = ?)` clause) must make this test
// fail by admitting the wrong caller.
func TestAdmitReviewPublicationAttemptRequiresTheCurrentClaimToken(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()
	decision := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	claimKey := "publish:" + decision.ID
	if _, _, err := store.ClaimReview(claimKey, "owner-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	admitted, err := store.AdmitReviewPublicationAttempt(decision.ID, claimKey, "the-wrong-token", now)
	if err != nil {
		t.Fatalf("AdmitReviewPublicationAttempt: %v", err)
	}
	if admitted {
		t.Fatal("expected admission to be refused for a token that does not name the current claim holder")
	}
	if _, outstanding, err := store.ReviewPublicationAttempt(decision.ID); err != nil || outstanding {
		t.Fatalf("expected no attempt row to be written, outstanding=%v err=%v", outstanding, err)
	}
}

// Mutation check (#233 P1-A): once ANY attempt is outstanding for a
// decision, a second admission attempt - even one correctly holding the
// CURRENT claim token - must still be refused, never silently overwrite the
// existing marker. This is the exact defect a plain upsert had: a caller
// resuming after a pause could blindly take over a different, already-
// admitted caller's marker. Removing the "NOT EXISTS attempt" condition
// (reverting to an upsert) must make this test fail by re-admitting.
func TestAdmitReviewPublicationAttemptNeverOverwritesAnExistingAttempt(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()
	decision := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	claimKey := "publish:" + decision.ID
	_, token, err := store.ClaimReview(claimKey, "owner-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.AdmitReviewPublicationAttempt(decision.ID, claimKey, token, now)
	if err != nil || !first {
		t.Fatalf("expected the first admission to succeed, admitted=%v err=%v", first, err)
	}
	second, err := store.AdmitReviewPublicationAttempt(decision.ID, claimKey, token, now.Add(time.Second))
	if err != nil {
		t.Fatalf("AdmitReviewPublicationAttempt (second): %v", err)
	}
	if second {
		t.Fatal("expected a second admission attempt to be refused while the first attempt is still outstanding")
	}
}

// Mutation check (#233 P1-A): admission must also refuse once a publication
// is already durably recorded, even if no attempt row happens to be
// outstanding (the FinalizeReviewPublication case: the attempt was already
// cleared). Removing the "NOT EXISTS publication" condition must make this
// test fail by admitting a fresh submission for an already-published decision.
func TestAdmitReviewPublicationAttemptRefusesWhenAlreadyPublished(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()
	decision := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordReviewPublication(review.Publication{DecisionID: decision.ID, Published: true, GitHubReviewID: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	claimKey := "publish:" + decision.ID
	_, token, err := store.ClaimReview(claimKey, "owner-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	admitted, err := store.AdmitReviewPublicationAttempt(decision.ID, claimKey, token, now)
	if err != nil {
		t.Fatalf("AdmitReviewPublicationAttempt: %v", err)
	}
	if admitted {
		t.Fatal("expected admission to be refused once the decision is already durably published")
	}
}

// Mutation check (#233 P1-B): a forced failure injected between
// FinalizeReviewPublication's publication INSERT and its attempt DELETE -
// standing in for a crash at exactly that window - must roll back BOTH
// writes together: the publication must not be durable, and the attempt
// marker must still be outstanding. Removing the transaction (committing the
// insert before attempting the delete) must make this test fail by leaving
// the publication durably recorded despite the injected failure.
func TestFinalizeReviewPublicationRollsBackBothWritesOnAnInjectedFailure(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	defer store.Close()
	decision := testReviewDecision(t, "head1")
	if _, _, err := store.CreateReviewDecision(decision); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	if err := store.RecordReviewPublicationAttempt(decision.ID, now); err != nil {
		t.Fatal(err)
	}

	injected := fmt.Errorf("simulated crash between the publication write and the attempt clear")
	reviewPublicationFinalizeTestHook = func() error { return injected }
	defer func() { reviewPublicationFinalizeTestHook = nil }()

	_, err = store.FinalizeReviewPublication(review.Publication{DecisionID: decision.ID, Published: true, GitHubReviewID: 1})
	if !errors.Is(err, injected) {
		t.Fatalf("expected the injected failure to propagate, got %v", err)
	}
	if _, found, err := store.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("expected the publication to NOT be durable after a rolled-back finalize, found=%v err=%v", found, err)
	}
	if _, outstanding, err := store.ReviewPublicationAttempt(decision.ID); err != nil || !outstanding {
		t.Fatalf("expected the attempt marker to remain outstanding after a rolled-back finalize, outstanding=%v err=%v", outstanding, err)
	}

	// Without the injected failure, the SAME call now commits both writes
	// together - proving the hook alone was what blocked it, not some other
	// defect.
	reviewPublicationFinalizeTestHook = nil
	inserted, err := store.FinalizeReviewPublication(review.Publication{DecisionID: decision.ID, Published: true, GitHubReviewID: 1})
	if err != nil || !inserted {
		t.Fatalf("expected finalize to succeed once unblocked, inserted=%v err=%v", inserted, err)
	}
	if _, found, err := store.ReviewPublication(decision.ID); err != nil || !found {
		t.Fatalf("expected the publication to be durable, found=%v err=%v", found, err)
	}
	if _, outstanding, err := store.ReviewPublicationAttempt(decision.ID); err != nil || outstanding {
		t.Fatalf("expected the attempt marker to be cleared, outstanding=%v err=%v", outstanding, err)
	}
}
