package runtime

// GitHubRESTAdapter is the one real GitHub.com adapter: structured REST calls
// over an injectable Doer, decoded into the normalized types in github.go.
//
// It does not shell out to `gh` and does not scrape anyone's output. `gh` has
// exactly one job in this design, and it is to answer the credential question
// (see GitHubCLICredential); the API surface is spoken directly so a response is
// a typed value rather than a parsed presentation.
//
// The credential is never a field here. It is resolved per call through the
// CredentialProvider seam, kept in a local variable, and applied as an
// Authorization header - never a query parameter, never a body member, never an
// error string.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type GitHubRESTAdapter struct {
	HTTP Doer
	// Endpoint is the API root. Empty uses the public GitHub.com API.
	Endpoint string
	// Credentials is the operator-authorized resolver. Nil is not "anonymous",
	// it is github_auth_required.
	Credentials CredentialProvider
}

var _ GitHubAdapter = GitHubRESTAdapter{}

func (a GitHubRESTAdapter) root() string {
	if a.Endpoint == "" {
		return "https://api.github.com"
	}
	return strings.TrimSuffix(a.Endpoint, "/")
}

// token resolves the credential for exactly this repository. Every failure is a
// typed GitHubAuthError; none of them is a panic and none of them is an empty
// token that would silently become an unauthenticated request.
func (a GitHubRESTAdapter) token(repo GitHubRepo) (string, error) {
	identity, err := repo.identity()
	if err != nil {
		return "", &GitHubAuthError{Detail: "repository is not a governed GitHub remote"}
	}
	if a.Credentials == nil {
		return "", &GitHubAuthError{Detail: "no operator-authorized credential provider is configured"}
	}
	_, secret, err := a.Credentials.Credential(identity)
	if err != nil {
		var authErr *GitHubAuthError
		if errors.As(err, &authErr) {
			return "", authErr
		}
		return "", &GitHubAuthError{Detail: "credential resolution failed"}
	}
	if strings.TrimSpace(secret) == "" {
		return "", &GitHubAuthError{Detail: "credential resolution produced an empty token"}
	}
	return secret, nil
}

// do performs one HTTP request and returns the raw status and body. It
// classifies nothing: call() applies the shared "every non-2xx is an error"
// rule that every operation except RefSHA wants, and RefSHA applies its own
// rule, where a 404 means the ref is genuinely absent rather than that the
// observation failed.
func (a GitHubRESTAdapter) do(ctx context.Context, repo GitHubRepo, method, path string, query url.Values, payload any) (int, []byte, error) {
	status, _, raw, err := a.doRaw(ctx, repo, method, path, query, payload, nil)
	return status, raw, err
}

// doRaw is do() plus the two things discovery needs and nothing else does: the
// ability to send conditional-request headers, and the response headers back,
// which is where pagination, ETag and rate-limit budget all live.
func (a GitHubRESTAdapter) doRaw(ctx context.Context, repo GitHubRepo, method, path string, query url.Values, payload any, extra http.Header) (int, http.Header, []byte, error) {
	if a.HTTP == nil {
		return 0, nil, nil, fmt.Errorf("github adapter has no HTTP transport")
	}
	secret, err := a.token(repo)
	if err != nil {
		return 0, nil, nil, err
	}
	target := a.root() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "zenchron-engineering")
	request.Header.Set("Authorization", "Bearer "+secret)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// Caller-supplied headers are applied last but cannot displace the
	// Authorization header, which is set from the resolved credential above and
	// is not a caller concern.
	for name, values := range extra {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := a.HTTP.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("github request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("github response unreadable")
	}
	return response.StatusCode, response.Header, raw, nil
}

// call performs one REST request and decodes into out (nil to discard).
func (a GitHubRESTAdapter) call(ctx context.Context, repo GitHubRepo, method, path string, query url.Values, payload, out any) error {
	status, raw, err := a.do(ctx, repo, method, path, query, payload)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &GitHubAuthError{Detail: "github rejected the credential with status " + strconv.Itoa(status)}
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("github %s %s returned status %d", method, path, status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s %s returned an unexpected payload", method, path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Path safety
// ---------------------------------------------------------------------------

// repoPath is the /repos/{owner}/{repo} prefix. The owner and name have already
// been shape-checked by GovernedRemote through repo.identity(); escaping them is
// the belt to that suspenders.
func repoPath(repo GitHubRepo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

// safeRef refuses anything that is not a plain ref name. Slashes are legal in a
// branch name and must stay unescaped in the path, so the character set is
// checked instead of being escaped away.
func safeRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.Contains(ref, "..") {
		return fmt.Errorf("refused ref %q", ref)
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/':
		default:
			return fmt.Errorf("refused ref %q", ref)
		}
	}
	return nil
}

func safeSHA(sha string) error {
	if len(sha) < 7 || len(sha) > 64 {
		return fmt.Errorf("refused head sha %q", sha)
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return fmt.Errorf("refused head sha %q", sha)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wire types. Only the fields the runtime relies on are modelled.
// ---------------------------------------------------------------------------

type ghActor struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

func (a *ghActor) normalize() GitHubActor {
	if a == nil {
		return GitHubActor{}
	}
	return GitHubActor{Login: a.Login, ID: a.ID}
}

type ghIssue struct {
	Number    int      `json:"number"`
	HTMLURL   string   `json:"html_url"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	UpdatedAt string   `json:"updated_at"`
	User      *ghActor `json:"user"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is GitHub's tell that this "issue" is really a pull request.
	// The issues endpoints report pull requests as issues, and only the presence
	// of this member distinguishes them. It is decoded raw because nothing in it
	// is used: its existence is the whole signal.
	PullRequest json.RawMessage `json:"pull_request"`
}

// isPullRequest reports whether GitHub returned a pull request under an issue
// shape. A pull request is never a source of work, so this is a hard exclusion
// everywhere an issue payload is read, not a hint.
func (i ghIssue) isPullRequest() bool {
	trimmed := strings.TrimSpace(string(i.PullRequest))
	return trimmed != "" && trimmed != "null"
}

func (i ghIssue) normalize() GitHubIssue {
	issue := GitHubIssue{
		Number: i.Number, URL: i.HTMLURL,
		Title: UntrustedText(i.Title), Body: UntrustedText(i.Body),
		State: normalizeState(i.State), Author: i.User.normalize(),
	}
	if at, ok := parseTime(i.UpdatedAt); ok {
		issue.UpdatedAt = at
	}
	for _, l := range i.Labels {
		issue.Labels = append(issue.Labels, UntrustedText(l.Name))
	}
	return issue
}

type ghPull struct {
	Number   int    `json:"number"`
	HTMLURL  string `json:"html_url"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergedAt string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (p ghPull) normalize() GitHubPullRequest {
	pr := GitHubPullRequest{
		Number: p.Number, URL: p.HTMLURL,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA,
		BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA,
		State: normalizeState(p.State), Merged: p.Merged,
	}
	if at, ok := parseTime(p.MergedAt); ok {
		// The list endpoint omits "merged"; merged_at is the authoritative
		// signal on every endpoint that reports one.
		pr.MergedAt, pr.Merged = at, true
	}
	return pr
}

func normalizeState(state string) GitHubState {
	if strings.EqualFold(state, "open") {
		return GitHubOpen
	}
	return GitHubClosed
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

func (a GitHubRESTAdapter) Issue(ctx context.Context, repo GitHubRepo, number int) (GitHubIssue, error) {
	if number <= 0 {
		return GitHubIssue{}, fmt.Errorf("issue number must be positive")
	}
	var wire ghIssue
	if err := a.call(ctx, repo, http.MethodGet, repoPath(repo)+"/issues/"+strconv.Itoa(number), nil, nil, &wire); err != nil {
		return GitHubIssue{}, err
	}
	// The issues endpoint answers a pull request number with the pull request.
	// A pull request is not a source issue, so this refuses rather than
	// normalizing it into one.
	if wire.isPullRequest() {
		return GitHubIssue{}, fmt.Errorf("refused %s#%d: it is a pull request, not a source issue", repo, number)
	}
	return wire.normalize(), nil
}

func (a GitHubRESTAdapter) FindPullRequests(ctx context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error) {
	if err := safeRef(headRef); err != nil {
		return nil, err
	}
	if err := safeRef(baseRef); err != nil {
		return nil, err
	}
	query := url.Values{
		"state":    {"all"},
		"head":     {repo.Owner + ":" + headRef},
		"base":     {baseRef},
		"per_page": {"100"},
	}
	var wire []ghPull
	if err := a.call(ctx, repo, http.MethodGet, repoPath(repo)+"/pulls", query, nil, &wire); err != nil {
		return nil, err
	}
	var found []GitHubPullRequest
	for _, p := range wire {
		// The server filter is a convenience; the binding is asserted here.
		if p.Head.Ref != headRef || p.Base.Ref != baseRef {
			continue
		}
		found = append(found, p.normalize())
	}
	return found, nil
}

func (a GitHubRESTAdapter) CreatePullRequest(ctx context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error) {
	if err := safeRef(request.HeadRef); err != nil {
		return GitHubPullRequest{}, err
	}
	if err := safeRef(request.BaseRef); err != nil {
		return GitHubPullRequest{}, err
	}
	if request.Title == "" {
		return GitHubPullRequest{}, fmt.Errorf("pull request requires a title")
	}
	if request.Body.Body() == "" {
		return GitHubPullRequest{}, fmt.Errorf("pull request body must be an explicitly cleared publication")
	}
	payload := map[string]string{
		"title": request.Title,
		"head":  request.HeadRef,
		"base":  request.BaseRef,
		"body":  request.Body.Body(),
	}
	var wire ghPull
	if err := a.call(ctx, repo, http.MethodPost, repoPath(repo)+"/pulls", nil, payload, &wire); err != nil {
		return GitHubPullRequest{}, err
	}
	return wire.normalize(), nil
}

func (a GitHubRESTAdapter) UpdatePullRequest(ctx context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error) {
	if number <= 0 {
		return GitHubPullRequest{}, fmt.Errorf("pull request number must be positive")
	}
	payload := map[string]string{}
	if update.Title != "" {
		payload["title"] = update.Title
	}
	if update.Body.Body() != "" {
		payload["body"] = update.Body.Body()
	}
	if update.State != "" {
		payload["state"] = string(update.State)
	}
	if len(payload) == 0 {
		return GitHubPullRequest{}, fmt.Errorf("pull request update names no change")
	}
	var wire ghPull
	if err := a.call(ctx, repo, http.MethodPatch, repoPath(repo)+"/pulls/"+strconv.Itoa(number), nil, payload, &wire); err != nil {
		return GitHubPullRequest{}, err
	}
	return wire.normalize(), nil
}

func (a GitHubRESTAdapter) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	if number <= 0 {
		return GitHubPullRequest{}, fmt.Errorf("pull request number must be positive")
	}
	var wire ghPull
	if err := a.call(ctx, repo, http.MethodGet, repoPath(repo)+"/pulls/"+strconv.Itoa(number), nil, nil, &wire); err != nil {
		return GitHubPullRequest{}, err
	}
	return wire.normalize(), nil
}

// Checks reads the check runs of exactly headSHA.
//
// ponytail: check runs only. Legacy commit statuses (/commits/{sha}/status) are
// a separate GitHub surface; add and merge them if an integration that reports
// only statuses ever has to gate a run.
func (a GitHubRESTAdapter) Checks(ctx context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error) {
	if err := safeSHA(headSHA); err != nil {
		return GitHubCheckObservation{}, err
	}
	var wire struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			DetailsURL string `json:"details_url"`
			Output     struct {
				Summary string `json:"summary"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	query := url.Values{"per_page": {"100"}}
	if err := a.call(ctx, repo, http.MethodGet, repoPath(repo)+"/commits/"+headSHA+"/check-runs", query, nil, &wire); err != nil {
		return GitHubCheckObservation{}, err
	}
	observation := GitHubCheckObservation{HeadSHA: headSHA, State: GitHubCheckNone}
	for _, run := range wire.CheckRuns {
		observation.Runs = append(observation.Runs, GitHubCheckRun{
			Name:       UntrustedText(run.Name),
			State:      normalizeCheck(run.Status, run.Conclusion),
			Summary:    UntrustedText(run.Output.Summary),
			DetailsURL: run.DetailsURL,
		})
	}
	observation.State = aggregateChecks(observation.Runs)
	return observation, nil
}

func normalizeCheck(status, conclusion string) GitHubCheckState {
	if !strings.EqualFold(status, "completed") {
		return GitHubCheckPending
	}
	switch strings.ToLower(conclusion) {
	case "success", "neutral", "skipped":
		return GitHubCheckSuccess
	default:
		return GitHubCheckFailure
	}
}

// aggregateChecks fails closed: any failure is a failure, anything unfinished is
// pending, and no checks at all is "none" rather than "success".
func aggregateChecks(runs []GitHubCheckRun) GitHubCheckState {
	state := GitHubCheckNone
	for _, run := range runs {
		switch run.State {
		case GitHubCheckFailure:
			return GitHubCheckFailure
		case GitHubCheckPending:
			state = GitHubCheckPending
		case GitHubCheckSuccess:
			if state != GitHubCheckPending {
				state = GitHubCheckSuccess
			}
		}
	}
	return state
}

// Reviews reads reviews and review comments and keeps only the ones GitHub
// attributes to exactly headSHA.
//
// ponytail: first page only (per_page=100). Paginate if a PR ever accumulates
// more than a hundred reviews or review comments on one head.
func (a GitHubRESTAdapter) Reviews(ctx context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error) {
	if number <= 0 {
		return GitHubReviewObservation{}, fmt.Errorf("pull request number must be positive")
	}
	if err := safeSHA(headSHA); err != nil {
		return GitHubReviewObservation{}, err
	}
	base := repoPath(repo) + "/pulls/" + strconv.Itoa(number)
	query := url.Values{"per_page": {"100"}}
	var reviews []struct {
		ID          int64    `json:"id"`
		User        *ghActor `json:"user"`
		State       string   `json:"state"`
		Body        string   `json:"body"`
		CommitID    string   `json:"commit_id"`
		SubmittedAt string   `json:"submitted_at"`
	}
	if err := a.call(ctx, repo, http.MethodGet, base+"/reviews", query, nil, &reviews); err != nil {
		return GitHubReviewObservation{}, err
	}
	var comments []struct {
		ID        int64    `json:"id"`
		User      *ghActor `json:"user"`
		Body      string   `json:"body"`
		Path      string   `json:"path"`
		CommitID  string   `json:"commit_id"`
		CreatedAt string   `json:"created_at"`
	}
	if err := a.call(ctx, repo, http.MethodGet, base+"/comments", query, nil, &comments); err != nil {
		return GitHubReviewObservation{}, err
	}
	observation := GitHubReviewObservation{HeadSHA: headSHA}
	for _, r := range reviews {
		if !strings.EqualFold(r.CommitID, headSHA) {
			continue
		}
		review := GitHubReview{
			ID: r.ID, Author: r.User.normalize(), State: normalizeReview(r.State),
			Body: UntrustedText(r.Body), CommitSHA: r.CommitID,
		}
		if at, ok := parseTime(r.SubmittedAt); ok {
			review.SubmittedAt = at
		}
		observation.Reviews = append(observation.Reviews, review)
	}
	for _, c := range comments {
		if !strings.EqualFold(c.CommitID, headSHA) {
			continue
		}
		comment := GitHubReviewComment{
			ID: c.ID, Author: c.User.normalize(), Body: UntrustedText(c.Body),
			Path: c.Path, CommitSHA: c.CommitID,
		}
		if at, ok := parseTime(c.CreatedAt); ok {
			comment.CreatedAt = at
		}
		observation.Comments = append(observation.Comments, comment)
	}
	return observation, nil
}

func normalizeReview(state string) GitHubReviewState {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return GitHubReviewApproved
	case "CHANGES_REQUESTED":
		return GitHubReviewChangesRequested
	case "DISMISSED":
		return GitHubReviewDismissed
	default:
		return GitHubReviewCommented
	}
}

func (a GitHubRESTAdapter) CommentOnPullRequest(ctx context.Context, repo GitHubRepo, number int, body Publication) error {
	if number <= 0 {
		return fmt.Errorf("pull request number must be positive")
	}
	if body.Body() == "" {
		return fmt.Errorf("comment body must be an explicitly cleared publication")
	}
	payload := map[string]string{"body": body.Body()}
	return a.call(ctx, repo, http.MethodPost, repoPath(repo)+"/issues/"+strconv.Itoa(number)+"/comments", nil, payload, nil)
}

// RefSHA classifies the response by STATUS CODE ONLY, never by message text:
// a 404 is the ref's genuine absence (RefObservation{}, nil error), a 401/403
// is a credential failure (*GitHubAuthError), and any other non-2xx or
// unparseable response is an observation failure (*GitHubAPIError). Only the
// 404 case is not an error.
//
// Endpoint choice matters here, and it is the whole reason this reads
// /git/ref/heads/<branch> rather than the more obvious /commits/<ref>. GitHub
// answers /commits/<ref> for an absent ref with 422, the same code it uses for
// a request it merely disliked, so absence and malformed-request are not
// separable by status alone on that endpoint - and this classifier is not
// permitted to fall back to reading message text. /git/ref answers 404 for a
// ref that is not there and nothing else does, which is exactly the
// distinction the caller needs. Every caller asks about a branch, so the
// heads/ namespace is hard-coded rather than guessed from the ref.
//
// The response is then checked for exactness in both directions: GitHub must
// have answered about refs/heads/<branch> itself and about a commit. A
// near-miss answer is refused rather than reported as the branch head.
func (a GitHubRESTAdapter) RefSHA(ctx context.Context, repo GitHubRepo, ref string) (RefObservation, error) {
	if err := safeRef(ref); err != nil {
		return RefObservation{}, err
	}
	path := repoPath(repo) + "/git/ref/heads/" + ref
	status, raw, err := a.do(ctx, repo, http.MethodGet, path, nil, nil)
	if err != nil {
		return RefObservation{}, err
	}
	switch {
	case status == http.StatusNotFound:
		return RefObservation{}, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return RefObservation{}, &GitHubAuthError{Detail: "github rejected the credential with status " + strconv.Itoa(status)}
	case status < 200 || status > 299:
		return RefObservation{}, &GitHubAPIError{Status: status, Detail: "github GET " + path + " returned status " + strconv.Itoa(status)}
	}
	var wire struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return RefObservation{}, &GitHubAPIError{Status: status, Detail: "github returned an unexpected payload for ref " + ref}
	}
	if wire.Ref != "refs/heads/"+ref {
		return RefObservation{}, &GitHubAPIError{Status: status, Detail: "github answered about a different ref than " + ref}
	}
	if wire.Object.Type != "commit" {
		return RefObservation{}, &GitHubAPIError{Status: status, Detail: "github reported a non-commit object for ref " + ref}
	}
	if wire.Object.SHA == "" {
		return RefObservation{}, &GitHubAPIError{Status: status, Detail: "github reported no sha for ref " + ref}
	}
	return RefObservation{Exists: true, SHA: wire.Object.SHA}, nil
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// maxDiscoveryPages bounds the walk. Reaching it is an error, never a silent
// truncation: an incomplete set that presents as complete is exactly the
// failure this whole path is written to avoid.
const maxDiscoveryPages = 50

// DiscoverIssues reads every opted-in issue in exactly one repository.
//
// Pagination vs. the conditional-request optimization
//
// Correctness outranks the optimization, and the two genuinely conflict. GitHub
// gives one ETag per page. A 304 on page 1 says only that page 1 is unchanged;
// an issue on page 3 that lost the opt-in label leaves page 1 byte-identical, so
// honouring that 304 would report a set that still contains work nobody opted
// into any more.
//
// The rule here, chosen over per-page cursor bookkeeping because it cannot be
// held wrong: a cursor is handed back ONLY when the whole opted-in set fit on a
// single page. A multi-page observation returns an empty ETag, so the caller has
// nothing to replay and the next discovery is unconditional and complete. The
// optimization is disabled exactly where it would be unsafe, and it is disabled
// by construction rather than by a flag someone has to remember to check.
//
// A conditional request is therefore only ever sent for page 1, and a 304 is
// only ever accepted there. It is reported as NotModified with no issues -
// never as an empty set, which a caller would rightly read as "every issue was
// opted out".
func (a GitHubRESTAdapter) DiscoverIssues(ctx context.Context, query DiscoveryQuery) (DiscoveryResult, error) {
	label := query.Label
	if label == "" {
		label = DefaultDiscoveryLabel
	}
	if strings.TrimSpace(label) == "" {
		return DiscoveryResult{}, fmt.Errorf("discovery requires an opt-in label")
	}
	if _, err := query.Repo.identity(); err != nil {
		return DiscoveryResult{}, err
	}
	path := repoPath(query.Repo) + "/issues"
	result := DiscoveryResult{Repo: query.Repo, Label: label}
	for page := 1; page <= maxDiscoveryPages; page++ {
		values := url.Values{
			"labels":    {label},
			"state":     {"open"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {"100"},
			"page":      {strconv.Itoa(page)},
		}
		var extra http.Header
		if page == 1 && query.ETag != "" {
			extra = http.Header{"If-None-Match": {query.ETag}}
		}
		status, header, raw, err := a.doRaw(ctx, query.Repo, http.MethodGet, path, values, nil, extra)
		if err != nil {
			return DiscoveryResult{}, err
		}
		rate, reported := observeRateLimit(header, status)
		result.RateLimit = rate
		if status == http.StatusNotModified {
			if page != 1 || extra == nil {
				// Only page 1 is ever conditional, so a 304 anywhere else is the
				// forge answering a question that was not asked. Reporting it as
				// "unchanged" would invent a completeness claim.
				return DiscoveryResult{}, &GitHubAPIError{Status: status, Detail: "github answered an unconditional discovery page with 304"}
			}
			result.NotModified, result.ETag, result.Pages = true, query.ETag, 1
			return result, nil
		}
		if err := classifyGitHubStatus(status, rate, reported, "github issue discovery"); err != nil {
			return DiscoveryResult{}, err
		}
		var wire []ghIssue
		if err := json.Unmarshal(raw, &wire); err != nil {
			return DiscoveryResult{}, &GitHubAPIError{Status: status, Detail: "github returned an unexpected payload for issue discovery"}
		}
		for _, entry := range wire {
			// GitHub's issues API returns pull requests as issues. They are not
			// sources of work and are excluded here, before anything downstream
			// can see one.
			if entry.isPullRequest() {
				continue
			}
			issue := entry.normalize()
			// The server-side label filter is a convenience; opt-in is asserted
			// against the labels the response actually carries.
			if !hasLabel(issue.Labels, label) {
				continue
			}
			result.Issues = append(result.Issues, issue)
		}
		result.Pages = page
		if page == 1 {
			result.ETag = header.Get("ETag")
		}
		if !hasNextPage(header.Get("Link")) {
			if result.Pages > 1 {
				// See the pagination note above: a multi-page view yields no
				// cursor, so it can never be replayed as a partial one.
				result.ETag = ""
			}
			return result, nil
		}
	}
	return DiscoveryResult{}, &GitHubAPIError{
		Status: 0,
		Detail: fmt.Sprintf("issue discovery in %s exceeded %d pages; refusing to report a truncated set", query.Repo, maxDiscoveryPages),
	}
}

// hasNextPage reports whether a Link header advertises a further page.
//
// The URL it names is deliberately not followed. The next page is requested by
// incrementing this adapter's own page parameter, so a forge-supplied URL never
// decides what gets fetched or from where.
func hasNextPage(link string) bool {
	for _, part := range strings.Split(link, ",") {
		for _, attribute := range strings.Split(part, ";") {
			attribute = strings.TrimSpace(attribute)
			if attribute == `rel="next"` || attribute == "rel=next" {
				return true
			}
		}
	}
	return false
}

// observeRateLimit normalizes what the response said about the budget. The
// second return reports whether the forge stated a remaining count at all, so a
// caller can tell "0 left" from "did not say" without a sentinel.
//
// Every instant here is read out of a header. Nothing consults the local clock,
// so the observation stays meaningful when it is persisted and read back later.
func observeRateLimit(header http.Header, status int) (RateLimitObservation, bool) {
	observation := RateLimitObservation{}
	if header == nil {
		return observation, false
	}
	reported := false
	if value := header.Get("X-RateLimit-Remaining"); value != "" {
		if remaining, err := strconv.Atoi(value); err == nil {
			observation.Remaining, reported = remaining, true
		}
	}
	if value := header.Get("X-RateLimit-Reset"); value != "" {
		if reset, err := strconv.ParseInt(value, 10, 64); err == nil {
			observation.ResetAt = time.Unix(reset, 0).UTC()
		}
	}
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			observation.RetryAfter = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(value); err == nil {
			// An HTTP-date is an absolute instant, so it can be recorded as a
			// reset time without turning it into a duration against a clock
			// this package is not allowed to read.
			observation.ResetAt = at.UTC()
		}
	}
	// GitHub reports primary exhaustion as a refusal with remaining 0 and no
	// Retry-After. A refusal that carries Retry-After, or one that arrives while
	// the primary budget still has room, is the secondary (abuse) limit.
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		observation.Secondary = observation.RetryAfter > 0 || (reported && observation.Remaining > 0)
	}
	return observation, reported
}

// classifyGitHubStatus splits one response status into the three outcomes a
// watcher routes differently: nil for success, *GitHubTransientError for
// anything that clears on its own, *GitHubAuthError for a credential the
// operator has to fix, and *GitHubAPIError for everything else.
//
// The split is made from the status and the rate-limit headers only. No message
// text is inspected, because forge-authored text is data and must not steer a
// control-flow decision.
func classifyGitHubStatus(status int, rate RateLimitObservation, reported bool, what string) error {
	detail := fmt.Sprintf("%s returned status %d", what, status)
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == http.StatusTooManyRequests, status >= 500:
		return &GitHubTransientError{Status: status, Detail: detail, RateLimit: rate}
	case status == http.StatusForbidden && (rate.Secondary || (reported && rate.Remaining == 0)):
		// A 403 is GitHub's rate-limit refusal as well as its permission
		// refusal. The budget headers are what tell them apart.
		return &GitHubTransientError{Status: status, Detail: detail, RateLimit: rate}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return &GitHubAuthError{Detail: "github rejected the credential with status " + strconv.Itoa(status)}
	default:
		return &GitHubAPIError{Status: status, Detail: detail}
	}
}
