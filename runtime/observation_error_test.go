package runtime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestObserveGitHubMissingWorkspaceDoesNotRecordExternalChange(t *testing.T) {
	forge := &FakeGitHubAdapter{PullRequests: map[int]GitHubPullRequest{
		1: {Number: 1, HeadSHA: "previous-runtime-head"},
	}}
	r := &EngineeringRuntime{deps: Dependencies{StateDir: t.TempDir(), GitHub: forge}}
	state := &runState{}
	state.run.ID = "missing-workspace"
	state.projection.CandidateRevision = "current-runtime-head"
	state.projection.PullRequest = &PullRequestObservation{GitHubPRObservedPayload: GitHubPRObservedPayload{Number: 1}}
	for attempt := 0; attempt < 2; attempt++ {
		result := r.observeGitHub(context.Background(), state, RunOperation{})
		if result.state != OperationFailed {
			t.Fatalf("observation state = %s, want failure", result.state)
		}
		if len(result.events) != 0 {
			t.Fatalf("failed ancestry observation produced events: %+v", result.events)
		}
	}
}

func TestGitRunnerPreservesFailureStatus(t *testing.T) {
	_, err := runGit(t.TempDir(), "merge-base", "--is-ancestor", "invalid-observed-revision", "HEAD")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 1 {
		t.Fatalf("repository failure must preserve its non-ancestry exit status: %v", err)
	}
}
