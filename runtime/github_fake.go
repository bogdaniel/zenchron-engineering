package runtime

// FakeGitHubAdapter is the in-memory GitHubAdapter ordinary tests use. It needs
// no token and no network, so `go test ./...` is offline by construction.
//
// It is a single-goroutine test double: the state is exported so a test scripts
// it by assignment, and there is no locking.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GitHubCall is one recorded invocation. Only the fields a call actually names
// are set, so an assertion reads as "this method, this head SHA".
type GitHubCall struct {
	Method  string
	Repo    GitHubRepo
	Number  int
	Ref     string
	BaseRef string
	SHA     string
	Body    string
	Label   string
	ETag    string
}

type FakeGitHubAdapter struct {
	Issues       map[int]GitHubIssue
	PullRequests map[int]GitHubPullRequest
	// Keyed by the exact head SHA, which is how the real thing is queried.
	ChecksByHead  map[string]GitHubCheckObservation
	ReviewsByHead map[string]GitHubReviewObservation
	// Refs maps a remote ref to its SHA: the push-landed question.
	Refs     map[string]string
	Comments map[int][]string
	// NextNumber is the number CreatePullRequest assigns.
	NextNumber int
	// Discoveries scripts successive DiscoverIssues answers, consumed in order;
	// the last one repeats. Empty means "derive the answer from Issues", which
	// is the ordinary case: every issue in Issues that carries the opt-in label.
	//
	// A step whose ETag is non-empty behaves like the real forge: a query that
	// replays that same ETag is answered NotModified with no issues, and any
	// other query gets the step's issues. Scripting the multi-page case is a
	// step with more issues than a page holds and Pages set accordingly - by
	// this seam pagination has already happened, and a multi-page step is the
	// one that must hand back an empty ETag.
	//
	// Failures - auth, transient, rate-limited - are scripted through Fail,
	// which is consulted for DiscoverIssues like every other call.
	Discoveries []DiscoveryResult
	// discovered is the position in Discoveries. It is not exported because it
	// is a cursor, not a fixture.
	discovered int
	// Fail is the failure injection seam. It is consulted after the call has
	// been recorded, so a crash matrix can assert that the call was attempted
	// and still fail it. Return nil to let the call proceed.
	Fail func(call GitHubCall) error
	// Calls is every invocation in order, including the ones Fail failed.
	Calls []GitHubCall
}

var _ GitHubAdapter = (*FakeGitHubAdapter)(nil)

func NewFakeGitHubAdapter() *FakeGitHubAdapter {
	return &FakeGitHubAdapter{
		Issues:        map[int]GitHubIssue{},
		PullRequests:  map[int]GitHubPullRequest{},
		ChecksByHead:  map[string]GitHubCheckObservation{},
		ReviewsByHead: map[string]GitHubReviewObservation{},
		Refs:          map[string]string{},
		Comments:      map[int][]string{},
		NextNumber:    1,
	}
}

func (f *FakeGitHubAdapter) record(call GitHubCall) error {
	f.Calls = append(f.Calls, call)
	if f.Fail != nil {
		return f.Fail(call)
	}
	return nil
}

// Methods is the recorded call sequence, for asserting what was and was not
// invoked.
func (f *FakeGitHubAdapter) Methods() []string {
	names := make([]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		names = append(names, c.Method)
	}
	return names
}

// Merge and Close script the transitions the reconciler has to observe.
func (f *FakeGitHubAdapter) Merge(number int, at time.Time) {
	pr := f.PullRequests[number]
	pr.State, pr.Merged, pr.MergedAt = GitHubClosed, true, at
	f.PullRequests[number] = pr
}

func (f *FakeGitHubAdapter) Close(number int) {
	pr := f.PullRequests[number]
	pr.State, pr.Merged = GitHubClosed, false
	f.PullRequests[number] = pr
}

func (f *FakeGitHubAdapter) Issue(_ context.Context, repo GitHubRepo, number int) (GitHubIssue, error) {
	if err := f.record(GitHubCall{Method: "Issue", Repo: repo, Number: number}); err != nil {
		return GitHubIssue{}, err
	}
	issue, ok := f.Issues[number]
	if !ok {
		return GitHubIssue{}, fmt.Errorf("no issue %d in %s", number, repo)
	}
	return issue, nil
}

func (f *FakeGitHubAdapter) FindPullRequests(_ context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error) {
	if err := f.record(GitHubCall{Method: "FindPullRequests", Repo: repo, Ref: headRef, BaseRef: baseRef}); err != nil {
		return nil, err
	}
	var found []GitHubPullRequest
	for number := 1; number <= f.NextNumber; number++ {
		pr, ok := f.PullRequests[number]
		if ok && pr.HeadRef == headRef && pr.BaseRef == baseRef {
			found = append(found, pr)
		}
	}
	return found, nil
}

func (f *FakeGitHubAdapter) CreatePullRequest(_ context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error) {
	if err := f.record(GitHubCall{Method: "CreatePullRequest", Repo: repo, Ref: request.HeadRef, BaseRef: request.BaseRef, Body: request.Body.Body()}); err != nil {
		return GitHubPullRequest{}, err
	}
	if request.HeadRef == "" || request.BaseRef == "" || request.Title == "" {
		return GitHubPullRequest{}, fmt.Errorf("pull request requires a head, a base and a title")
	}
	if request.Body.Body() == "" {
		return GitHubPullRequest{}, fmt.Errorf("pull request body must be an explicitly cleared publication")
	}
	number := f.NextNumber
	f.NextNumber++
	pr := GitHubPullRequest{
		Number:  number,
		URL:     fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		HeadRef: request.HeadRef,
		HeadSHA: f.Refs[request.HeadRef],
		BaseRef: request.BaseRef,
		BaseSHA: f.Refs[request.BaseRef],
		State:   GitHubOpen,
	}
	f.PullRequests[number] = pr
	return pr, nil
}

func (f *FakeGitHubAdapter) UpdatePullRequest(_ context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error) {
	if err := f.record(GitHubCall{Method: "UpdatePullRequest", Repo: repo, Number: number, Body: update.Body.Body()}); err != nil {
		return GitHubPullRequest{}, err
	}
	pr, ok := f.PullRequests[number]
	if !ok {
		return GitHubPullRequest{}, fmt.Errorf("no pull request %d in %s", number, repo)
	}
	if update.State != "" {
		pr.State = update.State
	}
	f.PullRequests[number] = pr
	return pr, nil
}

func (f *FakeGitHubAdapter) PullRequest(_ context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	if err := f.record(GitHubCall{Method: "PullRequest", Repo: repo, Number: number}); err != nil {
		return GitHubPullRequest{}, err
	}
	pr, ok := f.PullRequests[number]
	if !ok {
		return GitHubPullRequest{}, fmt.Errorf("no pull request %d in %s", number, repo)
	}
	return pr, nil
}

func (f *FakeGitHubAdapter) Checks(_ context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error) {
	if err := f.record(GitHubCall{Method: "Checks", Repo: repo, SHA: headSHA}); err != nil {
		return GitHubCheckObservation{}, err
	}
	if headSHA == "" {
		return GitHubCheckObservation{}, fmt.Errorf("checks require an exact head sha")
	}
	observation := f.ChecksByHead[headSHA]
	observation.HeadSHA = headSHA
	if observation.State == "" {
		observation.State = GitHubCheckNone
	}
	return observation, nil
}

func (f *FakeGitHubAdapter) Reviews(_ context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error) {
	if err := f.record(GitHubCall{Method: "Reviews", Repo: repo, Number: number, SHA: headSHA}); err != nil {
		return GitHubReviewObservation{}, err
	}
	if headSHA == "" {
		return GitHubReviewObservation{}, fmt.Errorf("reviews require an exact head sha")
	}
	scripted := f.ReviewsByHead[headSHA]
	observation := GitHubReviewObservation{HeadSHA: headSHA}
	// Filtered rather than trusted: an observation may only contain material
	// belonging to the head it claims.
	for _, r := range scripted.Reviews {
		if r.CommitSHA == headSHA {
			observation.Reviews = append(observation.Reviews, r)
		}
	}
	for _, c := range scripted.Comments {
		if c.CommitSHA == headSHA {
			observation.Comments = append(observation.Comments, c)
		}
	}
	return observation, nil
}

func (f *FakeGitHubAdapter) CommentOnPullRequest(_ context.Context, repo GitHubRepo, number int, body Publication) error {
	if err := f.record(GitHubCall{Method: "CommentOnPullRequest", Repo: repo, Number: number, Body: body.Body()}); err != nil {
		return err
	}
	if body.Body() == "" {
		return fmt.Errorf("comment body must be an explicitly cleared publication")
	}
	if _, ok := f.PullRequests[number]; !ok {
		return fmt.Errorf("no pull request %d in %s", number, repo)
	}
	f.Comments[number] = append(f.Comments[number], body.Body())
	return nil
}

// RefSHA reports a ref not present in Refs as a genuine absence: Exists is
// false and the error is nil. Scripting an observation *failure* instead (as
// opposed to absence) is what Fail is for - the two are never conflated here.
func (f *FakeGitHubAdapter) RefSHA(_ context.Context, repo GitHubRepo, ref string) (RefObservation, error) {
	if err := f.record(GitHubCall{Method: "RefSHA", Repo: repo, Ref: ref}); err != nil {
		return RefObservation{}, err
	}
	sha, ok := f.Refs[ref]
	if !ok {
		return RefObservation{}, nil
	}
	return RefObservation{Exists: true, SHA: sha}, nil
}

// DiscoverIssues answers from Discoveries, or from Issues when nothing is
// scripted. Whatever the source, the result is put through the same two filters
// the real adapter applies, so this double structurally cannot hand back a pull
// request or an issue that does not carry the opt-in label - the invariants a
// test asserts against the fake are the invariants the real thing enforces.
func (f *FakeGitHubAdapter) DiscoverIssues(_ context.Context, query DiscoveryQuery) (DiscoveryResult, error) {
	label := query.Label
	if label == "" {
		label = DefaultDiscoveryLabel
	}
	if err := f.record(GitHubCall{Method: "DiscoverIssues", Repo: query.Repo, Label: label, ETag: query.ETag}); err != nil {
		return DiscoveryResult{}, err
	}
	step := f.nextDiscovery()
	step.Repo, step.Label = query.Repo, label
	if step.ETag != "" && step.ETag == query.ETag {
		// The caller already holds this answer. Reported as NotModified with no
		// issues, never as an empty set.
		return DiscoveryResult{
			Repo: query.Repo, Label: label, ETag: step.ETag,
			NotModified: true, Pages: step.Pages, RateLimit: step.RateLimit,
		}, nil
	}
	step.Issues = discoverable(step.Issues, label)
	if step.Pages == 0 && !step.NotModified {
		step.Pages = 1
	}
	return step, nil
}

// nextDiscovery returns the scripted step for this call, or the set derived
// from Issues when nothing is scripted.
func (f *FakeGitHubAdapter) nextDiscovery() DiscoveryResult {
	if len(f.Discoveries) == 0 {
		numbers := make([]int, 0, len(f.Issues))
		for number := range f.Issues {
			numbers = append(numbers, number)
		}
		sort.Ints(numbers)
		derived := DiscoveryResult{}
		for _, number := range numbers {
			derived.Issues = append(derived.Issues, f.Issues[number])
		}
		return derived
	}
	step := f.Discoveries[min(f.discovered, len(f.Discoveries)-1)]
	f.discovered++
	return step
}

// discoverable applies the two exclusions discovery owes its caller: a pull
// request is never a source, and an issue that does not carry the opt-in label
// was never opted in.
func discoverable(issues []GitHubIssue, label string) []GitHubIssue {
	var kept []GitHubIssue
	for _, issue := range issues {
		if strings.Contains(issue.URL, "/pull/") {
			continue
		}
		if !hasLabel(issue.Labels, label) {
			continue
		}
		kept = append(kept, issue)
	}
	return kept
}
