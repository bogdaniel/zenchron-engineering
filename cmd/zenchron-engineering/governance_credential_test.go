package main

// The composition root is where a role confusion would actually happen: two
// credentials are built here from one configuration, and picking the wrong one
// is a wiring mistake rather than a logic one. These tests assert the two
// selectors stay independent, and that the governance selector fails closed.

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestGovernanceCredentialFailsClosedWhenUnauthorized(t *testing.T) {
	// An App-mode configuration with no governance member is exactly the
	// operator configuration #219 was filed against. It must refuse, and the
	// refusal must say what to add - it must NOT quietly reuse the App.
	_, err := githubGovernanceCredential(runtime.GitHubConfig{
		CredentialMode: runtime.GitHubCredentialApp,
		AppID:          4952506,
		InstallationID: 161898047,
	})
	if err == nil {
		t.Fatal("an unconfigured governance mode produced a credential; the publication credential cannot see bypass_actors and must never stand in")
	}
	if !strings.Contains(err.Error(), "governance_credential_mode") {
		t.Fatalf("the refusal does not tell the operator what to authorize: %v", err)
	}
}

func TestGovernanceCredentialIsNotThePublicationCredential(t *testing.T) {
	config := runtime.GitHubConfig{
		CredentialMode:           runtime.GitHubCredentialApp,
		AppID:                    4952506,
		InstallationID:           161898047,
		PrivateKeyPath:           "/dev/null",
		GovernanceCredentialMode: runtime.GitHubCredentialCLI,
	}
	governance, err := githubGovernanceCredential(config)
	if err != nil {
		t.Fatal(err)
	}
	if role := governance.Provenance().Role; role != runtime.CredentialRoleGovernance {
		t.Fatalf("the governance credential reports role %q", role)
	}
	// It cannot be wired into anything that publishes. The assertion is the
	// runtime-visible form of what the compiler already refuses.
	if _, ok := any(governance).(runtime.CredentialProvider); ok {
		t.Fatal("the governance credential satisfies CredentialProvider and could be selected as the publication identity")
	}

	// And the publication selector is unmoved by the governance member: the
	// App stays the publication identity, which is what #82 and #156 require.
	publication := githubCredentials(config)
	if _, ok := publication.(*runtime.GitHubAppCredential); !ok {
		t.Fatalf("the publication credential is %T, not the GitHub App", publication)
	}
	if _, ok := any(publication).(runtime.GovernanceCredential); ok {
		t.Fatal("the publication credential satisfies GovernanceCredential and could be selected as the governance observer")
	}
}
