package runtime

// The runtime refuses feedback it authored itself, by identity. When it
// publishes with the operator's own credential it IS the operator on GitHub, so
// that guard refuses the operator's reviews too and the #63 review loop cannot
// happen. These tests pin the separation that fixes it without weakening the
// guard.

import (
	"context"
	"crypto"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publicationTokenFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "publication.token")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// governedRemote is built through the real parser, because RemoteIdentity's
// transport is unexported: a hand-built literal has no transport and every
// credential refuses it, which would make these tests pass for the wrong reason.
func governedRemoteIdentity(t *testing.T) RemoteIdentity {
	t.Helper()
	identity, err := GovernedRemote("https://github.com/acme/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// TestAPublicationTokenIsOwnerOnly. A token another local account can read is a
// publication identity that account also has: it can open pull requests, and
// its comments would then be indistinguishable from the runtime's own.
func TestAPublicationTokenIsOwnerOnly(t *testing.T) {
	readable := publicationTokenFile(t, "ghs_fixture_token_9c3", 0o644)
	_, _, err := GitHubTokenFileCredential{Path: readable}.Credential(governedRemoteIdentity(t))
	if err == nil {
		t.Fatal("a world-readable publication token was accepted")
	}
	if !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("the refusal does not name the problem: %v", err)
	}

	ownerOnly := publicationTokenFile(t, "ghs_fixture_token_9c3\n", 0o600)
	user, secret, err := GitHubTokenFileCredential{Path: ownerOnly}.Credential(governedRemoteIdentity(t))
	if err != nil {
		t.Fatalf("an owner-only publication token was refused: %v", err)
	}
	if user != gitHubCredentialUser || secret != "ghs_fixture_token_9c3" {
		t.Fatalf("credential = %q/%q", user, secret)
	}
}

// TestAPublicationTokenIsOnlyIssuedToTheGovernedRemote mirrors the CLI
// credential's boundary: the secret is for the repository the runtime governs,
// not for whatever remote a caller names.
func TestAPublicationTokenIsOnlyIssuedToTheGovernedRemote(t *testing.T) {
	path := publicationTokenFile(t, "ghs_fixture_token_9c3", 0o600)
	for _, identity := range []RemoteIdentity{{}, {URL: "git@github.com:acme/repo.git"}} {
		if _, _, err := (GitHubTokenFileCredential{Path: path}).Credential(identity); err == nil {
			t.Fatalf("a credential was issued to %q", identity.URL)
		}
	}
}

// TestAnEmptyPublicationTokenIsRefused. An empty file is a misconfiguration, and
// authenticating with the empty string would fail later as an opaque 401 rather
// than here as an operator problem.
func TestAnEmptyPublicationTokenIsRefused(t *testing.T) {
	for _, contents := range []string{"", "   \n\t "} {
		path := publicationTokenFile(t, contents, 0o600)
		if _, _, err := (GitHubTokenFileCredential{Path: path}).Credential(governedRemoteIdentity(t)); err == nil {
			t.Fatalf("an empty publication token was accepted: %q", contents)
		}
	}
}

// TestDoctorStatesWhetherTheOperatorCanGiveFeedback is the check that would have
// saved a live run. The collision is not a fault the runtime can repair, but it
// is one it can NAME before an operator spends a subscription discovering that
// their review was refused as self-authored.
func TestDoctorStatesWhetherTheOperatorCanGiveFeedback(t *testing.T) {
	shared := doctorPublicationIdentity(context.Background(), DoctorInput{GitHubCredentialMode: GitHubCredentialCLI})
	if shared.Status != DoctorWarn {
		t.Fatalf("a shared publication identity reported %s", shared.Status)
	}
	for _, phrase := range []string{"acts as YOU", "will not reach a worker", GitHubCredentialToken} {
		if !strings.Contains(shared.Reason, phrase) {
			t.Fatalf("the diagnosis does not state %q: %s", phrase, shared.Reason)
		}
	}

	// A separate CREDENTIAL is not a separate ACCOUNT: a personal access token
	// for the operator's own login lives in that file just as happily. Doctor
	// must report the account it resolved and let the operator compare, not
	// infer distinctness from the mode.
	unresolvable := doctorPublicationIdentity(context.Background(), DoctorInput{GitHubCredentialMode: GitHubCredentialToken})
	if unresolvable.Status != DoctorWarn {
		t.Fatalf("an unresolvable publication account reported %s: %s", unresolvable.Status, unresolvable.Reason)
	}
	if strings.Contains(unresolvable.Reason, "DIFFERENT") {
		t.Fatalf("doctor claimed distinctness it did not resolve: %s", unresolvable.Reason)
	}

	forge := NewFakeGitHubAdapter()
	forge.ViewerActor = GitHubActor{Login: "zenchron-runtime", ID: 4242}
	forge.Permissions["bogdaniel"] = PermissionAdmin
	human := NewFakeGitHubAdapter()
	human.ViewerActor = GitHubActor{Login: "bogdaniel", ID: 7}
	resolved := doctorPublicationIdentity(context.Background(), DoctorInput{
		GitHubCredentialMode: GitHubCredentialToken,
		GitHub:               forge,
		OperatorGitHub:       human,
		Repository:           RepositoryTarget{Identity: "acme/repo"},
	})
	if resolved.Status != DoctorPass {
		t.Fatalf("a resolvable publication account reported %s: %s", resolved.Status, resolved.Reason)
	}
	if !strings.Contains(resolved.Reason, "zenchron-runtime") {
		t.Fatalf("doctor did not name the account it resolved: %s", resolved.Reason)
	}
}

// TestDoctorClaimsNoProtectedExecutionPolicy. There is no policy vocabulary for
// requiring protected execution, so doctor must not describe one. This was the
// last copy of a claim e09b170 removed from the durable documentation.
func TestDoctorClaimsNoProtectedExecutionPolicy(t *testing.T) {
	check := doctorProviderIsolation(DoctorInput{
		Provider: CLIAgentProvider{Agent: ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}},
		Agents:   []AgentStatus{{ID: "codex", TrustMode: TrustOperatorTrusted, Default: true}},
	})
	if strings.Contains(check.Reason, "policy requires protected execution") {
		t.Fatalf("doctor still describes a policy rule that does not exist: %s", check.Reason)
	}
	if !strings.Contains(check.Reason, "UNPROVEN") {
		t.Fatalf("doctor stopped stating the residual risk: %s", check.Reason)
	}
}

// TestAnUnresolvedPublicationIdentityAdmitsNothing is the fail-closed law for
// the self-loop guard.
//
// The dangerous case is not a bot and not a stranger: it is the runtime's OWN
// publisher, a dedicated non-bot account that is a collaborator on the
// repository it publishes to. It passes the permission threshold, it is not
// automation, and it is not in SelfLogins when the viewer lookup failed - so
// every other check waves it through and the system feeds itself.
//
// Narrowing what the runtime knows about itself is not a safe fallback. It
// removes the ONLY fact that lets it recognize itself.
func TestAnUnresolvedPublicationIdentityAdmitsNothing(t *testing.T) {
	publisher := GitHubActor{Login: "zenchron-runtime", ID: 424242} // a real account, not a bot
	own := FeedbackItem{
		Class: FeedbackPullRequestComment, ID: 1, Actor: publisher,
		Body: "the runtime's own status comment", Commit: "head-sha",
	}
	// The publisher holds write on the repository it publishes to, which is
	// exactly why permission cannot be the thing that stops this.
	permissions := map[string]GitHubPermission{"zenchron-runtime": PermissionWrite}

	unresolved := FeedbackPolicy{MinPermission: PermissionWrite}
	for _, decision := range AdmitFeedback([]FeedbackItem{own}, unresolved, permissions, "head-sha") {
		if decision.Admitted {
			t.Fatalf("the runtime's own comment was admitted while its identity was unresolved: %+v", decision)
		}
		if decision.Reason != feedbackRefusedUnidentifiedRuntime {
			t.Fatalf("refused for the wrong reason: %q", decision.Reason)
		}
	}

	// Resolved, and knowing itself: still refused, now BY identity.
	resolved := FeedbackPolicy{
		MinPermission: PermissionWrite, SelfLogins: []string{"zenchron-runtime"},
		PublicationIdentityResolved: true,
	}
	for _, decision := range AdmitFeedback([]FeedbackItem{own}, resolved, permissions, "head-sha") {
		if decision.Admitted || decision.Reason != feedbackRefusedSelf {
			t.Fatalf("a resolved runtime did not refuse its own comment by identity: %+v", decision)
		}
	}

	// ...and a human is still admitted, so this is fail-closed rather than
	// refuse-everything.
	human := FeedbackItem{
		Class: FeedbackReview, ID: 2,
		Actor: GitHubActor{Login: "operator", ID: 7}, Body: "please change this", Commit: "head-sha",
	}
	permissions["operator"] = PermissionWrite
	decisions := AdmitFeedback([]FeedbackItem{human}, resolved, permissions, "head-sha")
	if len(decisions) != 1 || !decisions[0].Admitted {
		t.Fatalf("a permitted human review was not admitted: %+v", decisions)
	}
}

// TestAdmissionIsNotAdvertisedWithoutAViewer. An adapter that cannot report who
// the runtime publishes as must not claim it can run the admission gate: the
// gate would then be asked to recognize an identity nothing can supply.
func TestAdmissionIsNotAdvertisedWithoutAViewer(t *testing.T) {
	full := NewMultiplexedForge(NewFakeGitHubAdapter(), newSteppingClock())
	if !full.SupportsFeedbackAdmission() {
		t.Fatal("a complete adapter stopped advertising feedback admission")
	}
	fake := NewFakeGitHubAdapter()
	blind := NewMultiplexedForge(viewerlessForge{GitHubAdapter: fake, permissions: fake, conversation: fake}, newSteppingClock())
	if _, isViewer := interface{}(viewerlessForge{}).(ForgeViewer); isViewer {
		t.Fatal("the fixture still satisfies ForgeViewer, so this test proves nothing")
	}
	if blind.SupportsFeedbackAdmission() {
		t.Fatal("an adapter that cannot resolve its own viewer advertised feedback admission")
	}
}

// viewerlessForge can answer permissions and conversations but genuinely lacks
// the viewer capability. It embeds the ADAPTER INTERFACE rather than the fake
// struct: embedding the struct would promote Viewer and the capability
// assertion would still succeed, so the test would be asserting nothing.
type viewerlessForge struct {
	GitHubAdapter
	permissions  ForgeActorPermissions
	conversation ForgeConversation
}

func (f viewerlessForge) RepositoryPermission(ctx context.Context, repo GitHubRepo, login string) (GitHubPermission, error) {
	return f.permissions.RepositoryPermission(ctx, repo, login)
}

func (f viewerlessForge) PullRequestComments(ctx context.Context, repo GitHubRepo, number int) ([]GitHubComment, error) {
	return f.conversation.PullRequestComments(ctx, repo, number)
}

func (f viewerlessForge) IssueComments(ctx context.Context, repo GitHubRepo, number int) ([]GitHubComment, error) {
	return f.conversation.IssueComments(ctx, repo, number)
}

// rotatingViewer is a forge whose publishing account changes underneath a live
// runtime, exactly as rotating the token file does: the credential is re-read
// per request, so the account the runtime acts as can change without a restart.
type rotatingViewer struct {
	*FakeGitHubAdapter
	actor   func() (GitHubActor, error)
	viewers int
}

func (f *rotatingViewer) Viewer(context.Context, GitHubRepo) (GitHubActor, error) {
	f.viewers++
	return f.actor()
}

// TestARotatedPublicationCredentialCannotFeedTheRuntimeItsOwnComments is the
// lifetime law: the publication credential and the identity the self-loop guard
// refuses must move together.
//
// Binding the identity once at construction while the token is re-read per
// request means a rotation leaves the guard recognizing the account the runtime
// USED to be. The account it has become is a dedicated, non-bot, write-holding
// publisher, so it passes every remaining check and its own comments are
// admitted - recreating the loop.
func TestARotatedPublicationCredentialCannotFeedTheRuntimeItsOwnComments(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	current := GitHubActor{Login: "zenchron-runtime-a", ID: 111}
	forge := &rotatingViewer{FakeGitHubAdapter: fixture.forge, actor: func() (GitHubActor, error) { return current, nil }}
	deps := fixture.deps
	deps.GitHub = forge
	deps.Feedback = FeedbackPolicy{MinPermission: PermissionWrite}
	engine, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}

	// First observation binds actor A durably.
	if _, err := engine.ObserveFeedback(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if bound := state.feedbackState().PublicationLogin; bound != "zenchron-runtime-a" {
		t.Fatalf("the first observation did not bind the publication identity: %q", bound)
	}

	// The operator rotates the token. No restart: the same engine, the same
	// cached everything, a different account.
	current = GitHubActor{Login: "zenchron-runtime-b", ID: 222}
	observation, err := engine.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Unavailable == "" {
		t.Fatal("a rotated publication credential was accepted silently; the guard is still bound to the previous account")
	}
	for _, phrase := range []string{"zenchron-runtime-b", "zenchron-runtime-a"} {
		if !strings.Contains(observation.Unavailable, phrase) {
			t.Fatalf("the refusal does not name both identities: %s", observation.Unavailable)
		}
	}
	if observation.Admitted != 0 {
		t.Fatalf("%d items were admitted while the publication identity was in doubt", observation.Admitted)
	}
}

// TestATransientIdentityFailureRecoversWithoutARestart. Failing closed is only
// safe if it is temporary: an engine that bound "unresolved" for its lifetime
// would disable feedback until the operator noticed and restarted.
func TestATransientIdentityFailureRecoversWithoutARestart(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	failing := true
	forge := &rotatingViewer{FakeGitHubAdapter: fixture.forge, actor: func() (GitHubActor, error) {
		if failing {
			return GitHubActor{}, &GitHubTransientError{Status: 503, Detail: "the forge is briefly unavailable"}
		}
		return GitHubActor{Login: "zenchron-runtime", ID: 7}, nil
	}}
	deps := fixture.deps
	deps.GitHub = forge
	deps.Feedback = FeedbackPolicy{MinPermission: PermissionWrite}
	engine, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}

	observation, err := engine.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Unavailable == "" {
		t.Fatal("an unresolvable publication identity did not fail closed")
	}

	// The forge recovers. The SAME engine must pick it up.
	failing = false
	recovered, err := engine.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Unavailable != "" {
		t.Fatalf("feedback stayed disabled after the forge recovered, so it needs a restart: %s", recovered.Unavailable)
	}
	if forge.viewers < 2 {
		t.Fatalf("the identity was resolved %d times; it must be resolved per observation, not bound once", forge.viewers)
	}
}

// TestTheCredentialIdentityIsNeverServedFromCache goes through the PRODUCTION
// MultiplexedForge, because that is where the staleness lived.
//
// Every other read the multiplexer coalesces answers a question about the
// repository, and answering it once per window is the whole point. Viewer
// answers a question about the CREDENTIAL, which is re-read per request and can
// change between two calls a cache would collapse into one. A stale answer means
// the self-loop guard compares against an account the runtime no longer is:
// rotate the token, publish as the new account, and a sibling run observing
// inside the window resolves the OLD identity, does not recognize the comment as
// its own, and admits it.
func TestTheCredentialIdentityIsNeverServedFromCache(t *testing.T) {
	inner := &rotatingViewer{FakeGitHubAdapter: NewFakeGitHubAdapter()}
	current := GitHubActor{Login: "zenchron-runtime-a", ID: 111}
	inner.actor = func() (GitHubActor, error) { return current, nil }

	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour // a window long enough that a cache would certainly hide the rotation
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	first, err := forge.Viewer(context.Background(), repo)
	if err != nil || first.Login != "zenchron-runtime-a" {
		t.Fatalf("first resolution = %+v %v", first, err)
	}

	// The token rotates. No write happens in between, because the dangerous
	// sequence does not require one to have completed.
	current = GitHubActor{Login: "zenchron-runtime-b", ID: 222}

	second, err := forge.Viewer(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if second.Login != "zenchron-runtime-b" {
		t.Fatalf("a rotated credential still resolved as %q from the shared cache; the guard would compare against an account the runtime is no longer using", second.Login)
	}
	if inner.viewers != 2 {
		t.Fatalf("the inner adapter was asked %d times; the identity must not be coalesced", inner.viewers)
	}

	// Repository state IS still shared - this fix must not have turned the
	// multiplexer into a pass-through.
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "head", State: GitHubOpen}
	if _, err := forge.PullRequest(context.Background(), repo, 1); err != nil {
		t.Fatal(err)
	}
	before := len(inner.Calls)
	if _, err := forge.PullRequest(context.Background(), repo, 1); err != nil {
		t.Fatal(err)
	}
	if len(inner.Calls) != before {
		t.Fatalf("repository reads stopped being shared: %d new calls", len(inner.Calls)-before)
	}
}

// TestARateLimitedIdentityLookupIsSharedAcrossRuns. Refusing to cache the
// credential identity must not also discard the repository's shared rate-limit
// law: with N active runs each observing feedback, all N would otherwise
// rediscover the same limit and keep asking.
//
// The answer is never shared; the forge's instruction to stop asking is.
func TestARateLimitedIdentityLookupIsSharedAcrossRuns(t *testing.T) {
	inner := &rotatingViewer{FakeGitHubAdapter: NewFakeGitHubAdapter()}
	inner.actor = func() (GitHubActor, error) {
		return GitHubActor{}, &GitHubTransientError{
			Status: 429, Detail: "rate limited",
			RateLimit: RateLimitObservation{RetryAfter: time.Minute},
		}
	}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Nanosecond // defeat coalescing, so only backoff can stop a call
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	if _, err := forge.Viewer(context.Background(), repo); err == nil {
		t.Fatal("the rate limit was not surfaced")
	}
	asked := inner.viewers

	// A sibling run observing feedback must be refused from the shared backoff
	// without reaching the forge.
	if _, err := forge.Viewer(context.Background(), repo); err == nil {
		t.Fatal("a sibling was allowed to ask again during the shared backoff")
	}
	if inner.viewers != asked {
		t.Fatalf("the identity lookup ignored shared backoff: %d extra calls reached the forge", inner.viewers-asked)
	}

	// A DIFFERENT question about the same repository is refused too: the
	// backoff belongs to the repository, not to this one read.
	if _, err := forge.Checks(context.Background(), repo, "head"); err == nil {
		t.Fatal("the identity lookup did not record repository-wide backoff")
	}
}

// TestAPermanentIdentityFailureIsReportedNotRetriedForever. A rejected or
// expired publication credential is not a temporary outage, and reporting it as
// one gave it the same shape: a successful ObserveFeedback with Unavailable set
// never reaches RunOutcome.FeedbackError, so the runtime retried every tick and
// the operator saw nothing to act on.
func TestAPermanentIdentityFailureIsReportedNotRetriedForever(t *testing.T) {
	for name, failure := range map[string]error{
		"rejected credential": &GitHubAuthError{Detail: "credential rejected"},
		"permanent api error": &GitHubAPIError{Status: 500, Detail: "server error"},
		"unrecognized":        errors.New("something nobody classified"),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPhase8Fixture(t)
			runID := fixture.start()
			forge := &rotatingViewer{FakeGitHubAdapter: fixture.forge}
			forge.actor = func() (GitHubActor, error) { return GitHubActor{}, failure }
			deps := fixture.deps
			deps.GitHub = forge
			deps.Feedback = FeedbackPolicy{MinPermission: PermissionWrite}
			engine, err := NewEngineeringRuntime(deps)
			if err != nil {
				t.Fatal(err)
			}
			observation, err := engine.ObserveFeedback(context.Background(), runID)
			if err == nil {
				t.Fatalf("a permanent identity failure was reported as temporary unavailability: %q", observation.Unavailable)
			}
			// Nothing may be bound on a failure path: a run must not record an
			// identity it never actually resolved.
			state, loadErr := engine.load(runID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if bound := state.feedbackState().PublicationLogin; bound != "" {
				t.Fatalf("a failed resolution bound the identity %q", bound)
			}
		})
	}
}

// TestATransientIdentityFailureStaysUnavailableRatherThanFailingTheRun keeps the
// other half honest: a brief forge outage must not be reported as a run problem.
func TestATransientIdentityFailureStaysUnavailableRatherThanFailingTheRun(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	forge := &rotatingViewer{FakeGitHubAdapter: fixture.forge}
	forge.actor = func() (GitHubActor, error) {
		return GitHubActor{}, &GitHubTransientError{Status: 503, Detail: "briefly unavailable"}
	}
	deps := fixture.deps
	deps.GitHub = forge
	deps.Feedback = FeedbackPolicy{MinPermission: PermissionWrite}
	engine, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := engine.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatalf("a transient forge outage was reported as a run failure: %v", err)
	}
	if observation.Unavailable == "" {
		t.Fatal("a transient identity failure did not fail closed")
	}
}

// ---------------------------------------------------------------------------
// The GitHub App publication identity (#82)
// ---------------------------------------------------------------------------
//
// credential_mode "token" gave the runtime a separate CREDENTIAL, and the
// documentation said a GitHub App installation token was one of the things that
// could go in that file. It was not: an App issues no token that can live in a
// file, and the installation token it does issue cannot answer GET /user - so
// the advertised App path resolved no publication identity at all, and feedback
// admission, which fails closed on an unresolved identity, was permanently
// dead. These tests pin the path that actually works.

// appKeyFile writes an owner-only PKCS#1 App key and returns its path.
func appKeyFile(t *testing.T, mode os.FileMode) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, encoded, mode); err != nil {
		t.Fatal(err)
	}
	return path, key
}

// appDoer answers the two endpoints an App credential talks to and records what
// it was asked, so a test can assert on the calls that were NOT made.
type appDoer struct {
	paths  []string
	mints  int
	slug   string
	expiry time.Time
	// assertions is every Authorization bearer value the credential presented.
	assertions []string
}

func (d *appDoer) Do(r *http.Request) (*http.Response, error) {
	d.paths = append(d.paths, r.URL.Path)
	d.assertions = append(d.assertions, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	body, status := "{}", http.StatusOK
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		d.mints++
		status = http.StatusCreated
		body = fmt.Sprintf(`{"token":"ghs_minted_%d","expires_at":%q}`, d.mints, d.expiry.UTC().Format(time.RFC3339))
	case r.URL.Path == "/app":
		body = fmt.Sprintf(`{"id":4242,"slug":%q}`, d.slug)
	default:
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// TestAGitHubAppMintsCachesAndRemintsBeforeExpiry. An installation token lives
// for an hour and nothing else re-mints it, so a `serve` that minted once would
// publish fine and then fail closed an hour in - which is the same permanently
// dead feedback loop by a slower route. The margin is what makes it safe: a
// token used to its last second expires between the decision to publish and
// GitHub reading the header.
func TestAGitHubAppMintsCachesAndRemintsBeforeExpiry(t *testing.T) {
	path, key := appKeyFile(t, 0o600)
	now := time.Unix(1_700_000_000, 0).UTC()
	doer := &appDoer{slug: "zenchron-engineering", expiry: now.Add(time.Hour)}
	credential := &GitHubAppCredential{
		AppID: 11, InstallationID: 22, PrivateKeyPath: path,
		HTTP: doer, Now: func() time.Time { return now },
	}
	remote := governedRemoteIdentity(t)

	user, secret, err := credential.Credential(remote)
	if err != nil {
		t.Fatalf("a valid App credential was refused: %v", err)
	}
	if user != gitHubCredentialUser || secret != "ghs_minted_1" {
		t.Fatalf("credential = %q/%q", user, secret)
	}

	// The assertion is a real RS256 JWT over the App id, verified with the
	// public half of the key the operator provisioned. A malformed or unsigned
	// assertion would be refused by GitHub and by nothing here.
	assertAppAssertion(t, doer.assertions[0], key, 11)

	// Still inside the token's life: cached, no second mint.
	now = now.Add(50 * time.Minute)
	if _, secret, err = credential.Credential(remote); err != nil || secret != "ghs_minted_1" {
		t.Fatalf("a live token was not cached: %q %v", secret, err)
	}
	if doer.mints != 1 {
		t.Fatalf("the credential minted %d times while holding a live token", doer.mints)
	}

	// Inside the safety margin: re-minted BEFORE expiry, not after it.
	now = now.Add(7 * time.Minute) // 57 minutes in, 3 minutes of life left
	doer.expiry = now.Add(time.Hour)
	if _, secret, err = credential.Credential(remote); err != nil || secret != "ghs_minted_2" {
		t.Fatalf("the credential did not re-mint inside the safety margin: %q %v", secret, err)
	}
	if doer.mints != 2 {
		t.Fatalf("mints = %d", doer.mints)
	}
}

// assertAppAssertion verifies the JWT the credential presented: the signature
// is the thing GitHub checks, so a test that only looked at the shape would
// pass on a credential nothing can authenticate with.
func assertAppAssertion(t *testing.T, assertion string, key *rsa.PrivateKey, appID int64) {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("the App assertion is not a JWT: %d segments", len(parts))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the signature is not base64url: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the App assertion is not signed by the configured key: %v", err)
	}
	var claims struct {
		Issuer  int64 `json:"iss"`
		Issued  int64 `json:"iat"`
		Expires int64 `json:"exp"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != appID {
		t.Fatalf("the assertion claims App %d", claims.Issuer)
	}
	if claims.Expires-claims.Issued > 600 {
		t.Fatalf("the assertion lives %ds; GitHub refuses anything over ten minutes", claims.Expires-claims.Issued)
	}
}

// TestAGitHubAppRefusesANonHTTPSEndpoint pins #223 on the App path:
// github.endpoint also feeds this credential, and the JWT assertion signed
// from the operator's private key must never be placed in a request to a
// refused endpoint.
func TestAGitHubAppRefusesANonHTTPSEndpoint(t *testing.T) {
	path, _ := appKeyFile(t, 0o600)
	for name, endpoint := range map[string]string{
		"plaintext http":   "http://api.example.com",
		"a non-web scheme": "ftp://api.example.com",
		"no scheme at all": "api.example.com",
		"a bare path":      "/api/v3",
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			doer := &appDoer{slug: "zenchron-engineering", expiry: time.Now().Add(time.Hour)}
			credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: path, HTTP: doer, Endpoint: endpoint}
			_, _, err := credential.Credential(governedRemoteIdentity(t))
			var authErr *GitHubAuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("expected a typed refusal, got %v", err)
			}
			if len(doer.paths) != 0 {
				t.Fatalf("a refused endpoint was still contacted: %v", doer.paths)
			}
		})
	}
}

// TestAGitHubAppPrivateKeyIsOwnerOnly. The key is stronger than the token file
// it replaces: a token another local account can read is one publication
// identity, and a KEY another local account can read is the App itself, on
// every repository it is installed on, until the key is revoked.
func TestAGitHubAppPrivateKeyIsOwnerOnly(t *testing.T) {
	readable, _ := appKeyFile(t, 0o644)
	credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: readable, HTTP: &appDoer{slug: "x"}}
	_, _, err := credential.Credential(governedRemoteIdentity(t))
	if err == nil {
		t.Fatal("a world-readable App private key was accepted")
	}
	if !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("the refusal does not name the problem: %v", err)
	}

	// Nor is a file that is not a key: an unreadable PEM must be an operator
	// problem here, not an opaque 401 from GitHub later.
	notAKey := publicationTokenFile(t, "ghs_this_is_a_token_not_a_key", 0o600)
	credential.PrivateKeyPath = notAKey
	if _, _, err := credential.Credential(governedRemoteIdentity(t)); err == nil {
		t.Fatal("a file that is not a private key was accepted as one")
	}
}

// TestAGitHubAppResolvesItsIdentityWithoutAskingWhoTheUserIs is the defect.
//
// GET /user is refused to an installation token with 403 "Resource not
// accessible by integration". That 403 carries no rate-limit headers, so it
// classifies as a permanently rejected credential, and publicationIdentity turns
// a permanent failure into a hard run error - which is why configuring the
// advertised App path made feedback admission permanently dead rather than
// merely degraded.
func TestAGitHubAppResolvesItsIdentityWithoutAskingWhoTheUserIs(t *testing.T) {
	path, _ := appKeyFile(t, 0o600)
	doer := &appDoer{slug: "zenchron-engineering", expiry: time.Now().Add(time.Hour)}
	credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: path, HTTP: doer}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: credential}

	actor, err := adapter.Viewer(context.Background(), GitHubRepo{Owner: "acme", Name: "repo"})
	if err != nil {
		t.Fatalf("an App credential could not resolve its publication identity: %v", err)
	}
	// The login GitHub actually stamps on the App's comments.
	if actor.Login != "zenchron-engineering[bot]" {
		t.Fatalf("publication identity = %q", actor.Login)
	}
	if !actor.Bot {
		t.Fatal("the App's own identity is not reported as automation")
	}
	for _, called := range doer.paths {
		if called == "/user" {
			t.Fatal("the adapter asked GET /user with an installation credential; GitHub refuses that permanently and admission fails closed forever")
		}
	}
	if len(doer.paths) == 0 || doer.paths[len(doer.paths)-1] != "/app" {
		t.Fatalf("the identity was not resolved from the App endpoint: %v", doer.paths)
	}
}

// appPublishedForge is the production admission path with ONE thing replaced:
// the publication identity comes from a real GitHubAppCredential over a fake
// transport, instead of from a field a test set. Everything else - observation,
// admission, the durable binding - is the real code.
type appPublishedForge struct {
	*FakeGitHubAdapter
	viewer ForgeViewer
}

func (f appPublishedForge) Viewer(ctx context.Context, repo GitHubRepo) (GitHubActor, error) {
	return f.viewer.Viewer(ctx, repo)
}

// TestAnAppPublishedRuntimeRefusesItselfAndAdmitsTheOperator is #82's product
// promise, end to end:
//
//	zenchron-engineering[bot]  -> runtime publication identity, refused as self
//	bogdaniel                  -> human review identity, admitted normally
//
// on ONE human GitHub account, with the self-loop guard unchanged.
func TestAnAppPublishedRuntimeRefusesItselfAndAdmitsTheOperator(t *testing.T) {
	path, _ := appKeyFile(t, 0o600)
	doer := &appDoer{slug: "zenchron-engineering", expiry: time.Now().Add(time.Hour)}
	credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: path, HTTP: doer}

	fixture := newPhase8Fixture(t)
	fixture.deps.GitHub = appPublishedForge{
		FakeGitHubAdapter: fixture.forge,
		viewer:            GitHubRESTAdapter{HTTP: doer, Credentials: credential},
	}
	fixture.deps.Feedback = FeedbackPolicy{MinPermission: PermissionWrite}
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.forge.Permissions["bogdaniel"] = PermissionWrite
	// The App holds write on the repository it publishes to, which is exactly
	// why permission cannot be what stops it.
	fixture.forge.Permissions["zenchron-engineering[bot]"] = PermissionWrite

	runID := fixture.start()
	if outcome := fixture.reconcile(runID); outcome.Disposition == Failed {
		t.Fatalf("run failed before publication: %#v", outcome)
	}
	number := fixture.state(runID).projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{
		{
			ID: 601, Author: GitHubActor{Login: "zenchron-engineering[bot]", ID: 9001, Bot: true},
			Body:      UntrustedText("Zenchron: candidate published by agent codex (operator_trusted)."),
			CreatedAt: fixture.clock.Now(),
		},
		{
			ID: 602, Author: GitHubActor{Login: "bogdaniel", ID: 7},
			Body:      UntrustedText("please add a doc comment to the new helper"),
			CreatedAt: fixture.clock.Now(),
		},
	}

	observation, err := fixture.runtime.ObserveFeedback(context.Background(), runID)
	if err != nil {
		t.Fatalf("ObserveFeedback: %v", err)
	}
	if observation.Unavailable != "" {
		t.Fatalf("an App publication identity did not resolve, so admission failed closed: %s", observation.Unavailable)
	}
	reasons := map[string]string{}
	admitted := map[string]bool{}
	for _, decision := range observation.Decisions {
		reasons[decision.Actor] = decision.Reason
		admitted[decision.Actor] = decision.Admitted
	}
	if admitted["zenchron-engineering[bot]"] {
		t.Fatalf("the runtime admitted its own comment: %q", reasons["zenchron-engineering[bot]"])
	}
	// Refused as SELF, not merely as a bot: the App is this system, and the
	// distinction is what the durable provenance records.
	if reasons["zenchron-engineering[bot]"] != feedbackRefusedSelf {
		t.Fatalf("the App's own comment was refused for the wrong reason: %q", reasons["zenchron-engineering[bot]"])
	}
	if !admitted["bogdaniel"] {
		t.Fatalf("the operator's own review was not admitted: %q", reasons["bogdaniel"])
	}
	// The identity the guard used is durable provenance, resolved from the App
	// rather than configured.
	if bound := fixture.state(runID).feedbackState().PublicationLogin; bound != "zenchron-engineering[bot]" {
		t.Fatalf("the publication identity bound to this run is %q", bound)
	}
}

// TestGitHubAppConfigurationIsCompleteOrRefused. A half-configured App mode
// would fail at the first publish with an opaque authentication error; every
// member it needs is checked where the operator can still fix it.
func TestGitHubAppConfigurationIsCompleteOrRefused(t *testing.T) {
	complete := GitHubConfig{
		CredentialMode: GitHubCredentialApp,
		AppID:          11, InstallationID: 22, PrivateKeyPath: "/etc/zenchron/app.pem",
	}
	if err := validGitHubConfig(complete); err != nil {
		t.Fatalf("a complete github-app configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*GitHubConfig){
		"no app id":          func(c *GitHubConfig) { c.AppID = 0 },
		"no installation id": func(c *GitHubConfig) { c.InstallationID = 0 },
		"no private key":     func(c *GitHubConfig) { c.PrivateKeyPath = "" },
		"relative key path":  func(c *GitHubConfig) { c.PrivateKeyPath = "app.pem" },
		"token path too":     func(c *GitHubConfig) { c.TokenPath = "/etc/zenchron/publication.token" },
	} {
		t.Run(name, func(t *testing.T) {
			config := complete
			mutate(&config)
			if err := validGitHubConfig(config); err == nil {
				t.Fatal("an incomplete github-app configuration was accepted")
			}
		})
	}
	// The App members are operator-visible configuration, not decoration: a
	// mode that ignores them must refuse them, exactly as it refuses token_path.
	for _, mode := range []string{GitHubCredentialCLI, GitHubCredentialToken, GitHubCredentialNone} {
		config := GitHubConfig{CredentialMode: mode, AppID: 11}
		if mode == GitHubCredentialToken {
			config.TokenPath = "/etc/zenchron/publication.token"
		}
		if err := validGitHubConfig(config); err == nil {
			t.Fatalf("credential_mode %q silently ignored github.app_id", mode)
		}
	}
}

// validGitHubConfig runs the real operator-configuration validation with the
// github block under test and everything else minimally valid, so these cases
// exercise the shipped rule rather than a copy of it.
func validGitHubConfig(github GitHubConfig) error {
	config := OperatorConfig{
		StateDir:         "/var/lib/zenchron",
		ProjectModelPath: "/etc/zenchron/model.json",
		PolicyPath:       "/etc/zenchron/policy.json",
		Assurance:        AssuranceConfig{Image: "sha256:" + strings.Repeat("a", 64)},
		Provider:         ProviderConfig{Kind: ProviderOpenAI, Model: "gpt-5", CredentialPath: "/etc/zenchron/openai"},
		Budgets: BudgetConfig{
			WallLimitSeconds: 600, MaxExecutionAttempts: 1,
			MaxRemediationAttempts: 1, MaxAssuranceAttempts: 1,
		},
		GitHub: github,
	}
	return config.validate("/etc/zenchron/config.json")
}

// TestDoctorComparesThePublicationAndHumanIdentities is #82's stated operator
// UX. Naming only the publishing account - which is what this check used to do -
// left the comparison to an operator with no way to know it mattered, and a
// separate credential is not a separate account.
func TestDoctorComparesThePublicationAndHumanIdentities(t *testing.T) {
	newInput := func(publication, operator string, permission GitHubPermission) DoctorInput {
		forge := NewFakeGitHubAdapter()
		forge.ViewerActor = GitHubActor{Login: publication, ID: 4242, Bot: true}
		forge.Permissions[strings.ToLower(operator)] = permission
		human := NewFakeGitHubAdapter()
		human.ViewerActor = GitHubActor{Login: operator, ID: 7}
		return DoctorInput{
			GitHubCredentialMode: GitHubCredentialApp,
			GitHub:               forge,
			OperatorGitHub:       human,
			Repository:           RepositoryTarget{Identity: "acme/repo"},
		}
	}
	ctx := context.Background()

	separate := doctorPublicationIdentity(ctx, newInput("zenchron-engineering[bot]", "bogdaniel", PermissionAdmin))
	if separate.Status != DoctorPass {
		t.Fatalf("a working #82 configuration reported %s: %s", separate.Status, separate.Reason)
	}
	for _, phrase := range []string{
		`publication identity "zenchron-engineering[bot]"`,
		`human feedback actor "bogdaniel" (permission: admin)`,
		"self-loop guard active",
	} {
		if !strings.Contains(separate.Reason, phrase) {
			t.Fatalf("doctor does not state %q: %s", phrase, separate.Reason)
		}
	}

	// One account wearing both hats is the collision this whole issue exists
	// for, and a separate credential does not fix it.
	collided := doctorPublicationIdentity(ctx, newInput("bogdaniel", "bogdaniel", PermissionAdmin))
	if collided.Status != DoctorWarn {
		t.Fatalf("a self-publishing configuration reported %s: %s", collided.Status, collided.Reason)
	}
	if !strings.Contains(collided.Reason, "SAME account") {
		t.Fatalf("doctor does not name which half failed: %s", collided.Reason)
	}

	// Distinct identities are only half the requirement: a human below the
	// admission threshold still cannot direct a worker.
	below := doctorPublicationIdentity(ctx, newInput("zenchron-engineering[bot]", "bogdaniel", PermissionRead))
	if below.Status != DoctorWarn {
		t.Fatalf("a human below the feedback threshold reported %s: %s", below.Status, below.Reason)
	}
	if !strings.Contains(below.Reason, "feedback admission") || !strings.Contains(below.Reason, "distinct") {
		t.Fatalf("doctor does not name which half failed: %s", below.Reason)
	}

	// And the comparison is never invented: an operator identity that cannot be
	// resolved is reported as unresolved, not assumed to be different.
	blind := newInput("zenchron-engineering[bot]", "bogdaniel", PermissionAdmin)
	blind.OperatorGitHub = nil
	unknown := doctorPublicationIdentity(ctx, blind)
	if unknown.Status != DoctorWarn || !strings.Contains(unknown.Reason, "UNRESOLVED") {
		t.Fatalf("doctor claimed a comparison it could not make: %s %s", unknown.Status, unknown.Reason)
	}
}

// mintFailureDoer fails the token exchange the two ways a real one fails
// transiently: the forge is unreachable, and the forge says "not now".
type mintFailureDoer struct {
	err    error
	status int
	header http.Header
}

func (d *mintFailureDoer) Do(*http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	header := d.header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader("{}")), Header: header}, nil
}

// TestATransientMintFailureIsNotACredentialRejection.
//
// GitHubAppCredential is the first credential provider that performs network
// I/O, so it is the first that can fail for a reason that clears on its own.
// The adapter's token() collapsed every non-GitHubAuthError into "credential
// resolution failed", which was harmless while every provider failed only for
// local, permanent reasons and is not harmless now: watch maps the auth class
// to WatchErrorAuth and parks every run in that repository as waiting on
// GitHub authentication, and feedback observation takes the hard-error branch
// instead of deferring. A network blip then reads as a credential the operator
// must go and fix.
//
// Viewer does not catch this, because an App credential answers it through
// AppIdentity and never reaches token().
func TestATransientMintFailureIsNotACredentialRejection(t *testing.T) {
	path, _ := appKeyFile(t, 0o600)
	for name, doer := range map[string]*mintFailureDoer{
		"the forge is unreachable": {err: errors.New("dial tcp 140.82.121.6:443: connect: connection refused")},
		"the forge says not now": {
			status: http.StatusForbidden,
			header: http.Header{"Retry-After": []string{"60"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: path, HTTP: doer}
			if _, _, err := credential.Credential(governedRemoteIdentity(t)); !transientForgeFailure(err) {
				t.Fatalf("the credential itself did not classify the failure as transient: %#v", err)
			}
			// Through the adapter, which is where the classification was lost.
			adapter := GitHubRESTAdapter{HTTP: doer, Credentials: credential}
			_, err := adapter.RepositoryPermission(context.Background(), GitHubRepo{Owner: "acme", Name: "repo"}, "bogdaniel")
			if err == nil {
				t.Fatal("a failed mint produced no error")
			}
			if !transientForgeFailure(err) {
				t.Fatalf("a transient mint failure reached the caller as a permanent one, so watch parks every run on GitHub auth: %#v", err)
			}
			var auth *GitHubAuthError
			if errors.As(err, &auth) {
				t.Fatalf("a transient mint failure was relabelled as a rejected credential: %v", auth)
			}
		})
	}
}
