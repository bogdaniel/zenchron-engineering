package runtime

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// CreateIssue opens one issue. It is the only way the product creates source
// intake on the forge, and it is reached only through a governed operator
// action; the title and body are the operator's own words, not untrusted input.
func (a GitHubRESTAdapter) CreateIssue(ctx context.Context, repo GitHubRepo, title, body string) (GitHubIssue, error) {
	if strings.TrimSpace(title) == "" {
		return GitHubIssue{}, fmt.Errorf("an issue requires a title")
	}
	var wire ghIssue
	payload := map[string]string{"title": title, "body": body}
	if err := a.call(ctx, repo, http.MethodPost, repoPath(repo)+"/issues", nil, payload, &wire); err != nil {
		return GitHubIssue{}, err
	}
	if wire.Number <= 0 {
		return GitHubIssue{}, fmt.Errorf("the forge created an issue but reported no number")
	}
	return wire.normalize(), nil
}
