package runtime

// The runtime refuses feedback it authored itself, by identity. When it
// publishes with the operator's own credential it IS the operator on GitHub, so
// that guard refuses the operator's reviews too and the #63 review loop cannot
// happen. These tests pin the separation that fixes it without weakening the
// guard.

import (
	"context"
	"errors"
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
	shared := doctorPublicationIdentity(DoctorInput{GitHubCredentialMode: GitHubCredentialCLI})
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
	unresolvable := doctorPublicationIdentity(DoctorInput{GitHubCredentialMode: GitHubCredentialToken})
	if unresolvable.Status != DoctorWarn {
		t.Fatalf("an unresolvable publication account reported %s: %s", unresolvable.Status, unresolvable.Reason)
	}
	if strings.Contains(unresolvable.Reason, "DIFFERENT") {
		t.Fatalf("doctor claimed distinctness it did not resolve: %s", unresolvable.Reason)
	}

	forge := NewFakeGitHubAdapter()
	forge.ViewerActor = GitHubActor{Login: "zenchron-runtime", ID: 4242}
	resolved := doctorPublicationIdentity(DoctorInput{
		GitHubCredentialMode: GitHubCredentialToken,
		GitHub:               forge,
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
