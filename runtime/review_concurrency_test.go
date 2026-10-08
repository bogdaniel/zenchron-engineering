package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"

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
// decision must never both reach GitHub. Removing publishReviewClaimed's
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

	claimed, err := store.ClaimReview("publish:"+first.Decision.ID, "a-concurrent-publisher", clockNow(in.Clock), reviewClaimStaleAfter)
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
