package runtime

import (
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
