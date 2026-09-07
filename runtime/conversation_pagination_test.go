package runtime

// GitHub returns conversation comments OLDEST FIRST. Reading one page therefore
// hides the newest comments - exactly the operator feedback this feature exists
// to observe - and hides them silently, because a full first page looks the same
// as a complete answer unless the Link header is followed.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pagingDoer serves one hundred comments per page and advertises the next page
// through Link, exactly as GitHub does.
type pagingDoer struct {
	pages    int
	requests []string
}

func (d *pagingDoer) Do(r *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, r.URL.String())
	page := 1
	if stated := r.URL.Query().Get("page"); stated != "" {
		fmt.Sscanf(stated, "%d", &page)
	}
	header := http.Header{}
	if page < d.pages {
		header.Set("Link", fmt.Sprintf(`<https://api.github.com/x?page=%d>; rel="next"`, page+1))
	}
	items := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		id := (page-1)*100 + i + 1
		items = append(items, fmt.Sprintf(
			`{"id":%d,"user":{"login":"operator","id":7},"body":"comment %d","created_at":"2026-09-08T00:00:00Z","updated_at":"2026-09-08T00:00:00Z"}`, id, id))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("[" + strings.Join(items, ",") + "]")),
		Header:     header,
	}, nil
}

// TestConversationCommentsFollowEveryPage. The newest comment on a busy thread
// is on the LAST page; a single-page read returns the oldest hundred and reports
// no error, so a busy conversation simply goes quiet forever.
func TestConversationCommentsFollowEveryPage(t *testing.T) {
	doer := &pagingDoer{pages: 3}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: staticCredential{secret: testToken}}

	comments, err := adapter.PullRequestComments(context.Background(), testRepo, 75)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 300 {
		t.Fatalf("read %d comments across %d pages, want 300", len(comments), len(doer.requests))
	}
	// The newest comment is the one an operator just wrote. It is the whole
	// point, and it lives on the last page.
	if newest := comments[len(comments)-1]; newest.ID != 300 {
		t.Fatalf("the newest comment was not returned: got id %d", newest.ID)
	}
	if len(doer.requests) != 3 {
		t.Fatalf("made %d requests, want one per page: %v", len(doer.requests), doer.requests)
	}
}

// TestAnUnboundedConversationIsRefusedRatherThanTruncated mirrors discovery: a
// walk that will not end is an error, never a quietly shortened answer.
func TestAnUnboundedConversationIsRefusedRatherThanTruncated(t *testing.T) {
	doer := &pagingDoer{pages: maxConversationPages + 5}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: staticCredential{secret: testToken}}

	comments, err := adapter.IssueComments(context.Background(), testRepo, 75)
	if err == nil {
		t.Fatalf("an unbounded conversation returned %d comments instead of refusing", len(comments))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("the refusal does not say what it is protecting: %v", err)
	}
}
