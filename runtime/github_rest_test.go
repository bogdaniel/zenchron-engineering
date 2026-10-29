package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type contextErrorDoer struct{}

func (contextErrorDoer) Do(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: req.Context().Err()}
}

// TestNormalizeReviewFailsClosedAtTheSourceBoundary is the adapter-boundary
// half of the review-disposition fix: GitHubReviewState.normalized() has
// always been fail-closed for a value it does not recognize, but
// normalizeReview - which turns GitHub's raw wire string into that type - used
// to relabel anything it did not recognize as GitHubReviewCommented instead of
// leaving it unknown. That silently turned a forge adapter bug, or a future
// GitHub review state this runtime does not yet know, into a durable record
// that falsely claims a neutral COMMENT was observed.
func TestNormalizeReviewFailsClosedAtTheSourceBoundary(t *testing.T) {
	cases := []struct {
		wire string
		want GitHubReviewState
	}{
		{"APPROVED", GitHubReviewApproved},
		{"CHANGES_REQUESTED", GitHubReviewChangesRequested},
		{"DISMISSED", GitHubReviewDismissed},
		{"COMMENTED", GitHubReviewCommented},
		// PENDING is a real GitHub review state - a review draft that was never
		// submitted - that this runtime has no disposition for. A future state
		// this runtime has never heard of must land here too.
		{"PENDING", ""},
		{"SOME_FUTURE_STATE", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeReview(c.wire); got != c.want {
			t.Fatalf("normalizeReview(%q) = %q, want %q", c.wire, got, c.want)
		}
	}

	// The unknown value this adapter boundary now produces must reach
	// admission as unknown, and be refused for it, rather than being
	// relabelled COMMENTED somewhere between the wire and the journal.
	unknown := normalizeReview("PENDING")
	item := FeedbackItem{
		Class: FeedbackReview, ID: 1,
		Actor:       GitHubActor{Login: "reviewer", ID: 9},
		Body:        UntrustedText("looks fine, but still consider renaming the helper"),
		ReviewState: unknown,
	}
	policy := FeedbackPolicy{PublicationIdentityResolved: true}
	permissions := map[string]GitHubPermission{"reviewer": PermissionWrite}
	decisions := AdmitFeedback([]FeedbackItem{item}, policy, permissions, "")
	if len(decisions) != 1 {
		t.Fatalf("AdmitFeedback returned %d decisions, want 1: %#v", len(decisions), decisions)
	}
	if d := decisions[0]; d.Admitted || d.Reason != feedbackRefusedNonBlockingReview || d.ReviewState != "" {
		t.Fatalf("a wire-unknown review state was admitted or relabelled COMMENTED: %#v", d)
	}
}

func TestGitHubRESTDoRawPreservesCallerLocalError(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "deadline exceeded"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
				defer cancel()
			}
			adapter := GitHubRESTAdapter{
				HTTP: contextErrorDoer{}, Credentials: staticCredential{secret: testToken},
			}
			status, headers, body, err := adapter.doRaw(ctx, testRepo, http.MethodGet, "/repos/zenchron/fixture", nil, nil, nil)
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("doRaw error = %v, want cause %v", err, ctx.Err())
			}
			if !callerLocalError(err) {
				t.Fatalf("transport error must remain caller-local: %v", err)
			}
			if status != 0 || headers != nil || body != nil {
				t.Fatalf("transport failure returned response data: %d, %v, %q", status, headers, body)
			}
			if !strings.Contains(err.Error(), ctx.Err().Error()) {
				t.Fatalf("transport diagnostic lost its cause: %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("transport diagnostic contains the credential")
			}
		})
	}
}
