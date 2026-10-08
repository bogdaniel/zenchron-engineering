package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// blockingReviewProvider lets a test deterministically observe "invocation
// has started" before a second, concurrent caller is allowed to race it.
type blockingReviewProvider struct {
	started     chan struct{}
	release     chan struct{}
	invocations *int32
}

func (p blockingReviewProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	atomic.AddInt32(p.invocations, 1)
	close(p.started)
	<-p.release
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		return ExecutionResult{}, err
	}
	if err := os.WriteFile(request.ReviewerResultPath, document, 0600); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{ProviderID: "blocking-reviewer", Outcome: execution.Succeeded}, nil
}

// Mutation check (#233 B4): two concurrent RunIndependentReview calls for the
// EXACT SAME subject and reviewer must never both invoke a provider. Removing
// the ClaimReview call (or its defer release) in RunIndependentReview must
// make this test fail by letting a second provider invocation through, or by
// deadlocking the second caller instead of refusing it immediately.
func TestRunIndependentReviewRefusesAConcurrentClaimForTheSameSubjectAndReviewer(t *testing.T) {
	in, _, _ := reviewRunFixture(t)
	var invocations int32
	started, release := make(chan struct{}), make(chan struct{})
	in.Provider = blockingReviewProvider{started: started, release: release, invocations: &invocations}

	firstDone := make(chan error, 1)
	go func() {
		_, err := RunIndependentReview(context.Background(), in)
		firstDone <- err
	}()
	<-started // the first call has claimed the review and entered the provider

	_, secondErr := RunIndependentReview(context.Background(), in)
	var conflict *ReviewClaimConflictError
	if !errors.As(secondErr, &conflict) {
		t.Fatalf("expected the concurrent second call to be refused with a claim conflict, got %v", secondErr)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt32(&invocations); got != 1 {
		t.Fatalf("expected exactly one provider invocation across both calls, got %d", got)
	}
}

// Mutation check (#233 B4): two concurrent publish attempts for the SAME
// decision must never both reach GitHub. Removing PublishReview's own
// ClaimReview call must make this test fail by letting a second SubmitReview
// call through.
func TestRunIndependentReviewRefusesAConcurrentPublishClaim(t *testing.T) {
	in, fake, store := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	in.Provider = reviewStubProvider{document: string(document)}
	first, err := RunIndependentReview(context.Background(), in)
	if err != nil || !first.Created {
		t.Fatalf("seeding the decision: %+v %v", first, err)
	}

	claimed, _, err := store.ClaimReview("publish:"+first.Decision.ID, "a-concurrent-publisher", clockNow(in.Clock), reviewClaimStaleAfter)
	if err != nil || !claimed {
		t.Fatalf("simulating a concurrent publisher's claim: claimed=%v err=%v", claimed, err)
	}

	in.Publish = true
	_, err = RunIndependentReview(context.Background(), in)
	var conflict *ReviewClaimConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a held publish claim to refuse this call, got %v", err)
	}
	for _, call := range fake.Methods() {
		if call == "SubmitReview" {
			t.Fatal("expected no SubmitReview call while another caller holds the publish claim")
		}
	}
}

// Mutation check (#233 B4-1): a caller whose pre-claim "no decision yet"
// read genuinely raced another caller's entire claimed section must, upon
// WINNING the claim afterward, discover the decision now exists and must NOT
// invoke a second provider. Removing the post-claim re-check in
// RunIndependentReview must make this test fail by letting a second provider
// invocation through.
func TestRunIndependentReviewRereadsTheDecisionAfterWinningAStaleClaim(t *testing.T) {
	in, _, _ := reviewRunFixture(t)
	document, err := json.Marshal(ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted})
	if err != nil {
		t.Fatal(err)
	}
	var invocations int32
	inB := in
	inB.Provider = countingExecuteProvider{document: string(document), calls: &invocations}

	reached := make(chan struct{})
	proceed := make(chan struct{})
	reviewClaimTestHook = func() {
		close(reached)
		<-proceed
	}
	defer func() { reviewClaimTestHook = nil }()

	bDone := make(chan error, 1)
	go func() {
		_, err := RunIndependentReview(context.Background(), inB)
		bDone <- err
	}()
	<-reached // B has read "no decision yet" and is parked before claiming.
	reviewClaimTestHook = nil

	// A runs to completion first - entirely before B is allowed to proceed.
	inA := in
	inA.Provider = reviewStubProvider{document: string(document)}
	aOut, err := RunIndependentReview(context.Background(), inA)
	if err != nil || !aOut.Created {
		t.Fatalf("A's run: %+v %v", aOut, err)
	}

	close(proceed) // let B continue: it will now win the claim A released.
	if err := <-bDone; err != nil {
		t.Fatalf("B's run: %v", err)
	}
	if got := atomic.LoadInt32(&invocations); got != 0 {
		t.Fatalf("expected B to find A's decision and never invoke its own provider, got %d invocations", got)
	}
}

// Mutation check (#233 B4-A): a claim stolen WHILE the original owner is
// still blocked inside the provider must fence that owner's own decision
// admission - the steal makes the owner's eventual verdict unfenced, not
// merely late. A's own claim is stolen (by directly manipulating the store,
// the same deterministic staleness-forcing pattern review_store_test.go
// already uses, rather than waiting out the real staleness window) while A
// is parked in the provider; A is then released and must refuse to admit a
// decision rather than writing one under a token it no longer holds.
// Reverting CreateReviewDecisionFenced to the unfenced CreateReviewDecision
// in RunIndependentReview must make this test fail by letting A's decision
// land anyway.
func TestRunIndependentReviewRefusesAdmissionAfterItsClaimIsStolenMidInvocation(t *testing.T) {
	in, _, store := reviewRunFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var invocations int32
	in.Provider = blockingReviewProvider{started: started, release: release, invocations: &invocations}

	aDone := make(chan error, 1)
	go func() {
		_, err := RunIndependentReview(context.Background(), in)
		aDone <- err
	}()
	<-started // A has claimed the review and is now blocked inside the provider.

	// Steal A's claim exactly the way TestClaimReviewReclaimsAStaleClaim does:
	// pass an artificial "now" far enough past A's claim time that it reads
	// as abandoned, with no real waiting. The claim key is every live row in
	// review_claims at this point - there is exactly one, A's own.
	claimKey := onlyReviewClaimKey(t, store)
	stolenAt := clockNow(in.Clock).Add(reviewClaimStaleAfter + time.Minute)
	stolen, _, err := store.ClaimReview(claimKey, "attacker", stolenAt, reviewClaimStaleAfter)
	if err != nil || !stolen {
		t.Fatalf("stealing A's claim: stolen=%v err=%v", stolen, err)
	}

	close(release) // let A finish its (now unfenced) provider invocation.
	aErr := <-aDone
	var lost *ReviewClaimLostError
	if !errors.As(aErr, &lost) {
		t.Fatalf("expected A to be refused admission with a lost-claim error, got %v", aErr)
	}
	if _, found, err := store.ReviewDecision(claimKey); err != nil || found {
		t.Fatalf("expected no durable decision to have been admitted, found=%v err=%v", found, err)
	}
}

// onlyReviewClaimKey reads back the single claim key review_claims currently
// holds, for a test that needs to steal "whichever key A is using" without
// independently recomputing review.DecisionID itself.
func onlyReviewClaimKey(t *testing.T, store *SQLiteOperationStore) string {
	t.Helper()
	rows, err := store.db.Query(`SELECT claim_key FROM review_claims`)
	if err != nil {
		t.Fatalf("querying review_claims: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scanning claim_key: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating review_claims: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected exactly one live claim, found %d: %v", len(keys), keys)
	}
	return keys[0]
}

// countingExecuteProvider counts invocations rather than blocking, so
// TestRunIndependentReviewRereadsTheDecisionAfterWinningAStaleClaim can prove
// zero invocations happened rather than exactly one.
type countingExecuteProvider struct {
	document string
	calls    *int32
}

func (p countingExecuteProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	atomic.AddInt32(p.calls, 1)
	return reviewStubProvider{document: p.document}.Execute(ctx, request)
}
