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
