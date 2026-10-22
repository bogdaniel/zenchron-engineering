package runtime

import (
	"context"
	"errors"
)

// NewIssueCreator is the one forge capability issue creation needs. It is not
// part of GitHubAdapter so every other adapter and fake keeps its shape.
type NewIssueCreator interface {
	CreateIssue(ctx context.Context, repo GitHubRepo, title, body string) (GitHubIssue, error)
}

// CreateIssue forwards to the wrapped adapter when it can create issues. The
// supervisor wraps its forge in this decorator, so without the forward the
// dashboard's issue-create would be refused on every real controller. Like any
// write it is never coalesced and always invalidates.
func (m *MultiplexedForge) CreateIssue(ctx context.Context, repo GitHubRepo, title, body string) (GitHubIssue, error) {
	creator, ok := m.Inner.(NewIssueCreator)
	if !ok {
		return GitHubIssue{}, errors.New("the configured forge cannot create issues")
	}
	defer m.invalidate()
	return creator.CreateIssue(ctx, repo, title, body)
}
