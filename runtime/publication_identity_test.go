package runtime

// The runtime refuses feedback it authored itself, by identity. When it
// publishes with the operator's own credential it IS the operator on GitHub, so
// that guard refuses the operator's reviews too and the #63 review loop cannot
// happen. These tests pin the separation that fixes it without weakening the
// guard.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
