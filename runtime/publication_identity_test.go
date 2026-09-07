package runtime

// The runtime refuses feedback it authored itself, by identity. When it
// publishes with the operator's own credential it IS the operator on GitHub, so
// that guard refuses the operator's reviews too and the #63 review loop cannot
// happen. These tests pin the separation that fixes it without weakening the
// guard.

import (
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

	separate := doctorPublicationIdentity(DoctorInput{GitHubCredentialMode: GitHubCredentialToken})
	if separate.Status != DoctorPass {
		t.Fatalf("a separate publication identity reported %s: %s", separate.Status, separate.Reason)
	}
	if !strings.Contains(separate.Reason, "DIFFERENT GitHub actor") {
		t.Fatalf("the pass does not state what was proven: %s", separate.Reason)
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
