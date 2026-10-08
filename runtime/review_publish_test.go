package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/review"
)

func publishableDecision(t *testing.T, store *SQLiteOperationStore, verdict review.Verdict) review.Decision {
	t.Helper()
	subject := review.Subject{Repository: testRepo.String(), PRNumber: 7, HeadSHA: testHeadSHA}
	id, err := review.DecisionID(subject, "claude")
	if err != nil {
		t.Fatal(err)
	}
	d := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject,
		RunID: "run-1", ReviewerAgentID: "claude", Verdict: verdict, CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if verdict == review.VerdictRequestChanges {
		d.Findings = []review.Finding{{Severity: review.SeverityBlocking, Signature: "real defect"}}
	}
	stored, _, err := store.CreateReviewDecision(d)
	if err != nil {
		t.Fatalf("CreateReviewDecision: %v", err)
	}
	return stored
}

func newReviewPublishFixture(t *testing.T) (*SQLiteOperationStore, *FakeGitHubAdapter) {
	t.Helper()
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteOperationStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	fake := NewFakeGitHubAdapter()
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testHeadSHA, BaseSHA: testOtherSHA, BaseRef: "main", State: GitHubOpen}
	fake.ViewerActor = GitHubActor{Login: "zenchron-engineering[bot]"}
	return store, fake
}

func TestPublishReviewPublishesOnceAndIsIdempotent(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)

	first, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil || !first.Published || first.GitHubReviewID == 0 {
		t.Fatalf("first PublishReview: %+v %v", first, err)
	}
	second, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil {
		t.Fatalf("second PublishReview: %v", err)
	}
	if second.GitHubReviewID != first.GitHubReviewID {
		t.Fatalf("expected the second call to return the SAME publication, got %+v vs %+v", second, first)
	}
	submits := 0
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			submits++
		}
	}
	if submits != 1 {
		t.Fatalf("expected exactly one SubmitReview call across both PublishReview calls, got %d", submits)
	}
}

func TestPublishReviewRefusesAStaleHead(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	fake.PullRequests[7] = GitHubPullRequest{Number: 7, HeadSHA: testOtherSHA, BaseSHA: testOtherSHA, State: GitHubOpen} // PR has moved
	decision := publishableDecision(t, store, review.VerdictApprove)                                                     // bound to testHeadSHA

	if _, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision); err == nil {
		t.Fatal("expected publishing a decision bound to a superseded head to be refused")
	}
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			t.Fatal("expected no SubmitReview call for a stale decision")
		}
	}
}

// Crash recovery: GitHub already shows a review from this runtime's own
// identity for this exact head and decision reviewer, but the local
// publication row was never written (the crash window between SubmitReview
// succeeding and RecordReviewPublication committing). PublishReview must
// reconcile from the observation rather than submitting a second review.
func TestPublishReviewReconcilesAnAlreadyObservedReviewWithoutDuplicating(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)
	fake.ReviewsByHead[testHeadSHA] = GitHubReviewObservation{
		Reviews: []GitHubReview{{ID: 555, Author: fake.ViewerActor, State: GitHubReviewApproved, CommitSHA: testHeadSHA,
			Body: UntrustedText("looks good\n\n" + reviewDecisionMarker(decision.ID))}},
	}

	publication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil || !publication.Published || publication.GitHubReviewID != 555 {
		t.Fatalf("expected reconciliation from the observed review, got %+v %v", publication, err)
	}
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			t.Fatal("expected no SubmitReview call when GitHub already shows this runtime's own review")
		}
	}
}

// #233 requirement 13: when GitHub refuses the configured identity's APPROVE
// (self-approval, or any other refusal), fall back to an explicit COMMENT_ONLY
// publication rather than failing the whole publish or pretending GitHub
// approved.
func TestPublishReviewFallsBackToCommentOnlyWhenApprovalIsRefused(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)
	attempts := 0
	fake.Fail = func(call GitHubCall) error {
		if call.Method != "SubmitReview" {
			return nil
		}
		attempts++
		if attempts == 1 {
			return &GitHubSelfApprovalRejectedError{Detail: "Review cannot be submitted as approval by the author of the pull request"}
		}
		return nil
	}

	publication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if publication.PublishedVerdict != review.VerdictCommentOnly {
		t.Fatalf("expected a COMMENT_ONLY fallback, got %q", publication.PublishedVerdict)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly one retry after the refused approval, got %d attempts", attempts)
	}
}

// Mutation check (#233 B2): only GitHub's OWN definitive self-approval
// rejection may be downgraded to COMMENT_ONLY. Any other failure - a
// transient 5xx here - must be preserved for replay. Removing the
// errors.As(*GitHubSelfApprovalRejectedError) narrowing in PublishReview
// (reverting to "any error during APPROVE falls back") must make this test
// fail by returning a COMMENT_ONLY publication instead of propagating the
// error.
func TestPublishReviewNeverFallsBackOnATransientFailure(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)
	fake.Fail = func(call GitHubCall) error {
		if call.Method != "SubmitReview" {
			return nil
		}
		return &GitHubTransientError{Status: 503, Detail: "server error"}
	}

	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err == nil {
		t.Fatal("expected a transient failure to propagate rather than fall back to COMMENT_ONLY")
	}
	if published, found, _ := store.ReviewPublication(decision.ID); found && published.Published {
		t.Fatal("a failed publish must not record a durable publication")
	}
}

// A SubmitReview client-side error (e.g. a lost reply) must not be treated as
// failure when GitHub's own state, read back through the decision marker,
// shows the review already landed.
func TestPublishReviewReconcilesAfterASubmitErrorWhenGitHubAlreadyShowsIt(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)
	attempts := 0
	fake.Fail = func(call GitHubCall) error {
		if call.Method != "SubmitReview" {
			return nil
		}
		attempts++
		if attempts == 1 {
			// The client never saw the (successful) response, but GitHub's
			// own state - scripted below - already carries the review.
			fake.ReviewsByHead[testHeadSHA] = GitHubReviewObservation{
				Reviews: []GitHubReview{{ID: 777, Author: fake.ViewerActor, State: GitHubReviewApproved, CommitSHA: testHeadSHA,
					Body: UntrustedText("looks good\n\n" + reviewDecisionMarker(decision.ID))}},
			}
			return &GitHubTransientError{Status: 0, Detail: "connection reset"}
		}
		return nil
	}

	publication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, decision)
	if err != nil || !publication.Published || publication.GitHubReviewID != 777 {
		t.Fatalf("expected reconciliation from GitHub's own state after a lost reply, got %+v %v", publication, err)
	}
}

// Mutation check (#233 B1): two DIFFERENT decisions on the same exact head,
// both published by this runtime's identity, must never be confused with one
// another. Removing the marker-based match (or the ambiguity refusal) in
// observeOwnPublishedReview must make this test fail by letting an unrelated
// review satisfy a decision it was never proof of.
func TestPublishReviewRefusesAnUnrelatedOwnReviewOnTheSameHead(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	// A DIFFERENT reviewer already published an APPROVE on this exact head.
	other := publishableDecisionForReviewer(t, store, review.VerdictApprove, "codex")
	if _, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, other); err != nil {
		t.Fatalf("publishing the other decision: %v", err)
	}

	// This decision (a DIFFERENT reviewer, REQUEST_CHANGES) must still be
	// published for real, not silently satisfied by the unrelated review
	// already on the head.
	blocking := publishableDecision(t, store, review.VerdictRequestChanges)
	publication, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: fake}, testRepo, blocking)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if publication.PublishedVerdict != review.VerdictRequestChanges {
		t.Fatalf("expected the blocking decision's own REQUEST_CHANGES to be published, got %q", publication.PublishedVerdict)
	}
	submits := 0
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			submits++
		}
	}
	if submits != 2 {
		t.Fatalf("expected both decisions to each submit their own review, got %d SubmitReview calls", submits)
	}
}

func publishableDecisionForReviewer(t *testing.T, store *SQLiteOperationStore, verdict review.Verdict, reviewerAgentID string) review.Decision {
	t.Helper()
	subject := review.Subject{Repository: testRepo.String(), PRNumber: 7, HeadSHA: testHeadSHA}
	id, err := review.DecisionID(subject, reviewerAgentID)
	if err != nil {
		t.Fatal(err)
	}
	d := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: id, Subject: subject,
		RunID: "run-1", ReviewerAgentID: reviewerAgentID, Verdict: verdict, CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if verdict == review.VerdictRequestChanges {
		d.Findings = []review.Finding{{Severity: review.SeverityBlocking, Signature: "real defect"}}
	}
	stored, _, err := store.CreateReviewDecision(d)
	if err != nil {
		t.Fatalf("CreateReviewDecision: %v", err)
	}
	return stored
}

// Mutation check: PublishReview must fail closed when it cannot resolve its
// own publishing identity, never silently treat that as "nothing published
// yet" and proceed to submit - which could duplicate an earlier publication
// this runtime simply cannot see without knowing who it is. Removing the
// viewer-error propagation in observeOwnPublishedReview must make this test
// fail by letting PublishReview proceed to SubmitReview.
func TestPublishReviewFailsClosedWhenItsOwnIdentityCannotBeResolved(t *testing.T) {
	store, fake := newReviewPublishFixture(t)
	decision := publishableDecision(t, store, review.VerdictApprove)
	failingViewer := &viewerErrorForge{FakeGitHubAdapter: fake}

	_, err := PublishReview(context.Background(), ReviewPublicationDeps{Store: store, GitHub: failingViewer}, testRepo, decision)
	if err == nil {
		t.Fatal("expected a failed self-identity resolution to refuse publication")
	}
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			t.Fatal("expected no SubmitReview call when self-identity could not be resolved")
		}
	}
}

// viewerErrorForge wraps FakeGitHubAdapter and fails Viewer specifically,
// since FakeGitHubAdapter's own Fail hook is consulted by every method and
// this test needs ONLY Viewer to fail.
type viewerErrorForge struct{ *FakeGitHubAdapter }

func (v *viewerErrorForge) Viewer(context.Context, GitHubRepo) (GitHubActor, error) {
	return GitHubActor{}, fmt.Errorf("could not resolve viewer identity")
}

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
