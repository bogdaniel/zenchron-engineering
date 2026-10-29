package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// barrierSubmitReviewAdapter wraps FakeGitHubAdapter and blocks SubmitReview
// on a real channel until the test releases it, standing in for a GitHub
// response delayed well past the publish claim's staleness window - exactly
// the #233 B4-B scenario: "the first attempt's response may still be
// accepted" while a second caller considers stealing the claim.
type barrierSubmitReviewAdapter struct {
	*FakeGitHubAdapter
	reached chan struct{}
	release chan struct{}
}

func (a barrierSubmitReviewAdapter) SubmitReview(ctx context.Context, repo GitHubRepo, number int, submission GitHubReviewSubmission) (GitHubReview, error) {
	close(a.reached)
	<-a.release
	return a.FakeGitHubAdapter.SubmitReview(ctx, repo, number, submission)
}

// lockedClock is a concurrency-safe Clock a test can advance from one
// goroutine while another reads it via clockNow - unlike the package's plain
// fakeClock, safe here because the publish-claim renewal goroutine reads it
// concurrently with the test advancing it.
type lockedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waitForClaimRenewedPast blocks until a renewal has landed a
// claimed_unix_nano timestamp at or after "after" - proof that at least one
// renewal has executed against the clock AFTER the test advanced it, rather
// than one still reflecting the time before. It never sleeps blindly: each
// iteration only proceeds on an actual renewal signal from
// reviewClaimRenewedTestHook, bounded by an overall timeout.
func waitForClaimRenewedPast(t *testing.T, store *SQLiteOperationStore, claimKey string, after time.Time, renewed <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-renewed:
			var claimedNano int64
			if err := store.db.QueryRow(`SELECT claimed_unix_nano FROM review_claims WHERE claim_key = ?`, claimKey).Scan(&claimedNano); err != nil {
				t.Fatalf("reading review_claims row: %v", err)
			}
			if !time.Unix(0, claimedNano).UTC().Before(after) {
				return
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for a publish-claim renewal reflecting the advanced clock")
		}
	}
}

// Mutation check (#233 B4-B): a publication claim must be renewed for as
// long as the external SubmitReview call is in flight, so a caller that is
// merely slow - not crashed - can never have its claim stolen and have a
// second caller submit the SAME decision while the first's request may
// still be accepted by GitHub. The clock is advanced well past
// reviewClaimStaleAfter WHILE A is blocked inside SubmitReview; a steal
// attempt at that point must still fail, because A's renewal (proven via
// waitForClaimRenewedPast) has already carried the claim's timestamp past
// the attacker's own threshold. Removing PublishReview's renewal goroutine
// must make this test fail by letting the steal attempt succeed.
func TestPublishReviewRenewsItsClaimAcrossASlowSubmitReviewAndDefeatsASteal(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)

	reached, release := make(chan struct{}), make(chan struct{})
	adapter := barrierSubmitReviewAdapter{FakeGitHubAdapter: fake, reached: reached, release: release}
	clock := &lockedClock{now: time.Unix(1700000000, 0).UTC()}

	originalInterval := reviewClaimRenewalInterval
	reviewClaimRenewalInterval = 5 * time.Millisecond
	defer func() { reviewClaimRenewalInterval = originalInterval }()

	renewed := make(chan struct{}, 64)
	reviewClaimRenewedTestHook = func() {
		select {
		case renewed <- struct{}{}:
		default:
		}
	}
	defer func() { reviewClaimRenewedTestHook = nil }()

	deps := ReviewPublicationDeps{Store: store, GitHub: adapter, Clock: clock}
	done := make(chan error, 1)
	go func() {
		_, err := PublishReview(context.Background(), deps, testRepo, decision)
		done <- err
	}()

	<-reached // A is now blocked inside SubmitReview, its claim held.

	claimKey := "publish:" + decision.ID
	advancedTo := clock.Now().Add(reviewClaimStaleAfter + time.Minute)
	clock.Advance(reviewClaimStaleAfter + time.Minute)
	waitForClaimRenewedPast(t, store, claimKey, advancedTo, renewed)

	// B attempts to steal using a "now" far enough past the ALREADY RENEWED
	// timestamp to be stale relative to it - the same staleness-forcing
	// pattern used elsewhere, proving the renewal (not mere luck in timing)
	// is what defeats this steal.
	stolen, _, err := store.ClaimReview(claimKey, "attacker", advancedTo.Add(time.Second), reviewClaimStaleAfter)
	if err != nil {
		t.Fatalf("attacker's steal attempt: %v", err)
	}
	if stolen {
		t.Fatal("expected A's actively-renewed publish claim to survive a steal attempt, but it was stolen")
	}

	close(release) // let A's SubmitReview finally return.
	if err := <-done; err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if got := submitReviewCallCount(fake); got != 1 {
		t.Fatalf("expected exactly one SubmitReview call, got %d", got)
	}
}

// submitReviewCallCount counts how many times SubmitReview was actually
// invoked on fake, which is what every "never a duplicate POST" assertion in
// this file ultimately needs to check - a refusal that still let the call
// through would be a refusal in name only.
func submitReviewCallCount(fake *FakeGitHubAdapter) int {
	count := 0
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			count++
		}
	}
	return count
}

// Mutation check (#233 P2): a repository argument that does not match the
// decision's own subject must be refused before any claim, read or write -
// never silently publishing one repository's verdict against a different
// repository's pull request. Removing the repository check in PublishReview
// must make this test fail by letting SubmitReview (or any GitHub call) run.
func TestPublishReviewRefusesARepositoryMismatchBeforeAnyExternalCall(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)

	otherRepo := GitHubRepo{Owner: "someone-else", Name: "unrelated"}
	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, otherRepo, decision)
	var mismatch *ReviewRepositoryMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected a repository mismatch refusal, got %v", err)
	}
	if len(fake.Methods()) != 0 {
		t.Fatalf("expected no GitHub calls at all before the mismatch is checked, got %v", fake.Methods())
	}
	if _, found, err := store.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("expected no publication to be recorded, found=%v err=%v", found, err)
	}
}

// Mutation check (#233 P1): an outstanding, unresolved publication attempt -
// recorded here exactly as an earlier, now-crashed invocation would have left
// it, BEFORE PublishReview ever adds its own fresh call - must refuse a new
// submission rather than treat GitHub's silence as proof nothing landed.
// Uses a SEPARATE store handle on the same database file (openPair) to
// simulate the second invocation being a different, restarted process, not
// merely a second call sharing the first one's Go state. Removing the
// outstanding-attempt check in PublishReview must make this test fail by
// letting SubmitReview run.
func TestPublishReviewRefusesASecondSubmissionWhileAnEarlierOneIsUncertain(t *testing.T) {
	_, first, second := openPair(t)
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, BaseRef: "main", State: GitHubOpen}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}
	decision := publishableDecision(t, first, review.VerdictApprove)

	// Exactly what PublishReview itself writes immediately before calling
	// SubmitReview - simulated directly here to stand in for an earlier
	// invocation that started a submission and never confirmed its outcome.
	if err := first.RecordReviewPublicationAttempt(decision.ID, time.Now().UTC()); err != nil {
		t.Fatalf("seeding the outstanding attempt: %v", err)
	}

	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: second, GitHub: fake}, testRepo, decision)
	var uncertain *ReviewPublicationUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("expected a publication-uncertain refusal, got %v", err)
	}
	if got := submitReviewCallCount(fake); got != 0 {
		t.Fatalf("expected no SubmitReview call while an earlier attempt's outcome is unknown, got %d", got)
	}
	if _, found, err := second.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("expected no publication to be durably recorded, found=%v err=%v", found, err)
	}
}

// Mutation check (#233 P1): once GitHub's own matching review becomes
// observable, an outstanding attempt resolves through reconciliation alone -
// never through a second SubmitReview. Uses openPair the same way the
// refusal test above does, so this is also "recovery after restart": a
// second process, with no memory of the first's in-process state, correctly
// finishes what the first one started.
func TestPublishReviewResolvesAnOutstandingAttemptFromReconciliationAlone(t *testing.T) {
	_, first, second := openPair(t)
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, BaseRef: "main", State: GitHubOpen}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}
	decision := publishableDecision(t, first, review.VerdictApprove)

	if err := first.RecordReviewPublicationAttempt(decision.ID, time.Now().UTC()); err != nil {
		t.Fatalf("seeding the outstanding attempt: %v", err)
	}
	// GitHub's own state now shows the first attempt actually landed - the
	// marker-bearing review the first (crashed) invocation's SubmitReview
	// call produced, discovered here exactly as a real restart would.
	fake.ReviewsByHead[testHeadSHA] = GitHubReviewObservation{Reviews: []GitHubReview{{
		ID: 909, Author: GitHubActor{Login: "zenchron-engineering[bot]"}, State: GitHubReviewApproved,
		CommitSHA: testHeadSHA, Body: UntrustedText(reviewPublicationBody(decision)),
	}}}

	publication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: second, GitHub: fake}, testRepo, decision)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if !publication.Published || publication.GitHubReviewID != 909 {
		t.Fatalf("expected the resolved publication to reflect the observed review, got %+v", publication)
	}
	if got := submitReviewCallCount(fake); got != 0 {
		t.Fatalf("expected resolution from reconciliation alone, no new SubmitReview call, got %d", got)
	}
	if _, outstanding, err := second.ReviewPublicationAttempt(decision.ID); err != nil || outstanding {
		t.Fatalf("expected the attempt to be cleared once resolved, outstanding=%v err=%v", outstanding, err)
	}
}

// Mutation check (#233 P1): the full interleaving the re-review specifically
// asked for - A's claim is stolen (a lease expiry, forced deterministically
// the same way the B4 store tests already do) WHILE A is genuinely blocked
// inside a delayed SubmitReview, using a separate store handle for B to
// stand in for a different, restarted process. B must refuse to submit a
// second review; only once A's own delayed response eventually returns does
// the decision resolve - and exactly one SubmitReview call ever reaches
// GitHub. Removing either the pre-POST attempt write or the
// outstanding-attempt check must make this test fail by letting B submit.
func TestPublishReviewNeverDoublePublishesAcrossALeaseStealDuringADelayedSubmit(t *testing.T) {
	_, storeA, storeB := openPair(t)
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, BaseRef: "main", State: GitHubOpen}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}
	decision := publishableDecision(t, storeA, review.VerdictApprove)

	reached, release := make(chan struct{}), make(chan struct{})
	adapter := barrierSubmitReviewAdapter{FakeGitHubAdapter: fake, reached: reached, release: release}

	aDone := make(chan error, 1)
	go func() {
		_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: storeA, GitHub: adapter}, testRepo, decision)
		aDone <- err
	}()
	<-reached // A has recorded its attempt and is now blocked inside SubmitReview.

	// B's own PublishReview call claims this same key itself, same as A's
	// did - the fix under test is what happens AFTER that claim succeeds,
	// not the claim mechanics themselves (already covered by the B4 store
	// tests). B's clock reads far enough past A's real claim time that B's
	// OWN internal ClaimReview call reads A's claim as stale and steals it -
	// the same artificial-"now" pattern TestClaimReviewReclaimsAStaleClaim
	// already uses - standing in for A's process having crashed or stopped
	// renewing, not merely being slow.
	stolenAt := time.Now().UTC().Add(reviewClaimStaleAfter + time.Minute)
	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: storeB, GitHub: adapter, Clock: fixedClock{at: stolenAt}}, testRepo, decision)
	var uncertain *ReviewPublicationUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("expected B to refuse with a publication-uncertain error, got %v", err)
	}
	if got := submitReviewCallCount(fake); got != 0 {
		t.Fatalf("expected A's still-in-flight SubmitReview to be the only call so far, got %d completed", got)
	}

	close(release) // A's delayed response finally arrives.
	if err := <-aDone; err != nil {
		t.Fatalf("A's PublishReview: %v", err)
	}
	if got := submitReviewCallCount(fake); got != 1 {
		t.Fatalf("expected exactly one SubmitReview call in total, got %d", got)
	}
	published, found, err := storeB.ReviewPublication(decision.ID)
	if err != nil || !found || !published.Published {
		t.Fatalf("expected A's eventual success to be durably recorded, found=%v published=%+v err=%v", found, published, err)
	}
	if _, outstanding, err := storeB.ReviewPublicationAttempt(decision.ID); err != nil || outstanding {
		t.Fatalf("expected the attempt to be cleared once A resolved it, outstanding=%v err=%v", outstanding, err)
	}
}

// Mutation check (#233 P1-A): the exact race the round-3 re-review
// reproduced - A reads "no attempt outstanding", then pauses BEFORE writing
// its own marker; A's lease expires and B (a separate store handle) steals
// it, is admitted, submits, and finishes entirely; A then resumes and
// attempts its own admission. A must be refused by the atomic gate - never
// silently overwrite B's already-resolved marker and submit a second time.
// Reverting AdmitReviewPublicationAttempt's atomic write back to the old
// unconditional-upsert RecordReviewPublicationAttempt at this call site must
// make this test fail by letting A submit too.
func TestPublishReviewRefusesAdmissionAfterLosingTheAtomicGateToAStealingCaller(t *testing.T) {
	_, storeA, storeB := openPair(t)
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, BaseRef: "main", State: GitHubOpen}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}
	decision := publishableDecision(t, storeA, review.VerdictApprove)

	reached, proceed := make(chan struct{}), make(chan struct{})
	var hookFired atomic.Bool
	reviewPublishAdmissionTestHook = func() {
		if hookFired.CompareAndSwap(false, true) {
			close(reached)
			<-proceed
		}
	}
	defer func() { reviewPublishAdmissionTestHook = nil }()

	aDone := make(chan error, 1)
	go func() {
		_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: storeA, GitHub: fake}, testRepo, decision)
		aDone <- err
	}()
	<-reached // A has read "no attempt outstanding" and is parked right before admitting.

	// B steals A's claim via an artificially advanced clock (the same
	// pattern as the delayed-submit test above - no real waiting), is
	// admitted, and runs SubmitReview to completion before A is ever let go.
	stolenAt := time.Now().UTC().Add(reviewClaimStaleAfter + time.Minute)
	bPublication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: storeB, GitHub: fake, Clock: fixedClock{at: stolenAt}}, testRepo, decision)
	if err != nil || !bPublication.Published {
		t.Fatalf("B's PublishReview: %+v %v", bPublication, err)
	}

	close(proceed) // let A resume and attempt its own (now-stale) admission.
	aErr := <-aDone
	var admissionLost *ReviewPublicationAdmissionLostError
	if !errors.As(aErr, &admissionLost) {
		t.Fatalf("expected A to lose the atomic admission gate, got %v", aErr)
	}
	if got := submitReviewCallCount(fake); got != 1 {
		t.Fatalf("expected exactly one SubmitReview call (B's), got %d", got)
	}
	published, found, err := storeA.ReviewPublication(decision.ID)
	if err != nil || !found || published.GitHubReviewID != bPublication.GitHubReviewID {
		t.Fatalf("expected the durable publication to be B's and only B's, found=%v published=%+v err=%v", found, published, err)
	}
}

// Mutation check (#233 P1-B, end to end): a finalize failure injected
// exactly where the re-review asked - between the publication write and the
// attempt clear - must leave recovery safe: the SubmitReview call that
// already happened is never repeated, and the retry resolves purely from
// reconciling GitHub's own review (which that first, already-successful
// SubmitReview call produced - the fake forge makes a submitted review
// observable immediately, the same way a real one eventually does), never
// from a second POST. Removing FinalizeReviewPublication's transactional
// rollback (letting the publication write commit independently of the
// injected failure) must make this test fail: the retry would then find an
// already-published decision through the ordinary "found && Published"
// short-circuit instead of the outstanding-attempt reconciliation path this
// test is actually proving, and the first assertion below (no durable
// publication right after the injected failure) would fail outright.
func TestPublishReviewRecoversSafelyAfterAFinalizeFailureWithoutASecondSubmission(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)

	injected := fmt.Errorf("simulated crash between the publication write and the attempt clear")
	reviewPublicationFinalizeTestHook = func() error { return injected }
	defer func() { reviewPublicationFinalizeTestHook = nil }()

	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if !errors.Is(err, injected) {
		t.Fatalf("expected the injected finalize failure to propagate, got %v", err)
	}
	if _, found, err := store.ReviewPublication(decision.ID); err != nil || found {
		t.Fatalf("expected no durable publication after the rolled-back finalize, found=%v err=%v", found, err)
	}
	if _, outstanding, err := store.ReviewPublicationAttempt(decision.ID); err != nil || !outstanding {
		t.Fatalf("expected the attempt marker to remain outstanding after the rolled-back finalize, outstanding=%v err=%v", outstanding, err)
	}
	if got := submitReviewCallCount(fake); got != 1 {
		t.Fatalf("expected exactly one SubmitReview call so far (the one whose finalize was interrupted), got %d", got)
	}

	// Recovery: the attempt is still outstanding, so this retry takes the
	// outstanding-attempt reconciliation path - and resolves there, finding
	// the review the FIRST call's SubmitReview already posted - never by
	// calling SubmitReview again.
	reviewPublicationFinalizeTestHook = nil
	resolved, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil || !resolved.Published {
		t.Fatalf("expected recovery to resolve the decision as published, got %+v %v", resolved, err)
	}
	if got := submitReviewCallCount(fake); got != 1 {
		t.Fatalf("expected recovery to resolve without ever submitting a second review, got %d SubmitReview calls", got)
	}
	if _, outstanding, err := store.ReviewPublicationAttempt(decision.ID); err != nil || outstanding {
		t.Fatalf("expected the attempt marker to be cleared once recovery resolved it, outstanding=%v err=%v", outstanding, err)
	}
}
