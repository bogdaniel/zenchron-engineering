package runtime

// GitHubAdapter is the control-plane seam to the forge that holds the source of
// record: the issue a run answers, the pull request it publishes, and the
// checks and reviews that come back.
//
// Three properties are structural, not conventional:
//
//   - Every call names its exact repository, its exact pull request, or its
//     exact head SHA. There is no ambient "current repository" and no "latest"
//     anything, because the reconciler must be able to discard an observation
//     that belongs to a head it has already moved past.
//   - Every observation carries the head SHA it describes, for the same reason.
//   - Everything GitHub authored is UntrustedText. It is data. It is never an
//     instruction, and no field here exists to carry one.
//
// The credential is not a field of any adapter. It is resolved through the
// CredentialProvider seam that repository_git.go already defines, so exactly one
// operator-authorized resolution drives both the REST calls here and the
// runtime-owned askpass path that `git push` uses.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Normalized values
// ---------------------------------------------------------------------------

// UntrustedText is text authored outside the runtime: an issue body, a label, a
// review comment, a check summary. It is DATA. Nothing may execute it, obey it,
// or feed it to a policy decision as anything other than an observed string.
type UntrustedText string

// GitHubRepo is an exact repository.
type GitHubRepo struct{ Owner, Name string }

func (r GitHubRepo) String() string   { return r.Owner + "/" + r.Name }
func (r GitHubRepo) CloneURL() string { return "https://github.com/" + r.Owner + "/" + r.Name }

// identity reuses the governed-remote classifier, so an adapter cannot be
// pointed at another host, a userinfo URL, or a path that is not owner/name.
func (r GitHubRepo) identity() (RemoteIdentity, error) { return GovernedRemote(r.CloneURL()) }

// GitHubState is the open/closed state of an issue or pull request.
type GitHubState string

const (
	GitHubOpen   GitHubState = "open"
	GitHubClosed GitHubState = "closed"
)

// GitHubActor is a forge identity. It is an identity, not a capability: nothing
// here grants authority by itself.
type GitHubActor struct {
	Login string
	ID    int64
}

// GitHubIssue is the normalized source issue.
type GitHubIssue struct {
	Number    int
	URL       string
	Title     UntrustedText
	Body      UntrustedText
	Labels    []UntrustedText
	State     GitHubState
	UpdatedAt time.Time
	// Author is the identity that opened the issue - the initiating operator
	// where GitHub reports one, zero where it does not.
	Author GitHubActor
}

// GitHubPullRequest is the normalized bound pull request.
type GitHubPullRequest struct {
	Number   int
	URL      string
	HeadRef  string
	HeadSHA  string
	BaseRef  string
	BaseSHA  string
	State    GitHubState
	Merged   bool
	MergedAt time.Time
}

// GitHubPullRequestCreate is a creation request. Body is a Publication, so a
// raw or local-only artifact cannot reach the forge through it.
type GitHubPullRequestCreate struct {
	HeadRef string
	BaseRef string
	Title   string
	Body    Publication
}

// GitHubPullRequestUpdate is an update request. A zero field means "leave this
// unchanged", so an update never has to restate what it is not changing.
type GitHubPullRequestUpdate struct {
	Title string
	Body  Publication
	State GitHubState
}

// GitHubCheckState is the normalized CI conclusion.
type GitHubCheckState string

const (
	GitHubCheckNone    GitHubCheckState = "none"
	GitHubCheckPending GitHubCheckState = "pending"
	GitHubCheckSuccess GitHubCheckState = "success"
	GitHubCheckFailure GitHubCheckState = "failure"
)

// GitHubCheckRun is one check. Name and Summary are forge-authored data.
type GitHubCheckRun struct {
	Name       UntrustedText
	State      GitHubCheckState
	Summary    UntrustedText
	DetailsURL string
}

// GitHubCheckObservation is CI state for exactly one head SHA.
type GitHubCheckObservation struct {
	HeadSHA string
	State   GitHubCheckState
	Runs    []GitHubCheckRun
}

// GitHubReviewState is the normalized review verdict.
type GitHubReviewState string

const (
	GitHubReviewApproved         GitHubReviewState = "approved"
	GitHubReviewChangesRequested GitHubReviewState = "changes_requested"
	GitHubReviewCommented        GitHubReviewState = "commented"
	GitHubReviewDismissed        GitHubReviewState = "dismissed"
)

// GitHubReview is one submitted review of one exact commit.
type GitHubReview struct {
	ID          int64
	Author      GitHubActor
	State       GitHubReviewState
	Body        UntrustedText
	CommitSHA   string
	SubmittedAt time.Time
}

// GitHubReviewComment is one inline comment on one exact commit.
type GitHubReviewComment struct {
	ID        int64
	Author    GitHubActor
	Body      UntrustedText
	Path      string
	CommitSHA string
	CreatedAt time.Time
}

// GitHubReviewObservation is review state for exactly one head SHA. Entries
// belonging to any other commit are excluded, not merely labelled.
type GitHubReviewObservation struct {
	HeadSHA  string
	Reviews  []GitHubReview
	Comments []GitHubReviewComment
}

// RefObservation is the typed answer to "what is at this remote ref right
// now". It keeps two outcomes explicit that a bare (string, error) return
// used to conflate:
//
//   - the ref genuinely does not exist: Exists is false, SHA is empty, and
//     the error is nil. This is a normal, expected observation, not a
//     failure - a run-owned branch that has not been pushed yet looks exactly
//     like this.
//   - the observation itself failed (auth, network, rate limit, a 5xx): the
//     error is non-nil and RefObservation is the zero value. A caller must
//     never treat this as absence; absence is only ever the nil-error case.
//
// The ref exists case is Exists true with the exact SHA.
type RefObservation struct {
	Exists bool
	SHA    string
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// DefaultDiscoveryLabel is the opt-in marker. A repository is not watched
// wholesale: an issue is a candidate source only because an operator put this
// label on it, so the absence of the label is the absence of consent.
const DefaultDiscoveryLabel = "zenchron:auto"

// DiscoveryQuery names exactly one repository and exactly one opt-in label.
// There is no "all my repositories" form, for the same reason no other call in
// this file has one.
type DiscoveryQuery struct {
	Repo GitHubRepo
	// Label is the opt-in marker. Empty means DefaultDiscoveryLabel.
	Label string
	// ETag is a cursor a previous DiscoveryResult handed back, replayed as a
	// conditional request. Empty means unconditional. A DiscoveryResult only
	// ever hands back a cursor when the whole opted-in set fit on one page, so
	// a cursor can never stand for a partially observed multi-page view.
	ETag string
}

// DiscoveryResult is one complete observation of the opted-in set.
//
// Complete is the load-bearing word. Issues is either every opted-in issue in
// the repository or the call returned an error; it is never "the first page,
// probably". The one shape that is not a complete set is NotModified, which is
// explicitly flagged rather than expressed as an empty Issues slice, because
// "nothing changed" and "the label was removed from everything" are opposite
// instructions to a watcher and must never share a representation.
type DiscoveryResult struct {
	Repo GitHubRepo
	// Label is the effective opt-in label, after defaulting.
	Label string
	// Issues carries the normalized opted-in issues. Every entry has been
	// checked to actually carry Label - the server-side filter is treated as a
	// convenience, not as the authority - and pull requests are excluded.
	Issues []GitHubIssue
	// ETag is the cursor for the next conditional discovery, or empty when
	// there is none. See the pagination note on DiscoverIssues: a multi-page
	// observation deliberately yields no cursor.
	ETag string
	// NotModified reports that the forge answered the conditional request with
	// 304. Issues is empty and carries no meaning in that case; the caller must
	// keep whatever set it already holds.
	NotModified bool
	// Pages is how many pages were read. It is the evidence that a single-page
	// assumption was not silently made.
	Pages int
	// RateLimit is the budget the forge reported on the last response, whether
	// the call succeeded or not.
	RateLimit RateLimitObservation
}

// RateLimitObservation is what the forge said about the remaining budget. It is
// an observation, not a policy: nothing in this package sleeps on it, waits on
// it, or retries. The watcher owns scheduling and owns the clock.
//
// Every timestamp here comes out of a response header. None of it is derived
// from the local clock, so an observation can be persisted and reasoned about
// later without having silently baked in "when we happened to parse it".
//
// The zero value reads as "no budget reported", which a caller must treat as
// the conservative case rather than as an unlimited budget: Remaining is 0.
type RateLimitObservation struct {
	// Remaining is X-RateLimit-Remaining.
	Remaining int
	// ResetAt is X-RateLimit-Reset, or the absolute instant of a Retry-After
	// given as an HTTP-date. Zero means the forge reported none.
	ResetAt time.Time
	// RetryAfter is a Retry-After given in seconds. Zero means none was given.
	RetryAfter time.Duration
	// Secondary marks the secondary (abuse) rate limit, which is a different
	// budget from the primary hourly one and clears on its own schedule.
	Secondary bool
}

// GitHubTransientError is the typed outcome of an observation that failed for a
// reason expected to clear by itself: a 5xx, a 429, or a rate limit refusal.
//
// It is deliberately neither GitHubAuthError nor GitHubAPIError. A caller routes
// the three differently: a credential failure needs an operator, an API error
// needs a human to look at it, and this one needs nothing but bounded backoff.
// The RateLimit field carries what the forge said about when to come back;
// deciding when is the caller's job, not this package's.
type GitHubTransientError struct {
	Status    int
	Detail    string
	RateLimit RateLimitObservation
}

func (e *GitHubTransientError) Error() string {
	return fmt.Sprintf("github_transient_error: %s (status %d)", e.Detail, e.Status)
}

// hasLabel reports whether an observed label set carries name. GitHub matches
// label names case-insensitively, so opt-in does too - an operator who typed
// "Zenchron:Auto" opted in.
func hasLabel(labels []UntrustedText, name string) bool {
	for _, label := range labels {
		if strings.EqualFold(string(label), name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Interface
// ---------------------------------------------------------------------------

type GitHubAdapter interface {
	// Issue reads the exact source issue.
	Issue(ctx context.Context, repo GitHubRepo, number int) (GitHubIssue, error)
	// DiscoverIssues reads every issue in exactly this repository that carries
	// exactly this opt-in label. It is the only call that is not already bound
	// to a known issue or pull request, so it is the one place the runtime
	// learns of new work - and the label is the operator's consent to it.
	//
	// The contract the watcher depends on: a nil error means the result is a
	// COMPLETE view of the opted-in set, or NotModified. A partial page is
	// never returned as a success. Pull requests are excluded: GitHub's issues
	// endpoint reports them as issues, and a pull request is never a source.
	DiscoverIssues(ctx context.Context, query DiscoveryQuery) (DiscoveryResult, error)
	// FindPullRequests returns the pull requests bound to exactly this head
	// branch and base in exactly this repository, open or closed. This is the
	// crash-recovery discovery path: after an interrupted run the controller
	// must find out whether it already created the PR.
	FindPullRequests(ctx context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error)
	CreatePullRequest(ctx context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error)
	UpdatePullRequest(ctx context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error)
	// PullRequest reads the current state of the exact bound pull request.
	PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error)
	// Checks reads CI state for exactly headSHA, never "the latest".
	Checks(ctx context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error)
	// Reviews reads reviews and review comments for exactly headSHA.
	Reviews(ctx context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error)
	CommentOnPullRequest(ctx context.Context, repo GitHubRepo, number int, body Publication) error
	// RefSHA resolves a remote ref. This is the push crash-reconciliation
	// question: did the push that was interrupted land? A genuinely absent ref
	// is reported as RefObservation{Exists: false} with a nil error; an error
	// means the observation itself failed and nothing about existence may be
	// inferred from it.
	RefSHA(ctx context.Context, repo GitHubRepo, ref string) (RefObservation, error)
}

// ---------------------------------------------------------------------------
// Publication safety
// ---------------------------------------------------------------------------

// Publication is text that has passed the publication gate. Nothing outside
// NewPublication can produce a non-empty one, so an adapter method that takes a
// Publication cannot be handed unvetted output: the refusal happens before the
// adapter is reached, not inside it.
//
// The zero Publication is the empty body, which an update reads as "unchanged"
// and a create or comment refuses.
type Publication struct{ body string }

func (p Publication) Body() string { return p.body }

// NewPublication clears one excerpt for publication together with the artifacts
// it references. The artifact rule is ValidateArtifact's, not a second one:
// raw output must stay local-only and only sanitized output may be publishable.
// This adds the remaining requirement that a *published* artifact must have been
// explicitly marked publishable by a review - being sanitized is not enough.
func NewPublication(excerpt string, artifacts ...Artifact) (Publication, error) {
	if strings.TrimSpace(excerpt) == "" {
		return Publication{}, fmt.Errorf("publication requires an explicitly sanitized excerpt")
	}
	// Backstop, not the primary control: the same matcher redactTranscript uses.
	// The excerpt is supposed to be sanitized already; if it still carries a
	// credential shape, publishing it would be the leak.
	if transcriptSecrets.MatchString(excerpt) {
		return Publication{}, fmt.Errorf("refused publication: excerpt contains credential-shaped material")
	}
	for _, a := range artifacts {
		if err := ValidateArtifact(a); err != nil {
			return Publication{}, fmt.Errorf("refused publication: %w", err)
		}
		if a.LocalOnly || !a.Sanitized || !a.Publishable {
			return Publication{}, fmt.Errorf("refused publication of artifact %q: not marked publishable", a.Path)
		}
	}
	return Publication{body: excerpt}, nil
}

// ---------------------------------------------------------------------------
// Credential boundary
// ---------------------------------------------------------------------------

// GitHubAuthError is the typed outcome of a credential that cannot be resolved.
// Resolution never panics and never yields a silent empty token: every failure
// path returns one of these.
type GitHubAuthError struct{ Detail string }

func (e *GitHubAuthError) Error() string { return "github_auth_required: " + e.Detail }

// GitHubAPIError is the typed outcome of an observation that failed for a
// reason that is not a credential problem: a 5xx, an unexpected status, or an
// unreadable/unparseable response. It exists so a caller like the push
// crash-reconciler can tell "the observation failed, status is unknown" apart
// from both GitHubAuthError and a genuine absence (which is never an error at
// all - see RefObservation).
type GitHubAPIError struct {
	Status int
	Detail string
}

func (e *GitHubAPIError) Error() string {
	return fmt.Sprintf("github_api_error: %s (status %d)", e.Detail, e.Status)
}

// gitHubCredentialUser is the username half of a token credential. GitHub
// ignores it for token auth, and it is not a secret.
const gitHubCredentialUser = "x-access-token"

// GitHubCLICredential resolves the control-plane credential from the operator's
// already-authenticated local `gh` installation.
//
// It satisfies CredentialProvider, which is the whole point: the same value
// authorizes the REST calls in github_rest.go and the `git push` askpass path in
// repository_git.go, and there is exactly one place it is obtained.
//
// The boundary:
//   - Control plane only. This runs in the controller process. It is never
//     reachable from a candidate, a provider, or a brokered tool.
//   - The binary is resolved from trustedPATH exactly as gitBinary() resolves
//     git, so the candidate cannot substitute a program named `gh`.
//   - The arguments are runtime literals.
//   - The child environment is built from scratch, so no repository or
//     candidate-influenced variable reaches `gh`. HOME is the controller's own,
//     which is where the operator's `gh` login lives; nothing under a candidate
//     workspace can supply or override it.
//   - The secret is returned to the caller's local scope. It is never a struct
//     field, never in argv, never in the candidate/provider/verifier
//     environment, never in repository config, runtime.db, a canonical event
//     payload, an artifact, a log line, or a PR body.
type GitHubCLICredential struct{}

func (GitHubCLICredential) Credential(identity RemoteIdentity) (string, string, error) {
	if identity.URL == "" || identity.Transport() != "https" {
		return "", "", &GitHubAuthError{Detail: "credential is only issued to the governed https remote"}
	}
	binary, err := ghBinary()
	if err != nil {
		return "", "", &GitHubAuthError{Detail: err.Error()}
	}
	command := exec.Command(binary, "auth", "token")
	command.Env = []string{
		"PATH=" + trustedPATH,
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
		"NO_COLOR=1",
	}
	command.Stdin = nil
	out, err := command.Output()
	if err != nil {
		// The child's stderr is deliberately dropped: it is a diagnostic from an
		// authentication tool and is not worth risking in an error string.
		return "", "", &GitHubAuthError{Detail: "the local GitHub CLI is not authenticated"}
	}
	secret := strings.TrimSpace(string(out))
	if secret == "" {
		return "", "", &GitHubAuthError{Detail: "the local GitHub CLI returned no token"}
	}
	return gitHubCredentialUser, secret, nil
}

// ghBinary mirrors gitBinary(): resolve from the runtime's constant search path
// rather than the host PATH, so the program that runs is chosen by the runtime.
func ghBinary() (string, error) {
	for _, dir := range filepath.SplitList(trustedPATH) {
		p := filepath.Join(dir, "gh")
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("trusted GitHub CLI not found on the runtime search path")
}

// GitHubRemotePolicy binds a repository to the credential seam for `git push`,
// so the push path and the REST path are authorized by the same provider and
// the askpass implementation in repository_git.go is not duplicated.
func GitHubRemotePolicy(repo GitHubRepo, credentials CredentialProvider) (*RemotePolicy, error) {
	identity, err := repo.identity()
	if err != nil {
		return nil, err
	}
	return &RemotePolicy{Identity: identity, Credentials: credentials}, nil
}
