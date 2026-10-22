package runtime

import (
	"context"
	"testing"
)

type issueOpeningForge struct {
	*FakeGitHubAdapter
	opened int
}

func (f *issueOpeningForge) CreateIssue(context.Context, GitHubRepo, string, string) (GitHubIssue, error) {
	f.opened++
	return GitHubIssue{Number: 7}, nil
}

// The supervisor hands every consumer its forge through MultiplexedForge, so
// issue creation has to survive the wrapper in both directions.
func TestMultiplexedForgeForwardsIssueCreationOnlyWhenTheInnerForgeCanCreate(t *testing.T) {
	repo := GitHubRepo{Owner: "acme", Name: "repo"}
	inner := &issueOpeningForge{FakeGitHubAdapter: NewFakeGitHubAdapter()}
	issue, err := NewMultiplexedForge(inner, newSteppingClock()).CreateIssue(context.Background(), repo, "t", "b")
	if err != nil || issue.Number != 7 || inner.opened != 1 {
		t.Fatalf("issue = %+v err = %v opened = %d, want the inner forge's issue opened once", issue, err, inner.opened)
	}
	if _, err := NewMultiplexedForge(NewFakeGitHubAdapter(), newSteppingClock()).CreateIssue(context.Background(), repo, "t", "b"); err == nil {
		t.Fatal("a forge that cannot create issues was reported as having created one")
	}
}
