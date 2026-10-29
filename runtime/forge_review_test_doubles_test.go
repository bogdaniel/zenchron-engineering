package runtime

// SubmitReview (#233) for the two GitHubAdapter test doubles that forward to
// a wrapped adapter method-by-method (routingForge in watch_test.go,
// sharedForge in watch_matrix_test.go). Kept apart from both - each at its
// frozen file-size ceiling - since neither type needs more than this one
// extra forwarding method to keep satisfying the GitHubAdapter interface.

import "context"

func (r routingForge) SubmitReview(ctx context.Context, repo GitHubRepo, number int, submission GitHubReviewSubmission) (GitHubReview, error) {
	return r.adapter(repo).SubmitReview(ctx, repo, number, submission)
}

func (s *sharedForge) SubmitReview(ctx context.Context, repo GitHubRepo, number int, submission GitHubReviewSubmission) (GitHubReview, error) {
	s.enter("SubmitReview")
	defer s.leave("SubmitReview")
	return s.forge.SubmitReview(ctx, repo, number, submission)
}
